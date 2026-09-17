// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
//
// Rivora XDP ingress: VIP match -> connection affinity / Maglev backend
// selection -> DSR (MAC rewrite, XDP_TX) or full-NAT (IP/port rewrite,
// XDP_PASS to normal routing) forwarding. TCP/UDP only, both IPv4
// (handle_ipv4) and IPv6 (handle_ipv6, v0.3) — see rivora_common.h's
// comment above the vip_key6/etc. struct definitions for which maps are
// shared between the two families and which need a v6-specific sibling.
//
// Multiple VIPs share the one flat maglev_table via non-overlapping
// (offset, size) ranges the Go-side extent allocator hands out — see
// service_config.maglev_offset/maglev_size below and
// internal/dataplane/maglevalloc.go. The hash is reduced modulo *this
// VIP's* maglev_size, not the whole table, then added to its offset — see
// pick_backend()'s doc comment for why that distinction matters.
//
// Scope limitations tracked for later milestones: IP options / IPv6
// extension headers, and encapsulated (Geneve/IPIP) backends.

#include <linux/if_ether.h>
#include <linux/ip.h>
#include <linux/ipv6.h>
#include <linux/tcp.h>
#include <linux/udp.h>
#include <linux/in.h>

#include "rivora_common.h"

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 4096); /* bpfmaps.MaxVIPs — cluster-wide VIP ceiling, v0.2 */
    __type(key, struct vip_key);
    __type(value, __u32); /* service_id */
} vip_map SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, 4096); /* bpfmaps.MaxVIPs */
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
    __uint(max_entries, 8192); /* bpfmaps.MaxBackends — cluster-wide backend ceiling, v0.2 */
    __type(key, __u32); /* backend_id */
    __type(value, struct backend_info);
} backend_map SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, 8192); /* bpfmaps.MaxBackends */
    __type(key, __u32); /* backend_id */
    __type(value, __u8); /* RIVORA_HEALTH_{DOWN,HEALTHY,DRAINING} */
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
    __uint(max_entries, 8193); /* 1 (global) + bpfmaps.MaxBackends */
    __type(key, __u32); /* 0 = global, 1+backend_id = per backend */
    __type(value, struct lb_stats);
} stats_map SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, struct rl_config);
} rl_config_map SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LRU_PERCPU_HASH);
    __uint(max_entries, 65536);
    __type(key, __u32); /* source IP, network byte order */
    __type(value, struct rl_bucket);
} rl_buckets_map SEC(".maps");

/* IPv6 siblings of the address-keyed-or-valued maps above. Only these five
 * need one — service_config_map, maglev_table, backend_health_map,
 * stats_map, iface_mac_map and rl_config_map are shared as-is between v4
 * and v6 VIPs/backends (opaque-ID-keyed, address-family agnostic) — see
 * rivora_common.h's comment above the vip_key6/etc. struct definitions. */

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 4096); /* bpfmaps.MaxVIPs */
    __type(key, struct vip_key6);
    __type(value, __u32); /* service_id */
} vip_map6 SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 8192); /* bpfmaps.MaxBackends */
    __type(key, __u32); /* backend_id */
    __type(value, struct backend_info6);
} backend_map6 SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 65536);
    __type(key, struct conn_key6);
    __type(value, __u32); /* backend_id */
} connection_affinity_map6 SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 65536);
    __type(key, struct nat_reverse_key6);
    __type(value, struct nat_reverse_val6);
} nat_reverse_map6 SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LRU_PERCPU_HASH);
    __uint(max_entries, 65536);
    __type(key, struct addr6_key); /* source IPv6 address, network byte order */
    __type(value, struct rl_bucket);
} rl_buckets_map6 SEC(".maps");

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

/* Beyond this, a bucket is treated as fully refilled regardless of its
 * configured rate — caps the refill multiply below against overflow
 * without needing to reason about how long a source IP has been idle
 * (which, for a long-uptime LB, could otherwise be an arbitrarily large
 * nanosecond count). Any sane rate/burst combination refills in well
 * under 10s anyway. */
#define RIVORA_RL_MAX_ELAPSED_NS 10000000000ULL

/* Per-source-IP SYN token bucket: consult rl_config_map (a cheap single
 * array lookup, near-zero cost when the feature is off) and, if enabled,
 * refill-then-consume one token from saddr's bucket in rl_buckets_map,
 * reporting whether it was empty. Only ever called for TCP SYN packets —
 * see rivora_xdp_ingress — so established connections' data packets and
 * all UDP traffic are unaffected regardless of this limiter's state. */
