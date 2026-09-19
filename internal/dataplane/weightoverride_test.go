// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package dataplane

import (
	"errors"
	"testing"

	"github.com/zyvorai/rivora/internal/bpfmaps"
	"github.com/zyvorai/rivora/internal/config"
)

func vipWith(backends ...config.Backend) config.VIP {
	return config.VIP{Address: "10.0.0.1", Port: 80, Protocol: config.ProtoTCP, Backends: backends}
}

func TestApplyWeightOverrides(t *testing.T) {
	a := config.Backend{Address: "10.1.0.1", Port: 8080, Weight: 1}
	b := config.Backend{Address: "10.1.0.2", Port: 8080, Weight: 1}
	spec := vipWith(a, b)

	e := &serviceEntry{weightOverrides: map[string]uint32{backendName(b): 9}}
	got := e.applyWeightOverrides(spec)

	if got.Backends[0].Weight != 1 || got.Backends[1].Weight != 9 {
		t.Errorf("weights = %d,%d; want 1,9", got.Backends[0].Weight, got.Backends[1].Weight)
	}
	// The caller's VIP (a reconciler's object) must never be mutated, or a
	// later "clear override" would have no original weight to restore.
	if spec.Backends[1].Weight != 1 {
		t.Errorf("caller's spec was mutated: weight = %d, want 1", spec.Backends[1].Weight)
	}
}

func TestApplyWeightOverridesNoOverridesReturnsSpecUntouched(t *testing.T) {
	spec := vipWith(config.Backend{Address: "10.1.0.1", Port: 80, Weight: 3})
	got := (&serviceEntry{}).applyWeightOverrides(spec)
	if got.Backends[0].Weight != 3 {
		t.Errorf("weight = %d, want 3", got.Backends[0].Weight)
	}
}

func TestApplyWeightOverridesPrunesDepartedBackends(t *testing.T) {
	a := config.Backend{Address: "10.1.0.1", Port: 8080, Weight: 1}
	gone := config.Backend{Address: "10.1.0.9", Port: 8080, Weight: 1}
	e := &serviceEntry{weightOverrides: map[string]uint32{backendName(a): 4, backendName(gone): 7}}

	e.applyWeightOverrides(vipWith(a))

	if _, stale := e.weightOverrides[backendName(gone)]; stale {
		t.Error("override for a backend no longer in the VIP was kept; it would resurrect if the backend rejoined")
	}
	if e.weightOverrides[backendName(a)] != 4 {
		t.Error("override for a present backend was dropped")
	}
}

func TestHealthStateName(t *testing.T) {
	cases := map[uint8]string{
		bpfmaps.HealthHealthy:  "healthy",
		bpfmaps.HealthDraining: "draining",
		bpfmaps.HealthDown:     "down",
		200:                    "down", // unknown values must read as the safe state
	}
	for v, want := range cases {
		if got := healthStateName(v); got != want {
			t.Errorf("healthStateName(%d) = %q, want %q", v, got, want)
		}
	}
}

func TestSetBackendWeightRejectsOverMax(t *testing.T) {
	// Rejected before any map access, so a zero-value Dataplane suffices.
	_, err := (&Dataplane{}).SetBackendWeight(1, "", MaxBackendWeight+1)
	if !errors.Is(err, ErrInvalidWeight) {
		t.Fatalf("err = %v, want ErrInvalidWeight (the API maps it to 400)", err)
	}
}
