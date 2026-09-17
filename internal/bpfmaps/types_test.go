// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package bpfmaps

import (
	"testing"
	"unsafe"
)

// TestStructSizesMatchCABI guards the invariant this package's doc comments
// state but don't enforce: each Go struct's memory layout must exactly
// match its C counterpart in bpf/rivora_common.h, since cilium/ebpf
// marshals map keys/values by raw memory layout, not by field name. A
// silently reordered or resized field here would corrupt every read/write
// against the live BPF maps without any compiler error to catch it.
func TestStructSizesMatchCABI(t *testing.T) {
	cases := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"VipKey", unsafe.Sizeof(VipKey{}), 8},
		{"ServiceConfig", unsafe.Sizeof(ServiceConfig{}), 16},
		{"BackendInfo", unsafe.Sizeof(BackendInfo{}), 12},
		{"ConnKey", unsafe.Sizeof(ConnKey{}), 16},
		{"NATReverseKey", unsafe.Sizeof(NATReverseKey{}), 16},
		{"NATReverseVal", unsafe.Sizeof(NATReverseVal{}), 8},
		{"LBStats", unsafe.Sizeof(LBStats{}), 24},
		{"RLConfig", unsafe.Sizeof(RLConfig{}), 24},
		{"RLBucket", unsafe.Sizeof(RLBucket{}), 16},
		{"VipKey6", unsafe.Sizeof(VipKey6{}), 20},
		{"BackendInfo6", unsafe.Sizeof(BackendInfo6{}), 24},
		{"ConnKey6", unsafe.Sizeof(ConnKey6{}), 40},
		{"NATReverseKey6", unsafe.Sizeof(NATReverseKey6{}), 40},
		{"NATReverseVal6", unsafe.Sizeof(NATReverseVal6{}), 20},
		{"Addr6Key", unsafe.Sizeof(Addr6Key{}), 16},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("unsafe.Sizeof(%s{}) = %d, want %d (doc comment; must match struct %s in bpf/rivora_common.h)", c.name, c.got, c.want, c.name)
			}
		})
	}
}

// TestMaxCapacitiesArePositive guards against a future edit accidentally
// zeroing a compiled-in map capacity, which the ID allocators
// (internal/dataplane) trust as their ceiling before ever touching the
// kernel maps.
func TestMaxCapacitiesArePositive(t *testing.T) {
	if MaxVIPs <= 0 {
		t.Error("MaxVIPs must be positive")
	}
	if MaxBackends <= 0 {
		t.Error("MaxBackends must be positive")
	}
}

// TestHealthValuesAreDistinct guards the ordering pick_backend() in
// bpf/xdp_ingress.c depends on (== HealthHealthy for new-flow selection).
func TestHealthValuesAreDistinct(t *testing.T) {
	values := map[uint8]string{
		HealthDown:     "down",
		HealthHealthy:  "healthy",
		HealthDraining: "draining",
	}
	if len(values) != 3 {
		t.Errorf("HealthDown/HealthHealthy/HealthDraining must be pairwise distinct, got %v", values)
	}
}
