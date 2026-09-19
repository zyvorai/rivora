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

/* Per-service drop/bypass counters, indexed by service_id. PERCPU so bumping
 * needs no atomics; userspace sums across CPUs. A new map (not a change to
 * stats_map), so an already-pinned deployment simply gains it on upgrade. */
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, 4096); /* bpfmaps.MaxVIPs */
    __type(key, __u32); /* service_id */
    __type(value, struct drop_stats);
} drop_stats_map SEC(".maps");

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

/* Per-Service SYN limits (see struct svc_rl_key in rivora_common.h). New maps, so
 * an already-pinned deployment simply gains them on upgrade. */
struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, 4096); /* bpfmaps.MaxVIPs */
    __type(key, __u32); /* service_id */
    __type(value, struct rl_config);
} svc_rl_config_map SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LRU_PERCPU_HASH);
    __uint(max_entries, 65536);
    __type(key, struct svc_rl_key);
    __type(value, struct rl_bucket);
} svc_rl_buckets_map SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LRU_PERCPU_HASH);
    __uint(max_entries, 65536);
    __type(key, struct svc_rl_key6);
    __type(value, struct rl_bucket);
} svc_rl_buckets_map6 SEC(".maps");

/* Outer-header source addresses for the tunnel modes (see struct tunnel_config). */
struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, struct tunnel_config);
} tunnel_config_map SEC(".maps");

/* IPv4 fragment steering (see struct frag_key). */
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 16384);
    __type(key, struct frag_key);
    __type(value, struct frag_val);
} frag_map SEC(".maps");

/* Port-range VIPs (see struct vip_range_key). BPF_F_NO_PREALLOC is mandatory for an LPM
 * trie. Room for MaxVIPs ranges of a few blocks each. */
struct {
    __uint(type, BPF_MAP_TYPE_LPM_TRIE);
    __uint(max_entries, 16384); /* bpfmaps.MaxRangeBlocks */
    __uint(map_flags, BPF_F_NO_PREALLOC);
    __type(key, struct vip_range_key);
    __type(value, __u32); /* service_id */
} vip_range_map SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LPM_TRIE);
    __uint(max_entries, 16384); /* bpfmaps.MaxRangeBlocks */
    __uint(map_flags, BPF_F_NO_PREALLOC);
    __type(key, struct vip_range_key6);
    __type(value, __u32); /* service_id */
} vip_range_map6 SEC(".maps");

static __always_inline void bump_drop(__u32 service_id, __u32 reason)
{
    struct drop_stats *d = bpf_map_lookup_elem(&drop_stats_map, &service_id);
    if (!d || reason >= RIVORA_DROP_REASONS)
        return;
    d->by_reason[reason] += 1;
}

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

/* SYN token bucket. A Service with its own limit (svc_rl_config_map[sid].enabled)
 * is governed by that alone; every other Service falls back to the node-wide
 * limit in rl_config_map, which is a single cheap array lookup and does nothing
 * when the feature is off. Only ever called for TCP SYN packets — see
 * rivora_xdp_ingress — so established connections' data packets and all UDP
 * traffic are unaffected regardless of the limiter's state. */
static __always_inline int rate_limit_exceeded(__u32 sid, __u32 saddr)
{
    struct rl_bucket updated;
    __u64 now;

    struct rl_config *scfg = bpf_map_lookup_elem(&svc_rl_config_map, &sid);
    if (scfg && scfg->enabled) {
        struct svc_rl_key sk = {.service_id = sid, .saddr = saddr};
        now = bpf_ktime_get_ns();
        int exceeded = rl_take(scfg, bpf_map_lookup_elem(&svc_rl_buckets_map, &sk), &updated, now);
        bpf_map_update_elem(&svc_rl_buckets_map, &sk, &updated, BPF_ANY);
        return exceeded;
    }

    __u32 zero = 0;
    struct rl_config *cfg = bpf_map_lookup_elem(&rl_config_map, &zero);
    if (!cfg || !cfg->enabled)
        return 0;

    now = bpf_ktime_get_ns();
    int exceeded = rl_take(cfg, bpf_map_lookup_elem(&rl_buckets_map, &saddr), &updated, now);
    bpf_map_update_elem(&rl_buckets_map, &saddr, &updated, BPF_ANY);
    return exceeded;
}

/* IPv6 sibling of rate_limit_exceeded: the same limiter over the same configs
 * (v4 and v6 sources are governed by one configured rate), tracked in separate
 * bucket maps because the key width differs. */
