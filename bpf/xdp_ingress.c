// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
//
// Rivora XDP ingress: VIP match -> connection affinity / Maglev backend
// selection -> DSR (MAC rewrite, XDP_TX) or full-NAT (IP/port rewrite,
// XDP_PASS to normal routing) forwarding. IPv4 TCP/UDP only in v0.1.
//
// Scope limitations tracked for v0.2+: IPv6, IP options, encapsulated
// (Geneve/IPIP) backends, and a real Maglev "add backend" rebalance — v0.1
// serves a single VIP with one flat maglev_table.

#include <linux/if_ether.h>
#include <linux/ip.h>
#include <linux/tcp.h>
#include <linux/udp.h>
#include <linux/in.h>

#include "rivora_common.h"

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 256);
    __type(key, struct vip_key);
    __type(value, __u32); /* service_id */
} vip_map SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, 256);
    __type(key, __u32); /* service_id */
    __type(value, struct service_config);
} service_config_map SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, RIVORA_MAGLEV_M);
    __type(key, __u32); /* slot */
    __type(value, __u32); /* backend_id */
} maglev_table SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 256);
    __type(key, __u32); /* backend_id */
    __type(value, struct backend_info);
} backend_map SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, 256);
    __type(key, __u32); /* backend_id */
    __type(value, __u8); /* 1 = healthy, 0 = down */
} backend_health_map SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 65536);
    __type(key, struct conn_key);
    __type(value, __u32); /* backend_id */
} connection_affinity_map SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 65536);
    __type(key, struct nat_reverse_key);
    __type(value, struct nat_reverse_val);
} nat_reverse_map SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 16);
    __type(key, __u32); /* ifindex */
    __type(value, struct mac_addr); /* MAC of that interface, for DSR src rewrite */
} iface_mac_map SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, 256);
    __type(key, __u32); /* 0 = global, 1+backend_id = per backend */
    __type(value, struct lb_stats);
} stats_map SEC(".maps");

static __always_inline void bump_stats(__u32 idx, __u32 bytes, __u8 dropped)
{
    struct lb_stats *s = bpf_map_lookup_elem(&stats_map, &idx);
    if (!s)
        return;
    if (dropped) {
        s->dropped += 1;
    } else {
        s->packets += 1;
        s->bytes += bytes;
    }
}

/* Bounded probe for a healthy backend starting at the Maglev-selected slot.
 * Approximates Maglev's degraded lookup (which recomputes the whole
 * permutation on membership change) by walking forward a few slots — good
 * enough to route around a handful of simultaneously-down backends without
 * an unbounded loop in the verifier. */
static __always_inline int pick_backend(__u32 slot, __u32 *out_backend_id)
{
#pragma unroll
    for (int i = 0; i < RIVORA_MAGLEV_PROBES; i++) {
        __u32 s = (slot + i) % RIVORA_MAGLEV_M;
        __u32 *bid = bpf_map_lookup_elem(&maglev_table, &s);
        if (!bid)
            continue;
        __u8 *healthy = bpf_map_lookup_elem(&backend_health_map, bid);
        if (healthy && *healthy) {
            *out_backend_id = *bid;
            return 0;
        }
    }
    return -1;
}

