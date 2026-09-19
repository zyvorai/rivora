// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
//
// Rivora TCX egress: reverses the full-NAT rewrite xdp_ingress applied on
// the way in. v0.1 runs client and backends on the same single interface
// (one-armed NAT), so this attaches to that same interface's egress path —
// it rewrites the backend's reply (src = backend:port) back to
// (src = VIP:port) right before the packet leaves, using the reverse
// mapping xdp_ingress wrote to nat_reverse_map (or nat_reverse_map6 for
// IPv6, v0.3). DSR-mode traffic never touches this program (the backend
// replies directly using the VIP as its own source address).

#include <linux/if_ether.h>
#include <linux/ip.h>
#include <linux/ipv6.h>
#include <linux/tcp.h>
#include <linux/udp.h>
#include <linux/in.h>
#include <linux/pkt_cls.h>

#include "rivora_common.h"

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 65536);
    __type(key, struct nat_reverse_key);
    __type(value, struct nat_reverse_val);
} nat_reverse_map SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 65536);
    __type(key, struct nat_reverse_key6);
    __type(value, struct nat_reverse_val6);
} nat_reverse_map6 SEC(".maps");

/* Reverse tracking of fragmented replies (see struct frag_rev_val in rivora_common.h). Same
 * name as in xdp_ingress.o, so the loader shares one map between the two objects. */
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 16384);
    __type(key, struct frag_key);
    __type(value, struct frag_rev_val);
} frag_rev_map SEC(".maps");

/* IPv6 sibling of frag_rev_map. */
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 16384);
    __type(key, struct frag_key6);
    __type(value, struct frag_rev_val6);
} frag_rev_map6 SEC(".maps");

/* A later fragment of a backend's reply has no L4 header, so it cannot be looked up by port.
 * It gets the source rewrite its datagram's first fragment got. Only the IP header changes:
 * the L4 checksum lives in the first fragment, which already accounted for the new source. */
static __always_inline int handle_v4_later_fragment(struct iphdr *iph)
{
    struct frag_key fk = {.saddr = iph->saddr, .daddr = iph->daddr, .id = iph->id, .proto = iph->protocol};
    struct frag_rev_val *fv = bpf_map_lookup_elem(&frag_rev_map, &fk);
    if (!fv)
        return TC_ACT_OK;
    __u32 old_saddr = iph->saddr;
    __u32 new_saddr = fv->vip_addr;
    csum_replace(&iph->check, &old_saddr, &new_saddr, 4);
    iph->saddr = new_saddr;
    return TC_ACT_OK;
}

static __always_inline int handle_ipv4(void *data_end, struct iphdr *iph)
{
    if ((void *)(iph + 1) > data_end)
        return TC_ACT_OK;
    if (iph->ihl < 5) /* options are fine: the L4 header is found via ihl */
        return TC_ACT_OK;
    if (iph->frag_off & __constant_htons(RIVORA_IP_OFFSET))
        return handle_v4_later_fragment(iph);
    if (iph->protocol != IPPROTO_TCP_ && iph->protocol != IPPROTO_UDP_)
        return TC_ACT_OK;

    void *l4 = (void *)iph + (iph->ihl * 4);
    __u16 sport, dport;

    if (iph->protocol == IPPROTO_TCP_) {
        struct tcphdr *tcph = l4;
        if ((void *)(tcph + 1) > data_end)
            return TC_ACT_OK;
        sport = tcph->source;
        dport = tcph->dest;
    } else {
        struct udphdr *udph = l4;
        if ((void *)(udph + 1) > data_end)
            return TC_ACT_OK;
        sport = udph->source;
        dport = udph->dest;
    }

    /* This is the backend->client leg: src = backend, dst = client. */
    struct nat_reverse_key rk = {
        .backend_addr = iph->saddr, .client_addr = iph->daddr,
        .backend_port = sport, .client_port = dport, .proto = iph->protocol,
    };
    struct nat_reverse_val *rv = bpf_map_lookup_elem(&nat_reverse_map, &rk);
    if (!rv)
        return TC_ACT_OK; /* not a flow we NAT'd (or affinity already expired) */

    __u32 old_saddr = iph->saddr;
    __u32 new_saddr = rv->vip_addr;
    __u16 old_sport = sport;
    __u16 new_sport = rv->vip_port;

    /* First fragment of a fragmented reply: remember the rewrite for the rest of it. */
    if (iph->frag_off & __constant_htons(RIVORA_IP_MF)) {
        struct frag_key fk = {.saddr = iph->saddr, .daddr = iph->daddr, .id = iph->id, .proto = iph->protocol};
        struct frag_rev_val fv = {.vip_addr = new_saddr};
        bpf_map_update_elem(&frag_rev_map, &fk, &fv, BPF_ANY);
    }

    /* Re-derive the L4 pointer fresh here too — see xdp_ingress.c's NAT
     * block for why reusing tcph/udph from much earlier doesn't reliably
     * satisfy the verifier this far into the function. */
    void *l4b = (void *)iph + (iph->ihl * 4);
    if (iph->protocol == IPPROTO_TCP_) {
        struct tcphdr *t = l4b;
        if ((void *)(t + 1) > data_end)
            return TC_ACT_OK;
        csum_replace(&iph->check, &old_saddr, &new_saddr, 4);
        csum_replace(&t->check, &old_saddr, &new_saddr, 4);
        csum_replace(&t->check, &old_sport, &new_sport, 2);
        t->source = new_sport;
    } else {
        struct udphdr *u = l4b;
        if ((void *)(u + 1) > data_end)
            return TC_ACT_OK;
        csum_replace(&iph->check, &old_saddr, &new_saddr, 4);
        if (u->check != 0) {
            csum_replace(&u->check, &old_saddr, &new_saddr, 4);
            csum_replace(&u->check, &old_sport, &new_sport, 2);
        }
        u->source = new_sport;
    }
    iph->saddr = new_saddr;

    return TC_ACT_OK;
}