static __always_inline int rate_limit_exceeded_v6(__u32 sid, const __u8 saddr[16])
{
    struct rl_bucket updated;
    __u64 now;

    struct rl_config *scfg = bpf_map_lookup_elem(&svc_rl_config_map, &sid);
    if (scfg && scfg->enabled) {
        struct svc_rl_key6 sk;
        sk.service_id = sid;
        __builtin_memcpy(sk.saddr, saddr, 16);
        now = bpf_ktime_get_ns();
        int exceeded = rl_take(scfg, bpf_map_lookup_elem(&svc_rl_buckets_map6, &sk), &updated, now);
        bpf_map_update_elem(&svc_rl_buckets_map6, &sk, &updated, BPF_ANY);
        return exceeded;
    }

    __u32 zero = 0;
    struct rl_config *cfg = bpf_map_lookup_elem(&rl_config_map, &zero);
    if (!cfg || !cfg->enabled)
        return 0;

    struct addr6_key key;
    __builtin_memcpy(key.addr, saddr, 16);
    now = bpf_ktime_get_ns();
    int exceeded = rl_take(cfg, bpf_map_lookup_elem(&rl_buckets_map6, &key), &updated, now);
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

/* DSR: send the frame back out the interface it came in on, addressed to the backend. The
 * VIP stays the destination IP, so the backend must have the VIP bound locally. */
static __always_inline int dsr_forward(struct xdp_md *ctx, struct ethhdr *eth, const __u8 mac[6])
{
    __u32 ifindex = ctx->ingress_ifindex;
    struct mac_addr *self_mac = bpf_map_lookup_elem(&iface_mac_map, &ifindex);
    if (self_mac)
        __builtin_memcpy(eth->h_source, self_mac->addr, 6);
    __builtin_memcpy(eth->h_dest, mac, 6);
    return XDP_TX;
}

/* The service a v4 (addr, port, proto) belongs to: an exact-port VIP, else a port range. */
static __always_inline __u32 *v4_service(__u32 addr, __u16 port, __u8 proto)
{
    struct vip_key vk = {.addr = addr, .port = port, .proto = proto};
    __u32 *sid = bpf_map_lookup_elem(&vip_map, &vk);
    if (sid)
        return sid;
    struct vip_range_key rgk = {
        .prefixlen = RIVORA_RANGE_FULL_PREFIX_V4,
        .addr = addr, .proto = proto, .port = port,
    };
    return bpf_map_lookup_elem(&vip_range_map, &rgk);
}

static __always_inline __u32 *v6_service(const __u8 addr[16], __u16 port, __u8 proto)
{
    struct vip_key6 vk = {.port = port, .proto = proto};
    __builtin_memcpy(vk.addr, addr, 16);
    __u32 *sid = bpf_map_lookup_elem(&vip_map6, &vk);
    if (sid)
        return sid;
    struct vip_range_key6 rgk = {
        .prefixlen = RIVORA_RANGE_FULL_PREFIX_V6,
        .proto = proto, .port = port,
    };
    __builtin_memcpy(rgk.addr, addr, 16);
    return bpf_map_lookup_elem(&vip_range_map6, &rgk);
}

/* ---- L3 DSR: encapsulate and forward ---------------------------------------------------
 *
 * The packet becomes  [L2][outer IP][GRE?][inner IP...]  addressed to the backend. Room is made
 * at the front with bpf_xdp_adjust_head; that leaves the old L2 header sitting where the outer
 * header must go, so it is saved first and rewritten at the new front. Nothing in the packet
 * survives an adjust_head as a pointer, so everything is re-derived from ctx afterwards.
 *
 * The outer header is then routed with bpf_fib_lookup, which yields the next hop's MACs and the
 * egress interface: the frame goes straight out (XDP_TX on the same interface, else redirect),
 * never up the load balancer's own stack. That matters: a packet whose source is one of the
 * load balancer's own addresses would be dropped as a martian on the way back in. If the lookup
 * cannot answer (no neighbour yet, VLAN sub-interface, too big for the egress MTU) the packet is
 * passed to the kernel, which can resolve, forward and fragment it (the outer DF bit is clear).
 * The path from the load balancer to the backends must carry the inner packet plus the outer
 * header (20 bytes, 24 with GRE, 40 with IPv6): XDP cannot fragment. */

static __always_inline __u16 rivora_ip_csum(void *hdr, __u32 len)
{
    __u32 sum = (__u32)bpf_csum_diff(0, 0, (__be32 *)hdr, len, 0);
    sum = (sum & 0xffff) + (sum >> 16);
    sum = (sum & 0xffff) + (sum >> 16);
    return (__u16)~sum;
}

static __always_inline int tunnel_v4_l2(struct xdp_md *ctx, struct ethhdr *eth, struct iphdr *iph,
                                        __u8 mode, __u32 be_addr, const __u32 l2len)
{
    void *data = (void *)(long)ctx->data;
    void *data_end = (void *)(long)ctx->data_end;

    if ((void *)eth + 22 > data_end || (void *)(iph + 1) > data_end)
        return XDP_PASS;

    __u32 zero = 0;
    struct tunnel_config *tc = bpf_map_lookup_elem(&tunnel_config_map, &zero);
    if (!tc || !tc->src4) {
        bump_stats(RIVORA_STATS_GLOBAL, 0, 1);
        return XDP_DROP;
    }
    __u32 src4 = tc->src4;

    __u8 gre = mode == RIVORA_MODE_TUNNEL_GRE;
    __u32 encap = gre ? 24 : 20;
    __u8 tos = iph->tos;
    __u16 inner_len = __builtin_bswap16(iph->tot_len);
    __u8 l2buf[22];
    __builtin_memcpy(l2buf, eth, 22);

    if (bpf_xdp_adjust_head(ctx, 0 - (int)encap))
        return XDP_DROP;
    data = (void *)(long)ctx->data;
    data_end = (void *)(long)ctx->data_end;
    if (data + 22 > data_end)
        return XDP_DROP;
    __builtin_memcpy(data, l2buf, 22);

    struct iphdr hdr = {};
    hdr.version = 4;
    hdr.ihl = 5;
    hdr.tos = tos;
    hdr.tot_len = __builtin_bswap16((__u16)(inner_len + encap));
    hdr.id = (__u16)bpf_get_prandom_u32(); /* the outer header may be fragmented downstream: give it an id */
    hdr.ttl = 64;
    hdr.protocol = gre ? 47 : 4;
    hdr.saddr = src4;
    hdr.daddr = be_addr;
    hdr.check = rivora_ip_csum(&hdr, 20);

    struct iphdr *o = data + l2len;
    if ((void *)(o + 1) > data_end)
        return XDP_DROP;
    __builtin_memcpy(o, &hdr, 20);
    if (gre) {
        __be16 *g = (void *)(o + 1);
        if ((void *)(g + 2) > data_end)
            return XDP_DROP;
        g[0] = 0;                        /* no checksum, key or sequence */
        g[1] = __constant_htons(0x0800); /* the payload is IPv4 */
    }

    if (l2len != 14)
        return XDP_PASS;
    struct ethhdr *e2 = data;
    if ((void *)(e2 + 1) > data_end)
        return XDP_DROP;
    struct bpf_fib_lookup fib = {};
    fib.family = 2; /* AF_INET */
    fib.tos = tos;
    fib.l4_protocol = hdr.protocol;
    fib.tot_len = __builtin_bswap16(hdr.tot_len);
    fib.ifindex = ctx->ingress_ifindex;
    fib.ipv4_src = src4;
    fib.ipv4_dst = be_addr;
    if (bpf_fib_lookup(ctx, &fib, sizeof(fib), 0) != BPF_FIB_LKUP_RET_SUCCESS)
        return XDP_PASS;
    __builtin_memcpy(e2->h_dest, fib.dmac, 6);
    __builtin_memcpy(e2->h_source, fib.smac, 6);
    if (fib.ifindex == ctx->ingress_ifindex)
        return XDP_TX;
    return bpf_redirect(fib.ifindex, 0);
}

static __always_inline int tunnel_v6_l2(struct xdp_md *ctx, struct ethhdr *eth, struct ipv6hdr *iph,
                                        __u8 mode, const __u8 be_addr[16], const __u32 l2len)
{
    void *data = (void *)(long)ctx->data;
    void *data_end = (void *)(long)ctx->data_end;

    if ((void *)eth + 22 > data_end || (void *)(iph + 1) > data_end)
        return XDP_PASS;

    __u32 zero = 0;
    struct tunnel_config *tc = bpf_map_lookup_elem(&tunnel_config_map, &zero);
    if (!tc) {
        bump_stats(RIVORA_STATS_GLOBAL, 0, 1);
        return XDP_DROP;
    }
    __u8 src6[16];
    __builtin_memcpy(src6, tc->src6, 16);
    if ((src6[0] | src6[1] | src6[2] | src6[3] | src6[4] | src6[5] | src6[6] | src6[7] | src6[8] | src6[9] |
         src6[10] | src6[11] | src6[12] | src6[13] | src6[14] | src6[15]) == 0) {
        bump_stats(RIVORA_STATS_GLOBAL, 0, 1);
        return XDP_DROP;
    }

    __u8 gre = mode == RIVORA_MODE_TUNNEL_GRE;
    __u32 encap = gre ? 44 : 40;
    __u8 b0 = ((__u8 *)iph)[0], b1 = ((__u8 *)iph)[1];
    __u16 inner_payload = __builtin_bswap16(iph->payload_len);
    __u8 l2buf[22];
    __builtin_memcpy(l2buf, eth, 22);

    if (bpf_xdp_adjust_head(ctx, 0 - (int)encap))
        return XDP_DROP;
    data = (void *)(long)ctx->data;
    data_end = (void *)(long)ctx->data_end;
    if (data + 22 > data_end)
        return XDP_DROP;
    __builtin_memcpy(data, l2buf, 22);

    struct ipv6hdr hdr = {};
    ((__u8 *)&hdr)[0] = 0x60 | (b0 & 0x0f); /* version 6, the inner traffic class... */
    ((__u8 *)&hdr)[1] = b1 & 0xf0;          /* ...and a zero flow label */
    hdr.payload_len = __builtin_bswap16((__u16)(inner_payload + 40 + (gre ? 4 : 0)));
    hdr.nexthdr = gre ? 47 : 41;
    hdr.hop_limit = 64;
    __builtin_memcpy(&hdr.saddr, src6, 16);
    __builtin_memcpy(&hdr.daddr, be_addr, 16);

    struct ipv6hdr *o = data + l2len;
    if ((void *)(o + 1) > data_end)
        return XDP_DROP;
    __builtin_memcpy(o, &hdr, 40);
    if (gre) {
        __be16 *g = (void *)(o + 1);
        if ((void *)(g + 2) > data_end)
            return XDP_DROP;
        g[0] = 0;
        g[1] = __constant_htons(0x86dd); /* the payload is IPv6 */
    }

    if (l2len != 14)
        return XDP_PASS;
    struct ethhdr *e2 = data;
    if ((void *)(e2 + 1) > data_end)
        return XDP_DROP;
    struct bpf_fib_lookup fib = {};
    fib.family = 10; /* AF_INET6 */
    fib.l4_protocol = hdr.nexthdr;
    fib.tot_len = __builtin_bswap16(hdr.payload_len) + 40;
    fib.ifindex = ctx->ingress_ifindex;
    __builtin_memcpy(fib.ipv6_src, src6, 16);
    __builtin_memcpy(fib.ipv6_dst, be_addr, 16);
    if (bpf_fib_lookup(ctx, &fib, sizeof(fib), 0) != BPF_FIB_LKUP_RET_SUCCESS)
        return XDP_PASS;
    __builtin_memcpy(e2->h_dest, fib.dmac, 6);
    __builtin_memcpy(e2->h_source, fib.smac, 6);
    if (fib.ifindex == ctx->ingress_ifindex)
        return XDP_TX;
    return bpf_redirect(fib.ifindex, 0);
}

/* The verifier can only bound a packet offset it can see is constant or freshly range-checked, and
 * the L2 length is neither once a helper call has spilled it. So each supported length (plain
 * Ethernet, one VLAN tag, two) gets its own instance with the length as a compile-time constant. */
static __always_inline int tunnel_v4(struct xdp_md *ctx, struct ethhdr *eth, struct iphdr *iph,
                                     __u8 mode, __u32 be_addr)
{
    __u32 l2len = (void *)iph - (void *)eth;
    if (l2len == 14)
        return tunnel_v4_l2(ctx, eth, iph, mode, be_addr, 14);
    if (l2len == 18)
        return tunnel_v4_l2(ctx, eth, iph, mode, be_addr, 18);
    if (l2len == 22)
        return tunnel_v4_l2(ctx, eth, iph, mode, be_addr, 22);
    return XDP_PASS;
}

static __always_inline int tunnel_v6(struct xdp_md *ctx, struct ethhdr *eth, struct ipv6hdr *iph,
                                     __u8 mode, const __u8 be_addr[16])
{
    __u32 l2len = (void *)iph - (void *)eth;
    if (l2len == 14)
        return tunnel_v6_l2(ctx, eth, iph, mode, be_addr, 14);
    if (l2len == 18)
        return tunnel_v6_l2(ctx, eth, iph, mode, be_addr, 18);
    if (l2len == 22)
        return tunnel_v6_l2(ctx, eth, iph, mode, be_addr, 22);
    return XDP_PASS;
}

/* The forwarding step of every non-NAT mode: a MAC rewrite (DSR) or an encapsulation. */
static __always_inline int forward_v4(struct xdp_md *ctx, struct ethhdr *eth, struct iphdr *iph,
                                      __u8 mode, const struct backend_info *be)
{
    if (mode == RIVORA_MODE_DSR)
        return dsr_forward(ctx, eth, be->mac);
    return tunnel_v4(ctx, eth, iph, mode, be->addr);
}

static __always_inline int forward_v6(struct xdp_md *ctx, struct ethhdr *eth, struct ipv6hdr *iph,
                                      __u8 mode, const struct backend_info6 *be)
{
    if (mode == RIVORA_MODE_DSR)
        return dsr_forward(ctx, eth, be->mac);
    return tunnel_v6(ctx, eth, iph, mode, be->addr);
}

/* A later IPv4 fragment (offset > 0) has no L4 header, so it can't be matched to a VIP by
 * port. It follows the backend its datagram's first fragment was given. In full-NAT only the
 * destination address changes: the L4 header, and the checksum in it, live in the first
 * fragment, which already accounted for the rewrite. */
static __always_inline int handle_v4_later_fragment(struct xdp_md *ctx, void *data, void *data_end,
                                                    struct ethhdr *eth, struct iphdr *iph)
{
    struct frag_key fk = {.saddr = iph->saddr, .daddr = iph->daddr, .id = iph->id, .proto = iph->protocol};
    struct frag_val *fv = bpf_map_lookup_elem(&frag_map, &fk);
    if (!fv)
        return XDP_PASS; /* not ours, or its first fragment hasn't been seen */

    __u32 backend_id = fv->backend_id;
    struct service_config *cfg = bpf_map_lookup_elem(&service_config_map, &fv->service_id);
    if (!cfg)
        return XDP_PASS;
    __u8 *healthy = bpf_map_lookup_elem(&backend_health_map, &backend_id);
    if (!healthy || !*healthy) {
        bump_stats(RIVORA_STATS_GLOBAL, 0, 1);
        return XDP_DROP; /* the first fragment went to a backend that has since died */
    }
    struct backend_info *be = bpf_map_lookup_elem(&backend_map, &backend_id);
    if (!be)
        return XDP_PASS;

    __u32 pkt_len = (__u32)(data_end - data);
    bump_stats(RIVORA_STATS_GLOBAL, pkt_len, 0);
    bump_stats(1 + backend_id, pkt_len, 0);

    if (cfg->mode != RIVORA_MODE_NAT)
        return forward_v4(ctx, eth, iph, cfg->mode, be);

    __u32 old_daddr = iph->daddr;
    __u32 new_daddr = be->addr;
    csum_replace(&iph->check, &old_daddr, &new_daddr, 4);
    iph->daddr = new_daddr;
    return XDP_PASS;
}

/* An ICMP error addressed to a VIP (typically "fragmentation needed", i.e. path-MTU
 * discovery, or "time exceeded") quotes the packet that provoked it: the reply the backend
 * sent, which left as VIP:vport -> client:cport. Without help it lands on whichever node owns
 * the VIP, not on the backend whose connection it is about, so that backend never learns the
 * path MTU. Recover the connection from the quoted header and send the error to its backend:
 * unchanged under DSR (the backend owns the VIP), and under full-NAT with the destination
 * rewritten and the quoted source rewritten to match (VIP:vport -> backend:bport), so the
 * backend sees a packet it actually sent. Anything unrecognised passes untouched. */
static __always_inline int handle_v4_icmp(struct xdp_md *ctx, void *data, void *data_end,
                                          struct ethhdr *eth, struct iphdr *iph)
{
    if (iph->ihl != 5)
        return XDP_PASS;
    struct rivora_icmp *ic = (void *)(iph + 1);
    if ((void *)(ic + 1) > data_end)
        return XDP_PASS;
    if (ic->type != RIVORA_ICMP_DEST_UNREACH && ic->type != RIVORA_ICMP_TIME_EXCEEDED &&
        ic->type != RIVORA_ICMP_PARAM_PROB)
        return XDP_PASS;

    struct iphdr *in = (void *)(ic + 1);
    if ((void *)(in + 1) > data_end)
        return XDP_PASS;
    if (in->ihl != 5 || (in->protocol != IPPROTO_TCP_ && in->protocol != IPPROTO_UDP_))
        return XDP_PASS;
    if (in->saddr != iph->daddr) /* the quoted packet must have left from the VIP this is addressed to */
        return XDP_PASS;
    __u16 *ports = (void *)(in + 1);
    if ((void *)(ports + 2) > data_end)
        return XDP_PASS;
    __u16 in_sport = ports[0]; /* the VIP's port */
    __u16 in_dport = ports[1]; /* the client's port */

    __u32 *sid = v4_service(in->saddr, in_sport, in->protocol);
    if (!sid)
        return XDP_PASS;
    struct service_config *cfg = bpf_map_lookup_elem(&service_config_map, sid);
    if (!cfg)
        return XDP_PASS;

    /* The connection is keyed by the request direction: client -> VIP. */
    struct conn_key ck = {
        .saddr = in->daddr, .daddr = in->saddr,
        .sport = in_dport, .dport = in_sport, .proto = in->protocol,
    };
    __u32 *bid = bpf_map_lookup_elem(&connection_affinity_map, &ck);
    if (!bid)
        return XDP_PASS;
    __u32 backend_id = *bid;
    __u8 *healthy = bpf_map_lookup_elem(&backend_health_map, &backend_id);
    if (!healthy || !*healthy)
        return XDP_PASS;
    struct backend_info *be = bpf_map_lookup_elem(&backend_map, &backend_id);
    if (!be)
        return XDP_PASS;

    bump_stats(RIVORA_STATS_GLOBAL, (__u32)(data_end - data), 0);
    bump_stats(1 + backend_id, (__u32)(data_end - data), 0);

    if (cfg->mode != RIVORA_MODE_NAT)
        return forward_v4(ctx, eth, iph, cfg->mode, be);

    /* Full NAT. Re-derive every pointer fresh next to its write. */
    __u32 vip_addr = in->saddr;
    __u32 be_addr = be->addr;
    __u16 new_sport = (cfg->flags & RIVORA_SVC_RANGE) ? in_sport : be->port;

    if ((void *)(iph + 1) > data_end)
        return XDP_PASS;
    csum_replace(&iph->check, &vip_addr, &be_addr, 4); /* the outer header: destination */
    iph->daddr = be_addr;

    struct rivora_icmp *ic2 = (void *)(iph + 1);
    if ((void *)(ic2 + 1) > data_end)
        return XDP_PASS;
    struct iphdr *in2 = (void *)(ic2 + 1);
    if ((void *)(in2 + 1) > data_end)
        return XDP_PASS;
    __u16 *ports2 = (void *)(in2 + 1);
    if ((void *)(ports2 + 2) > data_end)
        return XDP_PASS;

    /* The ICMP checksum covers the quoted packet, so every field changed inside it,
     * including the quoted IP header's own checksum, is folded into it. */
    __u16 old_in_check = in2->check;
    csum_replace(&in2->check, &vip_addr, &be_addr, 4);
    __u16 new_in_check = in2->check;
    csum_replace(&ic2->checksum, &vip_addr, &be_addr, 4);
    csum_replace(&ic2->checksum, &old_in_check, &new_in_check, 2);
    if (new_sport != in_sport) {
        csum_replace(&ic2->checksum, &in_sport, &new_sport, 2);
        ports2[0] = new_sport;
    }
    in2->saddr = be_addr;
    return XDP_PASS;
}

/* IPv4 path — split out of rivora_xdp_ingress unchanged (a verbatim move,
 * not a rewrite) so the v6 branch below can sit alongside it as its own
 * named function instead of growing one already-long function further. */
static __always_inline int handle_ipv4(struct xdp_md *ctx, void *data, void *data_end, struct ethhdr *eth,
                                       struct iphdr *iph)
{
    if ((void *)(iph + 1) > data_end)
        return XDP_PASS;
    if (iph->ihl < 5) /* malformed; options (ihl > 5) are fine, the L4 header is found via ihl */
        return XDP_PASS;

    /* Fragments: the first one (offset 0, MF set) carries the L4 header and is balanced like
     * any packet, then remembered; later ones (offset > 0) have no L4 header and follow it. */
    __u16 frag = iph->frag_off;
    __u8 first_fragment = 0;
    if (frag & __constant_htons(RIVORA_IP_OFFSET))
        return handle_v4_later_fragment(ctx, data, data_end, eth, iph);
    if (frag & __constant_htons(RIVORA_IP_MF))
        first_fragment = 1;

    if (iph->protocol == IPPROTO_ICMP_)
        return handle_v4_icmp(ctx, data, data_end, eth, iph);
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

    __u32 *service_id = v4_service(iph->daddr, dport, iph->protocol);
    if (!service_id)
        return XDP_PASS; /* not a VIP we own */
    __u32 sid = *service_id;

    /* SYN-flood mitigation: only new-connection attempts (SYN packets)
     * consume a token — established connections' data packets and all UDP
     * traffic are untouched regardless of this limiter's state. A no-op,
     * one-array-lookup cost when rl_config_map's enabled bit is unset
     * (the default). */
    if (is_syn && rate_limit_exceeded(sid, iph->saddr)) {
        bump_stats(RIVORA_STATS_GLOBAL, 0, 1);
        bump_drop(sid, RIVORA_DROP_RATE_LIMITED);
        return XDP_DROP;
    }

    struct service_config *cfg = bpf_map_lookup_elem(&service_config_map, service_id);
    if (!cfg || cfg->backend_count == 0 || cfg->maglev_size == 0) {
        bump_drop(sid, RIVORA_UNSERVED);
        return XDP_PASS;
    }

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
        __u32 hash = cfg->affinity == RIVORA_AFFINITY_CLIENT_IP
                         ? rivora_hash_src(iph->saddr)
                         : rivora_hash5(iph->saddr, iph->daddr, sport, dport, iph->protocol);
        __u32 local_slot = hash % cfg->maglev_size;
        if (pick_backend(cfg->maglev_offset, cfg->maglev_size, local_slot, &backend_id) < 0) {
            bump_stats(RIVORA_STATS_GLOBAL, 0, 1);
            bump_drop(sid, RIVORA_DROP_NO_BACKEND);
            return XDP_DROP; /* no healthy backend within probe window */
        }
        bpf_map_update_elem(&connection_affinity_map, &ck, &backend_id, BPF_ANY);
    }

    struct backend_info *be = bpf_map_lookup_elem(&backend_map, &backend_id);
    if (!be) {
        bump_drop(sid, RIVORA_UNSERVED);
        return XDP_PASS;
    }

    __u32 pkt_len = (__u32)(data_end - data);
    bump_stats(RIVORA_STATS_GLOBAL, pkt_len, 0);
    bump_stats(1 + backend_id, pkt_len, 0);

    /* The first fragment of a fragmented datagram: remember its backend, keyed by the
     * ORIGINAL addresses (before any NAT rewrite), so the later fragments follow it. */
    if (first_fragment) {
        struct frag_key fk = {.saddr = iph->saddr, .daddr = iph->daddr, .id = iph->id, .proto = iph->protocol};
        struct frag_val fv = {.backend_id = backend_id, .service_id = sid};
        bpf_map_update_elem(&frag_map, &fk, &fv, BPF_ANY);
    }

    if (cfg->mode != RIVORA_MODE_NAT) {
        /* DSR / L3 DSR: the VIP stays the destination IP — the backend must have the VIP
         * bound locally (loopback/dummy) so it accepts and replies directly. */
        return forward_v4(ctx, eth, iph, cfg->mode, be);
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
    /* A range VIP keeps the client's port; a single-port VIP goes to the backend's. */
    __u16 new_dport = (cfg->flags & RIVORA_SVC_RANGE) ? dport : be->port;
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
        .backend_port = new_dport, .client_port = sport, .proto = iph->protocol,
    };
    struct nat_reverse_val rv = {.vip_addr = old_daddr, .vip_port = old_dport};
    bpf_map_update_elem(&nat_reverse_map, &rk, &rv, BPF_ANY);

    /* Let the kernel's normal routing deliver to the (now real) backend
     * address — simplest correct path for v0.1's full-NAT mode. */
    return XDP_PASS;
}

