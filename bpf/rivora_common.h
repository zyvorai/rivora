// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
//
// Shared BPF-side helpers for Rivora's XDP/TCX programs. Deliberately avoids
// libbpf's bpf_helpers.h / CO-RE (BTF relocations) so the programs build with
// nothing beyond the kernel uapi headers and clang — same approach as
// netra's bpf/netra_tc.c in the sibling repo.
#ifndef RIVORA_COMMON_H
#define RIVORA_COMMON_H

#include <linux/bpf.h>
#include <linux/types.h>

#ifndef NULL
#define NULL ((void *)0)
#endif

#define SEC(NAME) __attribute__((section(NAME), used))
#define __uint(name, val) int (*name)[val]
#define __type(name, val) val *name

/* clang rejects non-inlined helpers with many args on the BPF target. */
#undef __always_inline
#define __always_inline inline __attribute__((always_inline))

static void *(*bpf_map_lookup_elem)(void *map, const void *key) = (void *)BPF_FUNC_map_lookup_elem;
static long (*bpf_map_update_elem)(void *map, const void *key, const void *value, __u64 flags) = (void *)BPF_FUNC_map_update_elem;
static long (*bpf_map_delete_elem)(void *map, const void *key) = (void *)BPF_FUNC_map_delete_elem;
static __u64 (*bpf_ktime_get_ns)(void) = (void *)BPF_FUNC_ktime_get_ns;
static __s64 (*bpf_csum_diff)(__be32 *from, __u32 from_size, __be32 *to, __u32 to_size, __u32 seed) = (void *)BPF_FUNC_csum_diff;
static long (*bpf_xdp_adjust_head)(struct xdp_md *ctx, int delta) = (void *)BPF_FUNC_xdp_adjust_head;
static long (*bpf_fib_lookup)(void *ctx, struct bpf_fib_lookup *params, int plen, __u32 flags) = (void *)BPF_FUNC_fib_lookup;
static long (*bpf_redirect)(__u32 ifindex, __u64 flags) = (void *)BPF_FUNC_redirect;
static __u32 (*bpf_get_prandom_u32)(void) = (void *)BPF_FUNC_get_prandom_u32;

#define IPPROTO_ICMP_ 1
#define IPPROTO_TCP_ 6
#define IPPROTO_UDP_ 17
#define IPPROTO_ICMPV6_ 58

/* IPv4 flags/offset field (network order): MF and the fragment offset. */
#define RIVORA_IP_MF     0x2000
#define RIVORA_IP_OFFSET 0x1fff

/* ICMP / ICMPv6 error types that quote the packet that caused them. */
#define RIVORA_ICMP_DEST_UNREACH  3  /* includes "fragmentation needed" (code 4), i.e. PMTUD */
#define RIVORA_ICMP_TIME_EXCEEDED 11
#define RIVORA_ICMP_PARAM_PROB    12
#define RIVORA_ICMP6_DEST_UNREACH 1
#define RIVORA_ICMP6_PKT_TOO_BIG  2  /* IPv6 PMTUD */
#define RIVORA_ICMP6_TIME_EXCEEDED 3
#define RIVORA_ICMP6_PARAM_PROB   4

/* The 8-byte header of an ICMP or ICMPv6 message (type, code, checksum, 4 bytes that vary by
 * type). Declared here rather than taken from <linux/icmp.h>, which pulls in libc headers
 * this freestanding BPF build does not have. */
struct rivora_icmp {
    __u8    type;
    __u8    code;
    __sum16 checksum;
    __u32   rest;
};
_Static_assert(sizeof(struct rivora_icmp) == 8, "icmp header");

/* 802.1Q / 802.1ad tags between the Ethernet header and the network header. */
#define RIVORA_ETH_P_8021Q  0x8100
#define RIVORA_ETH_P_8021AD 0x88a8
struct rivora_vlan_hdr {
    __be16 tci;
    __be16 encap_proto;
};

/* Skip up to two VLAN tags (802.1Q, or 802.1ad + 802.1Q for QinQ) after the Ethernet
 * header. On success *l3 points at the network header and *proto is its EtherType
 * (network order). Fails only if a tag is cut short. Unrolled: the verifier will not
 * prove an open-ended loop terminates. */
