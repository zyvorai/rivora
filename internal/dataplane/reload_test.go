// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package dataplane

import (
	"reflect"
	"testing"

	"github.com/zyvorai/rivora/internal/config"
)

func vip(addr string, port uint16, backends ...string) config.VIP {
	v := config.VIP{Address: addr, Port: port, Protocol: config.ProtoTCP, Mode: config.ModeNAT}
	for _, b := range backends {
		v.Backends = append(v.Backends, config.Backend{Address: b, Port: 8080})
	}
	return v
}

func specs(vips ...config.VIP) map[string]config.VIP {
	m := map[string]config.VIP{}
	for _, v := range vips {
		m[vipKeyString(v)] = v
	}
	return m
}

func TestPlanReloadUnchangedTouchesNothing(t *testing.T) {
	a, b := vip("10.0.0.1", 80, "10.1.0.1", "10.1.0.2"), vip("10.0.0.2", 443, "10.1.0.3")
	plan := planReload(specs(a, b), []config.VIP{a, b})
	if len(plan.upserts) != 0 || len(plan.removes) != 0 || plan.unchanged != 2 {
		t.Errorf("identical config should be a no-op, got %+v", plan)
	}
}

func TestPlanReloadClassifiesAddUpdateRemove(t *testing.T) {
	keep := vip("10.0.0.1", 80, "10.1.0.1")
	changed := vip("10.0.0.2", 80, "10.1.0.2")
	gone := vip("10.0.0.3", 80, "10.1.0.3")
	added := vip("10.0.0.4", 80, "10.1.0.4")

	changedNext := vip("10.0.0.2", 80, "10.1.0.2", "10.1.0.9") // gained a backend
	plan := planReload(specs(keep, changed, gone), []config.VIP{keep, changedNext, added})

	if plan.unchanged != 1 || plan.updated != 1 || plan.added != 1 {
		t.Errorf("counts = unchanged %d updated %d added %d; want 1/1/1", plan.unchanged, plan.updated, plan.added)
	}
	if !reflect.DeepEqual(plan.removes, []string{vipKeyString(gone)}) {
		t.Errorf("removes = %v, want only the VIP dropped from the file", plan.removes)
	}
	if len(plan.upserts) != 2 || plan.upserts[0].Address != "10.0.0.2" || plan.upserts[1].Address != "10.0.0.4" {
		t.Errorf("upserts = %+v, want the changed then the added VIP, in file order", plan.upserts)
	}
}

func TestPlanReloadDetectsBackendWeightAndModeChanges(t *testing.T) {
	base := vip("10.0.0.1", 80, "10.1.0.1")

	weighted := base
	weighted.Backends = []config.Backend{{Address: "10.1.0.1", Port: 8080, Weight: 5}}
	if p := planReload(specs(base), []config.VIP{weighted}); p.updated != 1 {
		t.Errorf("a weight change must be picked up, got %+v", p)
	}

	dsr := base
	dsr.Mode = config.ModeDSR
	if p := planReload(specs(base), []config.VIP{dsr}); p.updated != 1 {
		t.Errorf("a mode change must be picked up, got %+v", p)
	}
}

func TestPlanReloadRemovesAreSortedForStableLogs(t *testing.T) {
	a, b, c := vip("10.0.0.3", 80), vip("10.0.0.1", 80), vip("10.0.0.2", 80)
	plan := planReload(specs(a, b, c), nil)
	if len(plan.removes) != 3 || plan.removes[0] > plan.removes[1] || plan.removes[1] > plan.removes[2] {
		t.Errorf("removes not sorted: %v", plan.removes)
	}
}

func TestPlanReloadDuplicateKeyLaterWins(t *testing.T) {
	first := vip("10.0.0.1", 80, "10.1.0.1")
	second := vip("10.0.0.1", 80, "10.1.0.2")
	plan := planReload(nil, []config.VIP{first, second})
	if plan.added != 1 || len(plan.upserts) != 1 {
		t.Fatalf("duplicate key should collapse to one add, got %+v", plan)
	}
	if plan.upserts[0].Backends[0].Address != "10.1.0.2" {
		t.Errorf("later duplicate should win, got backend %s", plan.upserts[0].Backends[0].Address)
	}
}

func TestPlanReloadRetriesAVIPWhoseEarlierUpdateFailed(t *testing.T) {
	// A VIP whose UpsertVIP failed keeps a zero-value recorded spec; the next
	// reload must see it as changed rather than trusting it as up to date.
	want := vip("10.0.0.1", 80, "10.1.0.1")
	cur := map[string]config.VIP{vipKeyString(want): {}}
	if p := planReload(cur, []config.VIP{want}); p.updated != 1 || len(p.upserts) != 1 {
		t.Errorf("half-applied VIP was not retried: %+v", p)
	}
}