/* IPv6 sibling of handle_v4_icmp: Packet Too Big (path-MTU discovery, which IPv6 relies on
 * far more than IPv4 does since routers never fragment) and the other errors that quote the
 * offending packet. Differences from v4: the checksum of an ICMPv6 message includes a
 * pseudo-header of the outer addresses, so rewriting the outer destination changes it too, and
 * there is no IP header checksum inside the quoted packet. */
static __always_inline int handle_v6_icmp(struct xdp_md *ctx, void *data, void *data_end,
                                          struct ethhdr *eth, struct ipv6hdr *iph)
{
    struct rivora_icmp *ic = (void *)(iph + 1);
    if ((void *)(ic + 1) > data_end)
        return XDP_PASS;
    if (ic->type != RIVORA_ICMP6_DEST_UNREACH && ic->type != RIVORA_ICMP6_PKT_TOO_BIG &&
        ic->type != RIVORA_ICMP6_TIME_EXCEEDED && ic->type != RIVORA_ICMP6_PARAM_PROB)
        return XDP_PASS;

    struct ipv6hdr *in = (void *)(ic + 1);
    if ((void *)(in + 1) > data_end)
        return XDP_PASS;
    if (in->nexthdr != IPPROTO_TCP_ && in->nexthdr != IPPROTO_UDP_)
        return XDP_PASS;
    if (__builtin_memcmp(&in->saddr, &iph->daddr, 16) != 0) /* must have left from the VIP this is addressed to */
        return XDP_PASS;
    __u16 *ports = (void *)(in + 1);
    if ((void *)(ports + 2) > data_end)
        return XDP_PASS;
    __u16 in_sport = ports[0];
    __u16 in_dport = ports[1];