static __always_inline int rivora_skip_vlan(void *l2_payload, __be16 ethertype, void *data_end,
                                            void **l3, __be16 *proto)
{
    void *p = l2_payload;
    __be16 t = ethertype;
#pragma unroll
    for (int i = 0; i < 2; i++) {
        if (t != __constant_htons(RIVORA_ETH_P_8021Q) && t != __constant_htons(RIVORA_ETH_P_8021AD))
            break;
        struct rivora_vlan_hdr *vh = p;
        if ((void *)(vh + 1) > data_end)
            return -1;
        t = vh->encap_proto;
        p = vh + 1;
    }
    *l3 = p;
    *proto = t;
    return 0;
}

/* Rivora ABI structs, mirrored byte-for-byte in internal/bpfmaps (Go). */

struct vip_key {
    __u32 addr;  /* network byte order */
    __u16 port;  /* network byte order */
    __u8  proto; /* IPPROTO_TCP / IPPROTO_UDP */
    __u8  pad;
};
_Static_assert(sizeof(struct vip_key) == 8, "vip_key ABI");

struct service_config {
    __u32 backend_count;
    __u32 maglev_offset; /* this VIP's slice of the shared maglev_table starts here */
    __u32 maglev_size;   /* ...and is this many slots wide — hash must be
                           * reduced mod *this*, not mod the full table, or
                           * it lands outside the VIP's populated range and
                           * reads zero-initialized (backend_id=0) slots */
    __u8  mode;           /* 0 = DSR, 1 = full NAT */
    __u8  affinity;       /* RIVORA_AFFINITY_*: what the Maglev slot is chosen from */
    __u8  flags;          /* RIVORA_SVC_* */
    __u8  pad;
};
_Static_assert(sizeof(struct service_config) == 16, "service_config ABI");

/* service_config.flags. RANGE marks a VIP that owns a whole port range: the destination
 * port is not rewritten (a backend is reached on the same port the client used), so
 * the backend's own port in backend_info is ignored. */
#define RIVORA_SVC_RANGE 0x1

struct backend_info {
    __u32 addr;   /* network byte order */
    __u16 port;   /* network byte order */
    __u8  mac[6];
};
_Static_assert(sizeof(struct backend_info) == 12, "backend_info ABI");

struct conn_key {
    __u32 saddr;
    __u32 daddr;
    __u16 sport;
    __u16 dport;
    __u8  proto;
    __u8  pad[3];
};
_Static_assert(sizeof(struct conn_key) == 16, "conn_key ABI");

struct nat_reverse_key {
    __u32 backend_addr;
    __u32 client_addr;
    __u16 backend_port;
    __u16 client_port;
    __u8  proto;
    __u8  pad[3];
};
_Static_assert(sizeof(struct nat_reverse_key) == 16, "nat_reverse_key ABI");

struct nat_reverse_val {
    __u32 vip_addr;
    __u16 vip_port;
    __u8  pad[2];
};
_Static_assert(sizeof(struct nat_reverse_val) == 8, "nat_reverse_val ABI");

struct mac_addr {
    __u8 addr[6];
};
_Static_assert(sizeof(struct mac_addr) == 6, "mac_addr ABI");

/* IPv6 siblings of vip_key/backend_info/conn_key/nat_reverse_key/val above.
 * Only the address-keyed-or-valued maps need a v6 sibling at all —
 * service_config_map, maglev_table, backend_health_map, stats_map,
 * iface_mac_map and rl_config_map are keyed/valued purely by opaque
 * service_id/backend_id (or, for rl_config_map, nothing address-shaped),
 * so a v4 and a v6 VIP share the exact same instances of those maps, the
 * same ID allocators (Go-side), and the same maglev_table — see
 * pick_backend() and internal/maglev, both already address-family
 * agnostic. Don't add v6 siblings for those; only vip_map, backend_map,
 * connection_affinity_map, nat_reverse_map and rl_buckets_map get one. */