/* IPv6 sibling of handle_ipv4 — same reverse-NAT shape against
 * nat_reverse_map6, with the same two checksum differences xdp_ingress.c's
 * handle_ipv6 documents: no IP-header checksum to touch at all (IPv6
 * dropped it), and a UDP checksum is never "unset" over IPv6 so it's
 * always updated unconditionally, unlike v4's "if (u->check != 0)" skip. */
static __always_inline int handle_ipv6(void *data_end, struct ipv6hdr *iph)
{
    if ((void *)(iph + 1) > data_end)
        return TC_ACT_OK;

    /* Same extension-header walk as xdp_ingress: a reply can carry a Fragment header (a large UDP
     * reply) or, rarely, Hop-by-Hop / Destination Options. */
    struct rivora_v6_l4 w;
    if (rivora_v6_walk(iph, data_end, &w) < 0)
        return TC_ACT_OK;
    const __u8 proto = w.proto;
    if (proto != IPPROTO_TCP_ && proto != IPPROTO_UDP_)
        return TC_ACT_OK;

    /* A later fragment of a reply has no L4 header: it gets the source rewrite its first fragment
     * got. Only the IP header changes; the L4 checksum, in the first fragment, already covers it. */
    if (w.is_frag && w.frag_off != 0) {
        struct frag_key6 fk = {.id = w.id, .proto = proto};
        __builtin_memcpy(fk.saddr, &iph->saddr, 16);
        __builtin_memcpy(fk.daddr, &iph->daddr, 16);
        struct frag_rev_val6 *fv = bpf_map_lookup_elem(&frag_rev_map6, &fk);
        if (fv)
            __builtin_memcpy(&iph->saddr, fv->vip_addr, 16);
        return TC_ACT_OK;
    }

    void *l4 = (void *)(iph + 1) + (w.off & 0xff);
    __u16 sport, dport;

    if (proto == IPPROTO_TCP_) {
        struct tcphdr *tcph = l4;
        if ((void *)(tcph + 1) > data_end)
            return TC_ACT_OK;
        sport = tcph->source;
        dport = tcph->dest;
    } else {
        struct udphdr *udph = l4;
        if ((void *)(udph + 1) > data_end)
            return TC_ACT_OK;
        sport = udph->source;
        dport = udph->dest;
    }

    /* This is the backend->client leg: src = backend, dst = client. */
    struct nat_reverse_key6 rk = {.backend_port = sport, .client_port = dport, .proto = proto};
    __builtin_memcpy(rk.backend_addr, &iph->saddr, 16);
    __builtin_memcpy(rk.client_addr, &iph->daddr, 16);
    struct nat_reverse_val6 *rv = bpf_map_lookup_elem(&nat_reverse_map6, &rk);
    if (!rv)
        return TC_ACT_OK; /* not a flow we NAT'd (or affinity already expired) */

    __u8 old_saddr[16], new_saddr[16];
    __builtin_memcpy(old_saddr, &iph->saddr, 16);
    __builtin_memcpy(new_saddr, rv->vip_addr, 16);
    __u16 old_sport = sport;
    __u16 new_sport = rv->vip_port;

    /* Re-derive the L4 pointer fresh here too — same reasoning as
     * handle_ipv4's NAT block. */
    void *l4b = (void *)(iph + 1) + (w.off & 0xff);
    if (proto == IPPROTO_TCP_) {
        struct tcphdr *t = l4b;
        if ((void *)(t + 1) > data_end)
            return TC_ACT_OK;
        csum_replace(&t->check, old_saddr, new_saddr, 16);
        csum_replace(&t->check, &old_sport, &new_sport, 2);
        t->source = new_sport;
    } else {
        struct udphdr *u = l4b;
        if ((void *)(u + 1) > data_end)
            return TC_ACT_OK;
        csum_replace(&u->check, old_saddr, new_saddr, 16);
        csum_replace(&u->check, &old_sport, &new_sport, 2);
        u->source = new_sport;
    }
    /* First fragment of a fragmented reply: remember the rewrite for the rest of it. Keyed by the
     * addresses as they are before the rewrite, which is what the later fragments carry. */
    if (w.is_frag && w.more) {
        struct frag_key6 fk = {.id = w.id, .proto = proto};
        __builtin_memcpy(fk.saddr, old_saddr, 16);
        __builtin_memcpy(fk.daddr, &iph->daddr, 16);
        struct frag_rev_val6 fv;
        __builtin_memcpy(fv.vip_addr, new_saddr, 16);
        bpf_map_update_elem(&frag_rev_map6, &fk, &fv, BPF_ANY);
    }
    __builtin_memcpy(&iph->saddr, new_saddr, 16);

    return TC_ACT_OK;
}

SEC("tc")
int rivora_tc_nat_egress(struct __sk_buff *skb)
{
    void *data = (void *)(long)skb->data;
    void *data_end = (void *)(long)skb->data_end;

    struct ethhdr *eth = data;
    if ((void *)(eth + 1) > data_end)
        return TC_ACT_OK;

    /* Step over VLAN tags that are in the frame (a tag the driver offloads is not). */
    void *l3;
    __be16 proto;
    if (rivora_skip_vlan(eth + 1, eth->h_proto, data_end, &l3, &proto) < 0)
        return TC_ACT_OK;

    if (proto == __constant_htons(ETH_P_IP))
        return handle_ipv4(data_end, l3);
    if (proto == __constant_htons(ETH_P_IPV6))
        return handle_ipv6(data_end, l3);
    return TC_ACT_OK;
}

char _license[] SEC("license") = "GPL";