    __u32 *sid = v6_service((__u8 *)&in->saddr, in_sport, in->nexthdr);
    if (!sid)
        return XDP_PASS;
    struct service_config *cfg = bpf_map_lookup_elem(&service_config_map, sid);
    if (!cfg)
        return XDP_PASS;

    struct conn_key6 ck = {.sport = in_dport, .dport = in_sport, .proto = in->nexthdr};
    __builtin_memcpy(ck.saddr, &in->daddr, 16);
    __builtin_memcpy(ck.daddr, &in->saddr, 16);
    __u32 *bid = bpf_map_lookup_elem(&connection_affinity_map6, &ck);
    if (!bid)
        return XDP_PASS;
    __u32 backend_id = *bid;
    __u8 *healthy = bpf_map_lookup_elem(&backend_health_map, &backend_id);
    if (!healthy || !*healthy)
        return XDP_PASS;
    struct backend_info6 *be = bpf_map_lookup_elem(&backend_map6, &backend_id);
    if (!be)
        return XDP_PASS;

    bump_stats(RIVORA_STATS_GLOBAL, (__u32)(data_end - data), 0);
    bump_stats(1 + backend_id, (__u32)(data_end - data), 0);

    if (cfg->mode != RIVORA_MODE_NAT)
        return forward_v6(ctx, eth, iph, cfg->mode, be);