struct vip_key6 {
    __u8  addr[16]; /* network byte order */
    __u16 port;     /* network byte order */
    __u8  proto;    /* IPPROTO_TCP / IPPROTO_UDP */
    __u8  pad;
};
_Static_assert(sizeof(struct vip_key6) == 20, "vip_key6 ABI");

/* A VIP that owns a port range is kept in an LPM trie, because a range is not a key. The
 * trie's key is (address, protocol, port) with a prefix length that covers the address,
 * protocol, pad and the leading bits of the port; a range decomposes into a handful of
 * aligned blocks (30000-30100 is five), each one entry, and a full-length lookup returns
 * the block containing the port. The port is in network byte order, so its high bits come
 * first, which is what the trie's bit ordering needs. Exact ports stay in vip_map and are
 * checked first, so a single port can be carved out of a range. */
struct vip_range_key {
    __u32 prefixlen; /* 48 + (0..16 bits of the port) */
    __u32 addr;      /* network byte order */
    __u8  proto;
    __u8  pad;       /* always 0: part of the bits the prefix covers */
    __u16 port;      /* network byte order */
};
_Static_assert(sizeof(struct vip_range_key) == 12, "vip_range_key ABI");
#define RIVORA_RANGE_FULL_PREFIX_V4 64u

struct backend_info6 {
    __u8  addr[16]; /* network byte order */
    __u16 port;     /* network byte order */
    __u8  mac[6];
};
_Static_assert(sizeof(struct backend_info6) == 24, "backend_info6 ABI");

struct vip_range_key6 {
    __u32 prefixlen; /* 144 + (0..16 bits of the port) */
    __u8  addr[16];  /* network byte order */
    __u8  proto;
    __u8  pad;
    __u16 port;      /* network byte order */
};
_Static_assert(sizeof(struct vip_range_key6) == 24, "vip_range_key6 ABI");
#define RIVORA_RANGE_FULL_PREFIX_V6 160u

struct conn_key6 {
    __u8  saddr[16];
    __u8  daddr[16];
    __u16 sport;
    __u16 dport;
    __u8  proto;
    __u8  pad[3];
};
_Static_assert(sizeof(struct conn_key6) == 40, "conn_key6 ABI");

struct nat_reverse_key6 {
    __u8  backend_addr[16];
    __u8  client_addr[16];
    __u16 backend_port;
    __u16 client_port;
    __u8  proto;
    __u8  pad[3];
};
_Static_assert(sizeof(struct nat_reverse_key6) == 40, "nat_reverse_key6 ABI");

struct nat_reverse_val6 {
    __u8  vip_addr[16];
    __u16 vip_port;
    __u8  pad[2];
};
_Static_assert(sizeof(struct nat_reverse_val6) == 20, "nat_reverse_val6 ABI");

/* A bare 16-byte address, wrapped in a struct because __type()'s
 * `val *name` expansion can't take a raw array type directly — used as
 * rl_buckets_map6's key (the v6 sibling of rl_buckets_map's plain __u32
 * source-address key). */
struct addr6_key {
    __u8 addr[16];
};
_Static_assert(sizeof(struct addr6_key) == 16, "addr6_key ABI");

/* IPv4 fragment tracking. Only the first fragment of a datagram carries the L4 header,
 * so only it can be matched to a VIP and given a backend; the later fragments carry just
 * an IP header. The first fragment records (saddr, daddr, ip id, proto) -> backend here,
 * and each later fragment of the same datagram looks that up and follows it. Fragments
 * that arrive before their first fragment are not steered (they take the normal PASS
 * path); the datagram is then lost, as it would be with any stateless per-packet balancer.
 * LRU, so an abandoned datagram's entry ages out on its own. */
struct frag_key {
    __u32 saddr;
    __u32 daddr;
    __u16 id;
    __u8  proto;
    __u8  pad;
};
_Static_assert(sizeof(struct frag_key) == 12, "frag_key ABI");

struct frag_val {
    __u32 backend_id;
    __u32 service_id;
};
_Static_assert(sizeof(struct frag_val) == 8, "frag_val ABI");

/* The reply direction of full-NAT: a backend's fragmented reply has its source rewritten
 * to the VIP on egress (tc_nat). The first reply fragment does that from nat_reverse_map
 * and records (backend, client, id, proto) -> VIP here so its later fragments get the
 * same rewrite. Shared between xdp_ingress.o and tc_nat.o by name, like nat_reverse_map. */
