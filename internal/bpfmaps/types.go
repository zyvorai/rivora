// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package bpfmaps mirrors the map key/value ABI defined in
// bpf/rivora_common.h. Field order, sizes and padding here must exactly
// match the C structs — cilium/ebpf marshals these via their raw memory
// layout, not by name.
package bpfmaps

const (
	MapVIP                = "vip_map"
	MapServiceConfig      = "service_config_map"
	MapMaglevTable        = "maglev_table"
	MapBackend            = "backend_map"
	MapBackendHealth      = "backend_health_map"
	MapConnectionAffinity = "connection_affinity_map"
	MapNATReverse         = "nat_reverse_map"
	MapIfaceMAC           = "iface_mac_map"
	MapStats              = "stats_map"
	MapRateLimitConfig    = "rl_config_map"
	MapRateLimitBuckets   = "rl_buckets_map"

	// IPv6 siblings (v0.3) of the address-keyed-or-valued maps above.
	// service_config_map, maglev_table, backend_health_map, stats_map,
	// iface_mac_map and rl_config_map are shared as-is between v4 and v6
	// — see bpf/rivora_common.h's comment above the VipKey6/etc. structs
	// below for why.
	MapVIP6                = "vip_map6"
	MapBackend6            = "backend_map6"
	MapConnectionAffinity6 = "connection_affinity_map6"
	MapNATReverse6         = "nat_reverse_map6"
	MapRateLimitBuckets6   = "rl_buckets_map6"

	ProgXDPIngress  = "rivora_xdp_ingress"
	ProgTCNATEgress = "rivora_tc_nat_egress"

	ModeDSR = 0
	ModeNAT = 1

	// backend_health_map values. Draining excludes a backend from *new*
	// Maglev flow selection (pick_backend() in bpf/xdp_ingress.c checks
	// `== HealthHealthy`) while leaving already-established flows alone —
	// connection_affinity_map's fast path only checks non-zero, so both
	// Healthy and Draining keep existing connections flowing.
	HealthDown     = 0
	HealthHealthy  = 1
	HealthDraining = 2

	MaglevM = 65537

	// Map capacities compiled into bpf/xdp_ingress.c — mirrored here so Go
	// code (the ID allocators) can enforce the same ceiling before ever
	// attempting a map write that the kernel would reject.
	MaxVIPs     = 4096 // vip_map / service_config_map max_entries
	MaxBackends = 8192 // backend_map / backend_health_map / stats_map max_entries

	StatsGlobalIdx = 0
)

// VipKey — struct vip_key. 8 bytes.
type VipKey struct {
	Addr  uint32
	Port  uint16
	Proto uint8
	Pad   uint8
}

// ServiceConfig — struct service_config. 16 bytes.
type ServiceConfig struct {
	BackendCount uint32
	MaglevOffset uint32
	MaglevSize   uint32 // must be nonzero and match the extent actually written into maglev_table
	Mode         uint8
	Pad          [3]uint8
}

// BackendInfo — struct backend_info. 12 bytes.
type BackendInfo struct {
	Addr uint32
	Port uint16
	Mac  [6]byte
}

// ConnKey — struct conn_key. 16 bytes.
type ConnKey struct {
	Saddr uint32
	Daddr uint32
	Sport uint16
	Dport uint16
	Proto uint8
	Pad   [3]uint8
}

// NATReverseKey — struct nat_reverse_key. 16 bytes.
type NATReverseKey struct {
	BackendAddr uint32
	ClientAddr  uint32
	BackendPort uint16
	ClientPort  uint16
	Proto       uint8
	Pad         [3]uint8
}

// NATReverseVal — struct nat_reverse_val. 8 bytes.
type NATReverseVal struct {
	VIPAddr uint32
	VIPPort uint16
	Pad     [2]uint8
}

// LBStats — struct lb_stats. 24 bytes. Read as a per-CPU slice and summed.
type LBStats struct {
	Packets uint64
	Bytes   uint64
	Dropped uint64
}

// RLConfig — struct rl_config. 24 bytes. RatePerSec/Burst are already
// pre-divided by runtime.NumCPU() before being written — see
// internal/dataplane's applyRateLimit and rl_bucket's doc comment in
// bpf/rivora_common.h for why.
type RLConfig struct {
	RatePerSec uint64
	Burst      uint64
	Enabled    uint8
	Pad        [7]uint8
}

// RLBucket — struct rl_bucket. 16 bytes. Read/written per-CPU (the map is
// BPF_MAP_TYPE_LRU_PERCPU_HASH) — not meant to be read from Go today, but
// mirrored here for ABI completeness/future observability.
type RLBucket struct {
	Tokens       uint64
	LastRefillNs uint64
}

// VipKey6 — struct vip_key6. 20 bytes. Addr holds the address's raw
// network-order bytes directly (unlike VipKey.Addr's uint32, a 16-byte
// array needs no endian conversion — see internal/dataplane's
// vipKeyBPF6/ip6To16 for why that's simpler than v4's ip4ToBE32 trick).
type VipKey6 struct {
	Addr  [16]byte
	Port  uint16
	Proto uint8
	Pad   uint8
}

// BackendInfo6 — struct backend_info6. 24 bytes.
type BackendInfo6 struct {
	Addr [16]byte
	Port uint16
	Mac  [6]byte
}

// ConnKey6 — struct conn_key6. 40 bytes.
type ConnKey6 struct {
	Saddr [16]byte
	Daddr [16]byte
	Sport uint16
	Dport uint16
	Proto uint8
	Pad   [3]uint8
}

// NATReverseKey6 — struct nat_reverse_key6. 40 bytes.
type NATReverseKey6 struct {
	BackendAddr [16]byte
	ClientAddr  [16]byte
	BackendPort uint16
	ClientPort  uint16
	Proto       uint8
	Pad         [3]uint8
}

// NATReverseVal6 — struct nat_reverse_val6. 20 bytes.
type NATReverseVal6 struct {
	VIPAddr [16]byte
	VIPPort uint16
	Pad     [2]uint8
}

// Addr6Key — struct addr6_key. 16 bytes. rl_buckets_map6's key (the v6
// sibling of rl_buckets_map's plain uint32 source-address key) — wrapped
// in a struct on the C side because __type()'s macro expansion can't take
// a raw array type directly; mirrored here for the same reason cilium/ebpf
// needs a concrete Go type per map, not because Go itself needs the
// wrapper.
type Addr6Key struct {
	Addr [16]byte
}