    __u8 vip_addr[16], be_addr[16];
    __builtin_memcpy(vip_addr, &in->saddr, 16);
    __builtin_memcpy(be_addr, be->addr, 16);
    __u16 new_sport = (cfg->flags & RIVORA_SVC_RANGE) ? in_sport : be->port;

    if ((void *)(iph + 1) > data_end)
        return XDP_PASS;
    struct rivora_icmp *ic2 = (void *)(iph + 1);
    if ((void *)(ic2 + 1) > data_end)
        return XDP_PASS;
    struct ipv6hdr *in2 = (void *)(ic2 + 1);
    if ((void *)(in2 + 1) > data_end)
        return XDP_PASS;
    __u16 *ports2 = (void *)(in2 + 1);
    if ((void *)(ports2 + 2) > data_end)
        return XDP_PASS;

    /* Outer destination (pseudo-header) and quoted source (payload) both fold into the checksum. */
    csum_replace(&ic2->checksum, vip_addr, be_addr, 16);
    csum_replace(&ic2->checksum, vip_addr, be_addr, 16);
    if (new_sport != in_sport) {
        csum_replace(&ic2->checksum, &in_sport, &new_sport, 2);
        ports2[0] = new_sport;
    }
    __builtin_memcpy(&iph->daddr, be_addr, 16);
    __builtin_memcpy(&in2->saddr, be_addr, 16);
    return XDP_PASS;
}