struct frag_rev_val {
    __u32 vip_addr;
    __u32 pad;
};
_Static_assert(sizeof(struct frag_rev_val) == 8, "frag_rev_val ABI");

struct lb_stats {
    __u64 packets;
    __u64 bytes;
    __u64 dropped;
};
_Static_assert(sizeof(struct lb_stats) == 24, "lb_stats ABI");

/* Why a packet for a matched VIP did not reach a backend, counted per service
 * (indexed by service_id) in drop_stats_map so an operator can tell a SYN flood
 * apart from a VIP with no healthy backends, and see which VIP it is. Both
 * drop sites run after the VIP lookup, so the service is always known. Mirrors
 * bpfmaps.Drop* on the Go side; append new reasons at the end and bump
 * RIVORA_DROP_REASONS together with struct drop_stats and its Go mirror. */
#define RIVORA_DROP_RATE_LIMITED 0 /* XDP_DROP: SYN over the per-source rate limit */
#define RIVORA_DROP_NO_BACKEND   1 /* XDP_DROP: no healthy backend in the probe window */
#define RIVORA_UNSERVED          2 /* XDP_PASS, not a drop: VIP matched but can't be
                                    * served (no service config / backend entry), so
                                    * the packet bypasses the load balancer */
#define RIVORA_DROP_REASONS      3

struct drop_stats {
    __u64 by_reason[RIVORA_DROP_REASONS];
};
_Static_assert(sizeof(struct drop_stats) == 24, "drop_stats ABI");

/* rl_config_map is a single-entry array: an opt-in on/off switch plus the
 * per-source-IP SYN token-bucket's rate/burst, written once at startup by
 * internal/dataplane. rate_per_sec/burst are already pre-divided by
 * runtime.NumCPU() on the Go side — see rl_bucket's doc comment for why. */
struct rl_config {
    __u64 rate_per_sec;
    __u64 burst;
    __u8  enabled;
    __u8  pad[7];
};
_Static_assert(sizeof(struct rl_config) == 24, "rl_config ABI");

/* rl_buckets_map is BPF_MAP_TYPE_LRU_PERCPU_HASH keyed by source IP: each
 * CPU keeps an independent bucket for the same source, so there's no
 * cross-CPU contention/locking (same non-atomic, lock-free style
 * bump_stats already uses for stats_map) at the cost of the *effective*
 * global rate being roughly rate_per_sec times however many CPUs are
 * processing that source's traffic — rl_config's rate_per_sec/burst are
 * pre-divided by runtime.NumCPU() to compensate, so the configured value
 * in rivorad's YAML means what it says despite per-CPU enforcement. */
struct rl_bucket {
    __u64 tokens;
    __u64 last_refill_ns;
};
_Static_assert(sizeof(struct rl_bucket) == 16, "rl_bucket ABI");

/* A Service can carry its own SYN rate limit, which replaces the node-wide one for
 * that VIP. Its config lives in svc_rl_config_map, indexed by service_id and shaped
 * like rl_config (enabled == 0 means "use the node-wide limit"). Its buckets are
 * keyed by (service_id, source) so two VIPs never share a client's tokens. */
struct svc_rl_key {
    __u32 service_id;
    __u32 saddr;      /* network byte order */
};
_Static_assert(sizeof(struct svc_rl_key) == 8, "svc_rl_key ABI");

struct svc_rl_key6 {
    __u32 service_id;
    __u8  saddr[16];  /* network byte order */
};
_Static_assert(sizeof(struct svc_rl_key6) == 20, "svc_rl_key6 ABI");

/* Beyond this, a bucket is treated as fully refilled regardless of its
 * configured rate — caps the refill multiply below against overflow
 * without needing to reason about how long a source IP has been idle
 * (which, for a long-uptime LB, could otherwise be an arbitrarily large
 * nanosecond count). Any sane rate/burst combination refills in well
 * under 10s anyway. */
#define RIVORA_RL_MAX_ELAPSED_NS 10000000000ULL

