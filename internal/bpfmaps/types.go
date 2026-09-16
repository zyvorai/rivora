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