SEC("xdp")
int rivora_xdp_ingress(struct xdp_md *ctx)
{
    void *data = (void *)(long)ctx->data;
    void *data_end = (void *)(long)ctx->data_end;

    struct ethhdr *eth = data;
    if ((void *)(eth + 1) > data_end)
        return XDP_PASS;
    if (eth->h_proto != __constant_htons(ETH_P_IP))
        return XDP_PASS;

    struct iphdr *iph = (void *)(eth + 1);
    if ((void *)(iph + 1) > data_end)
        return XDP_PASS;
    if (iph->ihl != 5) /* v0.1: no IP options */
        return XDP_PASS;
    if (iph->protocol != IPPROTO_TCP_ && iph->protocol != IPPROTO_UDP_)
        return XDP_PASS;

    __u16 sport, dport;
    void *l4 = (void *)iph + (iph->ihl * 4);

    if (iph->protocol == IPPROTO_TCP_) {
        struct tcphdr *tcph = l4;
        if ((void *)(tcph + 1) > data_end)
            return XDP_PASS;
        sport = tcph->source;
        dport = tcph->dest;
    } else {
        struct udphdr *udph = l4;
        if ((void *)(udph + 1) > data_end)
            return XDP_PASS;
        sport = udph->source;
        dport = udph->dest;
    }

    struct vip_key vk = {.addr = iph->daddr, .port = dport, .proto = iph->protocol};
    __u32 *service_id = bpf_map_lookup_elem(&vip_map, &vk);
    if (!service_id)
        return XDP_PASS; /* not a VIP we own */

    struct service_config *cfg = bpf_map_lookup_elem(&service_config_map, service_id);
    if (!cfg || cfg->backend_count == 0)
        return XDP_PASS;

    struct conn_key ck = {
        .saddr = iph->saddr, .daddr = iph->daddr,
        .sport = sport, .dport = dport, .proto = iph->protocol,
    };

    __u32 backend_id;
    __u32 *affinity = bpf_map_lookup_elem(&connection_affinity_map, &ck);
    __u8 use_affinity = 0;
    if (affinity) {
        __u8 *healthy = bpf_map_lookup_elem(&backend_health_map, affinity);
        if (healthy && *healthy) {
            backend_id = *affinity;
            use_affinity = 1;
        }
    }

    if (!use_affinity) {
        __u32 hash = rivora_hash5(iph->saddr, iph->daddr, sport, dport, iph->protocol);
        __u32 slot = (cfg->maglev_offset + (hash % RIVORA_MAGLEV_M)) % RIVORA_MAGLEV_M;
        if (pick_backend(slot, &backend_id) < 0) {
            bump_stats(RIVORA_STATS_GLOBAL, 0, 1);
            return XDP_DROP; /* no healthy backend within probe window */
        }
        bpf_map_update_elem(&connection_affinity_map, &ck, &backend_id, BPF_ANY);
    }

    struct backend_info *be = bpf_map_lookup_elem(&backend_map, &backend_id);
    if (!be)
        return XDP_PASS;

    __u32 pkt_len = (__u32)(data_end - data);
    bump_stats(RIVORA_STATS_GLOBAL, pkt_len, 0);
    bump_stats(1 + backend_id, pkt_len, 0);

    if (cfg->mode == RIVORA_MODE_DSR) {
        __u32 ifindex = ctx->ingress_ifindex;
        struct mac_addr *self_mac = bpf_map_lookup_elem(&iface_mac_map, &ifindex);
        if (self_mac)
            __builtin_memcpy(eth->h_source, self_mac->addr, 6);
        __builtin_memcpy(eth->h_dest, be->mac, 6);
        /* DSR: VIP stays the destination IP — the backend must have the VIP
         * bound locally (loopback/dummy) so it accepts and replies directly. */
        return XDP_TX;
    }

    /* Full NAT: rewrite destination IP/port to the backend; record the
     * reverse mapping so tc_nat's TCX egress program can un-NAT the
     * backend's reply back to VIP:port before it reaches the client.
     *
     * Re-derive the L4 header pointer fresh from iph right here, with its
     * own bounds check immediately adjacent to the write. Reusing tcph/udph
     * captured much earlier — before several intervening map lookups and
     * branches — doesn't reliably keep its verified-safe-pointer status
     * that far into the function, so each branch gets its own tight
     * check-then-use with nothing unrelated in between. */
    __u32 old_daddr = iph->daddr;
    __u32 new_daddr = be->addr;
    __u16 old_dport = dport;
    __u16 new_dport = be->port;
    void *l4b = (void *)iph + (iph->ihl * 4);

    if (iph->protocol == IPPROTO_TCP_) {
        struct tcphdr *t = l4b;
        if ((void *)(t + 1) > data_end)
            return XDP_PASS;
        csum_replace(&iph->check, &old_daddr, &new_daddr, 4);
        csum_replace(&t->check, &old_daddr, &new_daddr, 4);
        csum_replace(&t->check, &old_dport, &new_dport, 2);
        t->dest = new_dport;
    } else {
        struct udphdr *u = l4b;
        if ((void *)(u + 1) > data_end)
            return XDP_PASS;
        csum_replace(&iph->check, &old_daddr, &new_daddr, 4);
        if (u->check != 0) { /* UDP checksum 0 means "unset", leave it */
            csum_replace(&u->check, &old_daddr, &new_daddr, 4);
            csum_replace(&u->check, &old_dport, &new_dport, 2);
        }
        u->dest = new_dport;
    }
    iph->daddr = new_daddr;

    struct nat_reverse_key rk = {
        .backend_addr = be->addr, .client_addr = iph->saddr,
        .backend_port = be->port, .client_port = sport, .proto = iph->protocol,
    };
    struct nat_reverse_val rv = {.vip_addr = old_daddr, .vip_port = old_dport};
    bpf_map_update_elem(&nat_reverse_map, &rk, &rv, BPF_ANY);

    /* Let the kernel's normal routing deliver to the (now real) backend
     * address — simplest correct path for v0.1's full-NAT mode. */
    return XDP_PASS;
}

char _license[] SEC("license") = "GPL";