/* IPv6 path. Structurally mirrors handle_ipv4 closely (same VIP-match ->
 * rate-limit -> connection-affinity/Maglev -> DSR-or-NAT shape), reusing
 * every address-family-agnostic map (service_config_map, maglev_table,
 * backend_health_map, stats_map, iface_mac_map, rl_config_map,
 * pick_backend()) as-is — only the address-keyed maps and a few checksum
 * details differ from v4, noted inline below. */
static __always_inline int handle_ipv6(struct xdp_md *ctx, void *data, void *data_end, struct ethhdr *eth,
                                       struct ipv6hdr *iph)
{
    if ((void *)(iph + 1) > data_end)
        return XDP_PASS;
    if (iph->nexthdr == IPPROTO_ICMPV6_)
        return handle_v6_icmp(ctx, data, data_end, eth, iph);
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

    __u32 *service_id = v6_service((__u8 *)&iph->daddr, dport, iph->nexthdr);
    if (!service_id)
        return XDP_PASS; /* not a VIP we own */
    __u32 sid = *service_id;

    if (is_syn && rate_limit_exceeded_v6(sid, (__u8 *)&iph->saddr)) {
        bump_stats(RIVORA_STATS_GLOBAL, 0, 1);
        bump_drop(sid, RIVORA_DROP_RATE_LIMITED);
        return XDP_DROP;
    }

