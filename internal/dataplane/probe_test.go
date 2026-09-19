// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package dataplane

import (
	"testing"

	"github.com/zyvorai/rivora/internal/config"
)

func TestApplyProbeReachesEveryBackendOfTheVIPOnly(t *testing.T) {
	d := &Dataplane{backendStates: map[uint32]*backendState{
		1: {address: "10.1.0.1", port: 80},
		2: {address: "10.1.0.2", port: 80},
		3: {address: "10.1.0.3", port: 80}, // belongs to some other VIP
	}}
	entry := &serviceEntry{backendIDs: map[string]uint32{"10.1.0.1:80": 1, "10.1.0.2:80": 2}}
	want := config.ProbeSpec{Type: config.ProbeHTTP, Path: "/ready", Port: 9000}

	d.applyProbeLocked(entry, want)

	if d.backendStates[1].probe != want || d.backendStates[2].probe != want {
		t.Errorf("the VIP's backends did not all get the probe: %+v / %+v", d.backendStates[1].probe, d.backendStates[2].probe)
	}
	if d.backendStates[3].probe != (config.ProbeSpec{}) {
		t.Errorf("a backend of another VIP was changed: %+v", d.backendStates[3].probe)
	}
}

func TestApplyProbeSkipsAReleasedBackend(t *testing.T) {
	// An ID in the entry with no backendState (already released) must not panic.
	d := &Dataplane{backendStates: map[uint32]*backendState{}}
	d.applyProbeLocked(&serviceEntry{backendIDs: map[string]uint32{"10.1.0.1:80": 7}}, config.ProbeSpec{Type: config.ProbeHTTP})
}

func TestApplyProbeCanReturnABackendToTCP(t *testing.T) {
	// Removing healthCheck from a VIP in the config and reloading must revert its
	// backends to the default probe, not leave the old HTTP one behind.
	d := &Dataplane{backendStates: map[uint32]*backendState{1: {probe: config.ProbeSpec{Type: config.ProbeHTTP, Path: "/x"}}}}
	d.applyProbeLocked(&serviceEntry{backendIDs: map[string]uint32{"a:1": 1}}, config.ProbeSpec{})
	if d.backendStates[1].probe.Effective().Type != config.ProbeTCP {
		t.Errorf("probe after removing healthCheck = %+v, want tcp", d.backendStates[1].probe.Effective())
	}
}

func TestTargetsCarryTheProbe(t *testing.T) {
	spec := config.ProbeSpec{Type: config.ProbeHTTP, Path: "/ready", ExpectStatus: "200"}
	d := &Dataplane{backendStates: map[uint32]*backendState{
		4: {address: "10.1.0.4", port: 8080, probe: spec},
		5: {address: "10.1.0.5", port: 8080},
	}}
	got := map[uint32]config.ProbeSpec{}
	for _, tgt := range d.Targets() {
		got[tgt.ID] = tgt.Probe
	}
	if got[4] != spec {
		t.Errorf("target 4 probe = %+v, want %+v", got[4], spec)
	}
	if got[5] != (config.ProbeSpec{}) {
		t.Errorf("target 5 probe = %+v, want the zero (tcp) default", got[5])
	}
}

func TestPlanReloadNoticesAProbeChange(t *testing.T) {
	// Editing only a VIP's healthCheck must count as a change, or a reload would
	// leave the old probe running while reporting "unchanged".
	base := vip("10.0.0.1", 80, "10.1.0.1")
	edited := base
	edited.HealthCheck = config.ProbeSpec{Type: config.ProbeHTTP, Path: "/ready"}
	if p := planReload(specs(base), []config.VIP{edited}); p.updated != 1 {
		t.Errorf("a healthCheck-only edit was not seen as a change: %+v", p)
	}
	if p := planReload(specs(edited), []config.VIP{edited}); p.unchanged != 1 {
		t.Errorf("an identical healthCheck was seen as a change: %+v", p)
	}
}

func TestAffinityByte(t *testing.T) {
	if affinityByte("") != 0 || affinityByte(config.AffinityNone) != 0 {
		t.Error("none must encode as 0 (what an old pinned service_config holds)")
	}
	if affinityByte(config.AffinityClientIP) != 1 {
		t.Error("clientIP must encode as 1 (RIVORA_AFFINITY_CLIENT_IP)")
	}
	// A round trip, so an adopted service reads back the affinity it was given.
	for _, a := range []config.SessionAffinity{config.AffinityNone, config.AffinityClientIP} {
		if got := affinityFromByte(affinityByte(a)); got != a {
			t.Errorf("round trip of %q = %q", a, got)
		}
	}
}

func TestPlanReloadNoticesAnAffinityChange(t *testing.T) {
	base := vip("10.0.0.1", 80, "10.1.0.1")
	sticky := base
	sticky.SessionAffinity = config.AffinityClientIP
	if p := planReload(specs(base), []config.VIP{sticky}); p.updated != 1 {
		t.Errorf("an affinity-only edit was not seen as a change: %+v", p)
	}
}