static __always_inline int rate_limit_exceeded(__u32 saddr)
{
    __u32 zero = 0;
    struct rl_config *cfg = bpf_map_lookup_elem(&rl_config_map, &zero);
    if (!cfg || !cfg->enabled)
        return 0;

    __u64 now = bpf_ktime_get_ns();
    struct rl_bucket *b = bpf_map_lookup_elem(&rl_buckets_map, &saddr);
    struct rl_bucket fresh = {.tokens = cfg->burst, .last_refill_ns = now};
    if (!b)
        b = &fresh;

    __u64 elapsed_ns = now > b->last_refill_ns ? now - b->last_refill_ns : 0;
    if (elapsed_ns > RIVORA_RL_MAX_ELAPSED_NS)
        elapsed_ns = RIVORA_RL_MAX_ELAPSED_NS;

    __u64 tokens = b->tokens + (elapsed_ns * cfg->rate_per_sec) / 1000000000ULL;
    if (tokens > cfg->burst)
        tokens = cfg->burst;

    int exceeded = tokens < 1;
    if (!exceeded)
        tokens -= 1;

    struct rl_bucket updated = {.tokens = tokens, .last_refill_ns = now};
    bpf_map_update_elem(&rl_buckets_map, &saddr, &updated, BPF_ANY);
    return exceeded;
}

/* IPv6 sibling of rate_limit_exceeded: identical token-bucket math against
 * rl_buckets_map6 instead of rl_buckets_map, sharing the same rl_config_map
 * on/off switch and rate/burst (rate limiting is a single cluster-wide
 * per-source-address feature, not per-VIP or per-family — v4 and v6
 * sources are governed by the same configured rate, just tracked in
 * separate bucket maps since the key width differs). Duplicated rather
 * than parameterized over map/key type, matching this file's existing
 * style of explicit per-branch logic over generic helpers (see the
 * TCP/UDP duplication throughout rivora_xdp_ingress). */
static __always_inline int rate_limit_exceeded_v6(const __u8 saddr[16])
{
    __u32 zero = 0;
    struct rl_config *cfg = bpf_map_lookup_elem(&rl_config_map, &zero);
    if (!cfg || !cfg->enabled)
        return 0;

    struct addr6_key key;
    __builtin_memcpy(key.addr, saddr, 16);

    __u64 now = bpf_ktime_get_ns();
    struct rl_bucket *b = bpf_map_lookup_elem(&rl_buckets_map6, &key);
    struct rl_bucket fresh = {.tokens = cfg->burst, .last_refill_ns = now};
    if (!b)
        b = &fresh;

    __u64 elapsed_ns = now > b->last_refill_ns ? now - b->last_refill_ns : 0;
    if (elapsed_ns > RIVORA_RL_MAX_ELAPSED_NS)
        elapsed_ns = RIVORA_RL_MAX_ELAPSED_NS;

    __u64 tokens = b->tokens + (elapsed_ns * cfg->rate_per_sec) / 1000000000ULL;
    if (tokens > cfg->burst)
        tokens = cfg->burst;

    int exceeded = tokens < 1;
    if (!exceeded)
        tokens -= 1;

    struct rl_bucket updated = {.tokens = tokens, .last_refill_ns = now};
    bpf_map_update_elem(&rl_buckets_map6, &key, &updated, BPF_ANY);
    return exceeded;
}

/* Bounded probe for a healthy backend starting at the Maglev-selected slot.
 * Approximates Maglev's degraded lookup (which recomputes the whole
 * permutation on membership change) by walking forward a few slots — good
 * enough to route around a handful of simultaneously-down backends without
 * an unbounded loop in the verifier.
 *
 * local_slot/offset/size confine every probed slot to *this VIP's own*
 * extent of the shared maglev_table (wrapping within it, not past it) —
 * without that, probing forward from a slot near the end of a small extent
 * would walk into a different VIP's slots, or unpopulated (zero-valued,
 * i.e. backend_id 0) ones. */
static __always_inline int pick_backend(__u32 offset, __u32 size, __u32 local_slot, __u32 *out_backend_id)
{
#pragma unroll
    for (int i = 0; i < RIVORA_MAGLEV_PROBES; i++) {
        __u32 s = offset + ((local_slot + i) % size);
        __u32 *bid = bpf_map_lookup_elem(&maglev_table, &s);
        if (!bid)
            continue;
        /* Draining backends are excluded from *new* flow selection here,
         * but connection_affinity_map's fast path (any nonzero value is
         * truthy) still honors them for already-established flows — see
         * RIVORA_HEALTH_* in rivora_common.h. */
        __u8 *healthy = bpf_map_lookup_elem(&backend_health_map, bid);
        if (healthy && *healthy == RIVORA_HEALTH_HEALTHY) {
            *out_backend_id = *bid;
            return 0;
        }
    }
    return -1;
}