/* One token-bucket step: refill cur (a fresh, full bucket when NULL) for the time
 * since it was last touched, take one token if there is one, write the result to
 * out, and report whether the bucket was empty. Shared by the node-wide and the
 * per-service limiters and by their IPv4 and IPv6 forms, so the maths exists once. */
static __always_inline int rl_take(const struct rl_config *cfg, const struct rl_bucket *cur,
                                   struct rl_bucket *out, __u64 now)
{
    __u64 tokens = cfg->burst;
    __u64 last = now;
    if (cur) {
        tokens = cur->tokens;
        last = cur->last_refill_ns;
    }

    __u64 elapsed_ns = now > last ? now - last : 0;
    if (elapsed_ns > RIVORA_RL_MAX_ELAPSED_NS)
        elapsed_ns = RIVORA_RL_MAX_ELAPSED_NS;

    tokens += (elapsed_ns * cfg->rate_per_sec) / 1000000000ULL;
    if (tokens > cfg->burst)
        tokens = cfg->burst;

    int exceeded = tokens < 1;
    if (!exceeded)
        tokens -= 1;

    out->tokens = tokens;
    out->last_refill_ns = now;
    return exceeded;
}

#define RIVORA_MODE_DSR 0
#define RIVORA_MODE_NAT 1
/* L3 DSR: the packet is wrapped in an IP-in-IP (2) or GRE (3) tunnel to the backend, which
 * unwraps it and answers the client directly from the VIP. Unlike DSR (0) the backend need not
 * be on the load balancer's L2 segment, only routable from it. IPv4 VIPs use an IPv4 outer
 * header and IPv6 VIPs an IPv6 one. */
#define RIVORA_MODE_TUNNEL_IPIP 2
#define RIVORA_MODE_TUNNEL_GRE  3

/* tunnel_config_map: the source address of the outer header, per family, written by rivorad at
 * start-up (configured, or the attached interface's own address). A zero source means "none":
 * a tunnel VIP then drops rather than send a packet nobody can attribute. */
struct tunnel_config {
    __u32 src4;     /* network byte order */
    __u8  src6[16];
};
_Static_assert(sizeof(struct tunnel_config) == 20, "tunnel_config ABI");

/* backend_health_map values — mirrors bpfmaps.Health{Down,Healthy,Draining}
 * on the Go side. Draining excludes a backend from pick_backend()'s *new*
 * flow selection but not from connection_affinity_map's fast path (any
 * nonzero value is truthy there), so established flows keep flowing. */
#define RIVORA_HEALTH_DOWN 0
#define RIVORA_HEALTH_HEALTHY 1
#define RIVORA_HEALTH_DRAINING 2

/* Session affinity, chosen per service. NONE hashes the whole 5-tuple, so a client's
 * connections spread across backends. CLIENT_IP hashes the source address only, so
 * every connection from one client lands on the same backend for as long as the
 * backend set is unchanged (Maglev moves only a minimal share of clients when it
 * does). Mirrors bpfmaps.Affinity* on the Go side. An older pinned service_config
 * has 0 here, which is NONE, so nothing changes on upgrade. */
#define RIVORA_AFFINITY_NONE 0
#define RIVORA_AFFINITY_CLIENT_IP 1

#define RIVORA_MAGLEV_M 65537u
#define RIVORA_MAGLEV_PROBES 8

/* Stats slots: index 0 = global totals, 1..N = per-backend (backend_id+1). */
#define RIVORA_STATS_GLOBAL 0

/* RFC1624-style incremental checksum update for an arbitrary changed field.
 *
 * The fold-carry loop is a fixed 4-iteration unroll, not a `while`: 4
 * iterations is more than enough to fully fold any __s64 sum down to 16
 * bits (each pass can only shrink the high bits), but more importantly the
 * verifier can't prove a `while (sum >> 16)` terminates — its range
 * tracking doesn't reason about convergence, so it reports "infinite loop
 * detected" even though this converges in 2-3 passes in practice.
 *
 * bpf_csum_diff() only accepts sizes that are a multiple of 4: called with 2 it fails with
 * -EINVAL, and using its result unchecked silently subtracts 22 from the checksum. So a
 * 16-bit field (a port, or another checksum) is updated by hand, HC' = ~(~HC + ~m + m'),
 * and only 4- and 16-byte fields (IPv4/IPv6 addresses) go through the helper. `size` is a
 * compile-time constant at every call site, so the branch is resolved at build time. */