    struct service_config *cfg = bpf_map_lookup_elem(&service_config_map, service_id);
    if (!cfg || cfg->backend_count == 0 || cfg->maglev_size == 0) {
        bump_drop(sid, RIVORA_UNSERVED);
        return XDP_PASS;
    }

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
        __u32 hash = cfg->affinity == RIVORA_AFFINITY_CLIENT_IP
                         ? rivora_hash_src_v6((__u8 *)&iph->saddr)
                         : rivora_hash5_v6((__u8 *)&iph->saddr, (__u8 *)&iph->daddr, sport, dport, iph->nexthdr);
        __u32 local_slot = hash % cfg->maglev_size;
        if (pick_backend(cfg->maglev_offset, cfg->maglev_size, local_slot, &backend_id) < 0) {
            bump_stats(RIVORA_STATS_GLOBAL, 0, 1);
            bump_drop(sid, RIVORA_DROP_NO_BACKEND);
            return XDP_DROP; /* no healthy backend within probe window */
        }
        bpf_map_update_elem(&connection_affinity_map6, &ck, &backend_id, BPF_ANY);
    }

    struct backend_info6 *be = bpf_map_lookup_elem(&backend_map6, &backend_id);
    if (!be) {
        bump_drop(sid, RIVORA_UNSERVED);
        return XDP_PASS;
    }

    __u32 pkt_len = (__u32)(data_end - data);
    bump_stats(RIVORA_STATS_GLOBAL, pkt_len, 0);
    bump_stats(1 + backend_id, pkt_len, 0);

    if (cfg->mode != RIVORA_MODE_NAT) {
        /* DSR / L3 DSR: the VIP stays the destination IP — the backend must have the VIP
         * bound locally so it accepts and replies directly. */
        return forward_v6(ctx, eth, iph, cfg->mode, be);
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
    __u16 new_dport = (cfg->flags & RIVORA_SVC_RANGE) ? dport : be->port;
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

    struct nat_reverse_key6 rk = {.backend_port = new_dport, .client_port = sport, .proto = iph->nexthdr};
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

    /* Step over up to two VLAN tags (802.1Q, or QinQ). A DSR rewrite only touches the MACs, so
     * the tags go through unchanged. */
    void *l3;
    __be16 proto;
    if (rivora_skip_vlan(eth + 1, eth->h_proto, data_end, &l3, &proto) < 0)
        return XDP_PASS;

    if (proto == __constant_htons(ETH_P_IP))
        return handle_ipv4(ctx, data, data_end, eth, l3);
    if (proto == __constant_htons(ETH_P_IPV6))
        return handle_ipv6(ctx, data, data_end, eth, l3);
    return XDP_PASS;
}

char _license[] SEC("license") = "GPL";