/* IPv4 path — split out of rivora_xdp_ingress unchanged (a verbatim move,
 * not a rewrite) so the v6 branch below can sit alongside it as its own
 * named function instead of growing one already-long function further. */
static __always_inline int handle_ipv4(struct xdp_md *ctx, void *data, void *data_end, struct ethhdr *eth)
{
    struct iphdr *iph = (void *)(eth + 1);
    if ((void *)(iph + 1) > data_end)
        return XDP_PASS;
    if (iph->ihl != 5) /* v0.1: no IP options */
        return XDP_PASS;
    if (iph->protocol != IPPROTO_TCP_ && iph->protocol != IPPROTO_UDP_)
        return XDP_PASS;

    __u16 sport, dport;
    __u8 is_syn = 0;
    void *l4 = (void *)iph + (iph->ihl * 4);

    if (iph->protocol == IPPROTO_TCP_) {
        struct tcphdr *tcph = l4;
        if ((void *)(tcph + 1) > data_end)
            return XDP_PASS;
        sport = tcph->source;
        dport = tcph->dest;
        is_syn = tcph->syn;
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

    /* SYN-flood mitigation: only new-connection attempts (SYN packets)
     * consume a token — established connections' data packets and all UDP
     * traffic are untouched regardless of this limiter's state. A no-op,
     * one-array-lookup cost when rl_config_map's enabled bit is unset
     * (the default). */
    if (is_syn && rate_limit_exceeded(iph->saddr)) {
        bump_stats(RIVORA_STATS_GLOBAL, 0, 1);
        return XDP_DROP;
    }

    struct service_config *cfg = bpf_map_lookup_elem(&service_config_map, service_id);
    if (!cfg || cfg->backend_count == 0 || cfg->maglev_size == 0)
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
        __u32 local_slot = hash % cfg->maglev_size;
        if (pick_backend(cfg->maglev_offset, cfg->maglev_size, local_slot, &backend_id) < 0) {
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

/* IPv6 path. Structurally mirrors handle_ipv4 closely (same VIP-match ->
 * rate-limit -> connection-affinity/Maglev -> DSR-or-NAT shape), reusing
 * every address-family-agnostic map (service_config_map, maglev_table,
 * backend_health_map, stats_map, iface_mac_map, rl_config_map,
 * pick_backend()) as-is — only the address-keyed maps and a few checksum
 * details differ from v4, noted inline below. */
static __always_inline int handle_ipv6(struct xdp_md *ctx, void *data, void *data_end, struct ethhdr *eth)
{
    struct ipv6hdr *iph = (void *)(eth + 1);
    if ((void *)(iph + 1) > data_end)
        return XDP_PASS;
    /* v0.3: no extension headers, mirroring handle_ipv4's "no IP options"
     * scope limitation — nexthdr must be TCP/UDP directly. A packet with a
     * Hop-by-Hop/Routing/Fragment/etc. extension header before the L4
     * header is passed through unmodified rather than walked. */
    if (iph->nexthdr != IPPROTO_TCP_ && iph->nexthdr != IPPROTO_UDP_)
        return XDP_PASS;

    __u16 sport, dport;
    __u8 is_syn = 0;
    void *l4 = (void *)(iph + 1);

    if (iph->nexthdr == IPPROTO_TCP_) {
        struct tcphdr *tcph = l4;
        if ((void *)(tcph + 1) > data_end)
            return XDP_PASS;
        sport = tcph->source;
        dport = tcph->dest;
        is_syn = tcph->syn;
    } else {
        struct udphdr *udph = l4;
        if ((void *)(udph + 1) > data_end)
            return XDP_PASS;
        sport = udph->source;
        dport = udph->dest;
    }

    struct vip_key6 vk = {.port = dport, .proto = iph->nexthdr};
    __builtin_memcpy(vk.addr, &iph->daddr, 16);
    __u32 *service_id = bpf_map_lookup_elem(&vip_map6, &vk);
    if (!service_id)
        return XDP_PASS; /* not a VIP we own */

    if (is_syn && rate_limit_exceeded_v6((__u8 *)&iph->saddr)) {
        bump_stats(RIVORA_STATS_GLOBAL, 0, 1);
        return XDP_DROP;
    }

    struct service_config *cfg = bpf_map_lookup_elem(&service_config_map, service_id);
    if (!cfg || cfg->backend_count == 0 || cfg->maglev_size == 0)
        return XDP_PASS;

    struct conn_key6 ck = {.sport = sport, .dport = dport, .proto = iph->nexthdr};
    __builtin_memcpy(ck.saddr, &iph->saddr, 16);
    __builtin_memcpy(ck.daddr, &iph->daddr, 16);

    __u32 backend_id;
    __u32 *affinity = bpf_map_lookup_elem(&connection_affinity_map6, &ck);
    __u8 use_affinity = 0;
    if (affinity) {
        __u8 *healthy = bpf_map_lookup_elem(&backend_health_map, affinity);
        if (healthy && *healthy) {
            backend_id = *affinity;
            use_affinity = 1;
        }
    }

    if (!use_affinity) {
        __u32 hash = rivora_hash5_v6((__u8 *)&iph->saddr, (__u8 *)&iph->daddr, sport, dport, iph->nexthdr);
        __u32 local_slot = hash % cfg->maglev_size;
        if (pick_backend(cfg->maglev_offset, cfg->maglev_size, local_slot, &backend_id) < 0) {
            bump_stats(RIVORA_STATS_GLOBAL, 0, 1);
            return XDP_DROP; /* no healthy backend within probe window */
        }
        bpf_map_update_elem(&connection_affinity_map6, &ck, &backend_id, BPF_ANY);
    }

    struct backend_info6 *be = bpf_map_lookup_elem(&backend_map6, &backend_id);
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
         * bound locally so it accepts and replies directly. */
        return XDP_TX;
    }

    /* Full NAT (IPv6). Two real differences from handle_ipv4's NAT block,
     * not just cosmetic ones — get these right:
     *   - IPv6 has no IP-header checksum at all (RFC 8200 dropped it), so
     *     there's no csum_replace call for one here, unlike iph->check in
     *     v4. Only the L4 checksum needs updating.
     *   - The pseudo-header the L4 checksum covers includes the full
     *     128-bit address, so the diff is over 16 bytes, not 4.
     *   - A UDP checksum is never "unset" over IPv6 (RFC 8200 §8.1
     *     forbids sending it as zero) — always update it unconditionally,
     *     don't carry over v4's "if (u->check != 0)" skip.
     *
     * Re-derive the L4 pointer fresh right here, same reasoning as
     * handle_ipv4's NAT block: a pointer captured earlier in the function,
     * before several intervening map lookups and branches, doesn't
     * reliably keep its verified-safe-pointer status this far in. */
    __u8 old_daddr[16], new_daddr[16];
    __builtin_memcpy(old_daddr, &iph->daddr, 16);
    __builtin_memcpy(new_daddr, be->addr, 16);
    __u16 old_dport = dport;
    __u16 new_dport = be->port;
    void *l4b = (void *)(iph + 1);

    if (iph->nexthdr == IPPROTO_TCP_) {
        struct tcphdr *t = l4b;
        if ((void *)(t + 1) > data_end)
            return XDP_PASS;
        csum_replace(&t->check, old_daddr, new_daddr, 16);
        csum_replace(&t->check, &old_dport, &new_dport, 2);
        t->dest = new_dport;
    } else {
        struct udphdr *u = l4b;
        if ((void *)(u + 1) > data_end)
            return XDP_PASS;
        csum_replace(&u->check, old_daddr, new_daddr, 16);
        csum_replace(&u->check, &old_dport, &new_dport, 2);
        u->dest = new_dport;
    }
    __builtin_memcpy(&iph->daddr, new_daddr, 16);

    struct nat_reverse_key6 rk = {.backend_port = be->port, .client_port = sport, .proto = iph->nexthdr};
    __builtin_memcpy(rk.backend_addr, be->addr, 16);
    __builtin_memcpy(rk.client_addr, &iph->saddr, 16); /* saddr untouched by dest-NAT */
    struct nat_reverse_val6 rv = {.vip_port = old_dport};
    __builtin_memcpy(rv.vip_addr, old_daddr, 16);
    bpf_map_update_elem(&nat_reverse_map6, &rk, &rv, BPF_ANY);

    return XDP_PASS;
}

SEC("xdp")
int rivora_xdp_ingress(struct xdp_md *ctx)
{
    void *data = (void *)(long)ctx->data;
    void *data_end = (void *)(long)ctx->data_end;

    struct ethhdr *eth = data;
    if ((void *)(eth + 1) > data_end)
        return XDP_PASS;

    if (eth->h_proto == __constant_htons(ETH_P_IP))
        return handle_ipv4(ctx, data, data_end, eth);
    if (eth->h_proto == __constant_htons(ETH_P_IPV6))
        return handle_ipv6(ctx, data, data_end, eth);
    return XDP_PASS;
}

char _license[] SEC("license") = "GPL";