static __always_inline void csum_replace(__u16 *csum_be, void *old_val, void *new_val, __u32 size)
{
    if (size == 2) {
        __u32 sum = (~(__u32)(*csum_be) & 0xffff) + (~(__u32)(*(__u16 *)old_val) & 0xffff) + *(__u16 *)new_val;
        sum = (sum & 0xffff) + (sum >> 16);
        sum = (sum & 0xffff) + (sum >> 16);
        *csum_be = (__u16)(~sum & 0xffff);
        return;
    }
    __s64 diff = bpf_csum_diff((__be32 *)old_val, size, (__be32 *)new_val, size, 0);
    if (diff < 0 && diff >= -4095)
        return; /* the helper refused (bad size): leave the checksum alone rather than corrupt it */
    __u32 c = (~(__u32)(*csum_be)) & 0xffff;
    __s64 sum = (__s64)c + diff;
#pragma unroll
    for (int i = 0; i < 4; i++) {
        if (sum >> 16)
            sum = (sum & 0xffff) + (sum >> 16);
    }
    if (sum < 0)
        sum += 0xffff; /* keep the fold in range if diff underflowed */
    *csum_be = (__u16)(~sum & 0xffff);
}

/* Simple, fast, non-cryptographic 5-tuple mix — good enough for Maglev slot
 * selection, not a security boundary. */
static __always_inline __u32 rivora_hash5(__u32 saddr, __u32 daddr, __u16 sport, __u16 dport, __u8 proto)
{
    __u32 h = 2166136261u;
    h = (h ^ saddr) * 16777619u;
    h = (h ^ daddr) * 16777619u;
    h = (h ^ ((__u32)sport << 16 | dport)) * 16777619u;
    h = (h ^ proto) * 16777619u;
    return h;
}

/* IPv6 sibling of rivora_hash5: folds each 128-bit address in as 4 words
 * via a fixed (verifier-friendly) 4-iteration unroll instead of the single
 * XOR/multiply rivora_hash5 does per 32-bit v4 address. */
static __always_inline __u32 rivora_hash5_v6(const __u8 saddr[16], const __u8 daddr[16], __u16 sport, __u16 dport, __u8 proto)
{
    __u32 h = 2166136261u;
    __u32 sw[4], dw[4];
    __builtin_memcpy(sw, saddr, 16);
    __builtin_memcpy(dw, daddr, 16);
#pragma unroll
    for (int i = 0; i < 4; i++) {
        h = (h ^ sw[i]) * 16777619u;
        h = (h ^ dw[i]) * 16777619u;
    }
    h = (h ^ ((__u32)sport << 16 | dport)) * 16777619u;
    h = (h ^ proto) * 16777619u;
    return h;
}

/* murmur3's 32-bit finalizer: a single FNV round on one word is weak in its low
 * bits, and the slot is hash % maglev_size, so consecutive client addresses
 * (10.0.0.1, 10.0.0.2, ...) need real avalanche to spread evenly. */
static __always_inline __u32 rivora_fmix32(__u32 h)
{
    h ^= h >> 16;
    h *= 0x85ebca6bu;
    h ^= h >> 13;
    h *= 0xc2b2ae35u;
    h ^= h >> 16;
    return h;
}

/* Source-address-only hash, for RIVORA_AFFINITY_CLIENT_IP. */
static __always_inline __u32 rivora_hash_src(__u32 saddr)
{
    return rivora_fmix32((2166136261u ^ saddr) * 16777619u);
}

static __always_inline __u32 rivora_hash_src_v6(const __u8 saddr[16])
{
    __u32 h = 2166136261u;
    __u32 sw[4];
    __builtin_memcpy(sw, saddr, 16);
#pragma unroll
    for (int i = 0; i < 4; i++)
        h = (h ^ sw[i]) * 16777619u;
    return rivora_fmix32(h);
}

#endif /* RIVORA_COMMON_H */
