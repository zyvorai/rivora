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

#define IPPROTO_TCP_ 6
#define IPPROTO_UDP_ 17

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
    __u32 maglev_offset; /* reserved for multi-VIP tables; always 0 in v0.1 */
    __u8  mode;           /* 0 = DSR, 1 = full NAT */
    __u8  pad[3];
};
_Static_assert(sizeof(struct service_config) == 12, "service_config ABI");

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
_Static_assert(sizeof(struct conn_key) == 20, "conn_key ABI");

struct nat_reverse_key {
    __u32 backend_addr;
    __u32 client_addr;
    __u16 backend_port;
    __u16 client_port;
    __u8  proto;
    __u8  pad[3];
};
_Static_assert(sizeof(struct nat_reverse_key) == 20, "nat_reverse_key ABI");

struct nat_reverse_val {
    __u32 vip_addr;
    __u16 vip_port;
    __u8  pad[2];
};
_Static_assert(sizeof(struct nat_reverse_val) == 8, "nat_reverse_val ABI");

struct lb_stats {
    __u64 packets;
    __u64 bytes;
    __u64 dropped;
};
_Static_assert(sizeof(struct lb_stats) == 24, "lb_stats ABI");

#define RIVORA_MODE_DSR 0
#define RIVORA_MODE_NAT 1

#define RIVORA_MAGLEV_M 65537u
#define RIVORA_MAGLEV_PROBES 8

/* Stats slots: index 0 = global totals, 1..N = per-backend (backend_id+1). */
#define RIVORA_STATS_GLOBAL 0

static __always_inline __u16 csum_fold(__u32 sum)
{
    sum = (sum & 0xffff) + (sum >> 16);
    sum = (sum & 0xffff) + (sum >> 16);
    return (__u16)~sum;
}

/* RFC1624-style incremental checksum update for an arbitrary changed field. */
static __always_inline void csum_replace(__u16 *csum_be, void *old_val, void *new_val, __u32 size)
{
    __s64 diff = bpf_csum_diff((__be32 *)old_val, size, (__be32 *)new_val, size, 0);
    __u32 c = (~(__u32)(*csum_be)) & 0xffff;
    __s64 sum = (__s64)c + diff;
    while (sum >> 16)
        sum = (sum & 0xffff) + (sum >> 16);
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

#endif /* RIVORA_COMMON_H */
