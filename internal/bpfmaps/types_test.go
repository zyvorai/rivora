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
		{"DropStats", unsafe.Sizeof(DropStats{}), 24},
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

// TestServiceConfigFieldOffsetsMatchCABI pins WHERE each ServiceConfig field sits, not
// just the total size. Affinity was added into former padding, so the struct is still
// 16 bytes; if it were at the wrong offset, an older pinned service_config would be
// misread (its mode taken for affinity, or the reverse) and TestStructSizesMatchCABI
// would still pass.
func TestServiceConfigFieldOffsetsMatchCABI(t *testing.T) {
	var sc ServiceConfig
	for name, c := range map[string]struct{ got, want uintptr }{
		"BackendCount": {unsafe.Offsetof(sc.BackendCount), 0},
		"MaglevOffset": {unsafe.Offsetof(sc.MaglevOffset), 4},
		"MaglevSize":   {unsafe.Offsetof(sc.MaglevSize), 8},
		"Mode":         {unsafe.Offsetof(sc.Mode), 12},
		"Affinity":     {unsafe.Offsetof(sc.Affinity), 13},
	} {
		if c.got != c.want {
			t.Errorf("ServiceConfig.%s at offset %d, want %d (struct service_config in bpf/rivora_common.h)", name, c.got, c.want)
		}
	}
	if AffinityNone != 0 {
		t.Error("AffinityNone must be 0: an older pinned service_config has zero there and must mean no affinity")
	}
}
