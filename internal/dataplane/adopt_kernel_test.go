// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package dataplane

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cilium/ebpf"

	"github.com/zyvorai/rivora/internal/bpfmaps"
	"github.com/zyvorai/rivora/internal/config"
	"github.com/zyvorai/rivora/internal/loader"
)

// kernelDatapath creates the real BPF maps (unpinned, so it never touches a running rivorad's
// pinned ones) or skips: it needs Linux, CAP_BPF/root and the compiled objects (make bpf).
func kernelDatapath(t *testing.T) *loader.Datapath {
	t.Helper()
	ingress, nat := filepath.Join("..", "..", "bpf", "xdp_ingress.o"), filepath.Join("..", "..", "bpf", "tc_nat.o")
	for _, o := range []string{ingress, nat} {
		if _, err := os.Stat(o); err != nil {
			t.Skipf("%s not built (make bpf): %v", o, err)
		}
	}
	dp, err := loader.LoadMapsOnly(ingress, nat)
	if err != nil {
		t.Skipf("cannot create BPF maps here (needs Linux and root): %v", err)
	}
	t.Cleanup(dp.Close)
	return dp
}

func countEntries(t *testing.T, m *ebpf.Map) int {
	t.Helper()
	if m == nil {
		return 0
	}
	k, v := make([]byte, m.KeySize()), make([]byte, m.ValueSize())
	n := 0
	for it := m.Iterate(); it.Next(&k, &v); {
		n++
	}
	return n
}

func natVIP(addr string, port uint16, proto config.Protocol, backends ...string) config.VIP {
	v := config.VIP{Address: addr, Port: port, Protocol: proto, Mode: config.ModeNAT}
	for _, b := range backends {
		v.Backends = append(v.Backends, config.Backend{Address: b, Port: 8080})
	}
	return v
}

func serviceID(t *testing.T, d *Dataplane, key string) uint32 {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.services[key]
	if !ok {
		t.Fatalf("no service %s", key)
	}
	return e.serviceID
}

// A Kubernetes-mode rivorad restarts with its maps still pinned. Adoption must give the leftovers
// back their IDs (so nothing new collides with them), keep them forwarding until the reconcilers
// have run, then remove exactly the ones nothing claimed.
func TestKubernetesModeAdoptsThenPrunesUnclaimed(t *testing.T) {
	dp := kernelDatapath(t)

	a := natVIP("10.0.0.1", 80, config.ProtoTCP, "10.1.0.1", "10.1.0.2")
	b := natVIP("10.0.0.2", 443, config.ProtoTCP, "10.1.0.3")
	c := natVIP("10.0.0.3", 53, config.ProtoUDP, "10.1.0.4")
	r := natVIP("10.0.0.4", 30000, config.ProtoTCP, "10.1.0.5")
	r.PortEnd = 30010
	r.Backends[0].Port = 0
	r.HealthCheck.Port = 8080
	v6 := natVIP("2001:db8::1", 80, config.ProtoTCP, "2001:db8:1::1")

	// First run.
	first := New(config.Config{}, dp)
	if err := first.Apply(nil); err != nil {
		t.Fatal(err)
	}
	for _, v := range []config.VIP{a, b, c, r, v6} {
		if err := first.UpsertVIP(v); err != nil {
			t.Fatalf("upsert %s: %v", VIPKey(v), err)
		}
	}
	ids := map[string]uint32{}
	for _, v := range []config.VIP{a, b, c, r, v6} {
		ids[VIPKey(v)] = serviceID(t, first, VIPKey(v))
	}
	if n := countEntries(t, dp.Maps[bpfmaps.MapVIP]); n != 3 {
		t.Fatalf("vip_map holds %d entries after the first run, want 3", n)
	}

	// Restart: a new Dataplane over the same maps, as after a pinned-map restart.
	second := New(config.Config{}, dp)
	if err := second.Apply(nil); err != nil {
		t.Fatal(err)
	}
	if got := second.Startup().Adopted; got != 5 {
		t.Fatalf("adopted %d VIPs, want 5 (dropped %d)", got, second.Startup().Dropped)
	}
	if second.Unclaimed() != 5 {
		t.Fatalf("%d unclaimed, want 5", second.Unclaimed())
	}
	for k, want := range ids {
		if got := serviceID(t, second, k); got != want {
			t.Errorf("%s came back as service %d, was %d", k, got, want)
		}
	}
	// Nothing is removed by Apply itself: the leftovers keep forwarding.
	if n := countEntries(t, dp.Maps[bpfmaps.MapVIP]); n != 3 {
		t.Errorf("vip_map lost entries at start-up: %d, want 3", n)
	}

	// A new VIP must not be handed an ID an adopted one still holds.
	fresh := natVIP("10.0.0.9", 8080, config.ProtoTCP, "10.1.0.9")
	if err := second.UpsertVIP(fresh); err != nil {
		t.Fatal(err)
	}
	freshID := serviceID(t, second, VIPKey(fresh))
	for k, id := range ids {
		if id == freshID {
			t.Fatalf("new VIP took service id %d, which adopted %s still holds", id, k)
		}
	}

	// An operator weight change on a VIP no reconciler has claimed yet must not rebuild it from
	// the empty spec adoption left behind.
	backendID := uint32(0)
	second.mu.Lock()
	backendID = second.services[VIPKey(b)].backendIDs["10.1.0.3:8080"]
	second.mu.Unlock()
	if _, err := second.SetBackendWeight(backendID, "", 5); !errors.Is(err, ErrVIPNotFound) {
		t.Fatalf("SetBackendWeight on an unclaimed VIP: %v, want ErrVIPNotFound", err)
	}
	if n := countEntries(t, dp.Maps[bpfmaps.MapVIP]); n != 4 {
		t.Fatalf("vip_map has %d entries, want 4 (the failed weight change must not have removed anything)", n)
	}

	// The reconcilers claim a and c, changing a's backends on the way.
	a2 := natVIP("10.0.0.1", 80, config.ProtoTCP, "10.1.0.1", "10.1.0.7")
	for _, v := range []config.VIP{a2, c} {
		if err := second.UpsertVIP(v); err != nil {
			t.Fatal(err)
		}
	}
	if got := serviceID(t, second, VIPKey(a)); got != ids[VIPKey(a)] {
		t.Errorf("claiming a changed its service id: %d -> %d", ids[VIPKey(a)], got)
	}
	if second.Unclaimed() != 3 {
		t.Fatalf("%d unclaimed after claiming two, want 3", second.Unclaimed())
	}

	pruned := second.PruneUnclaimed()
	want := []string{VIPKey(v6), VIPKey(b), VIPKey(r)}
	if len(pruned) != 3 {
		t.Fatalf("pruned %v, want %v", pruned, want)
	}
	gotSet := map[string]bool{}
	for _, k := range pruned {
		gotSet[k] = true
	}
	for _, k := range want {
		if !gotSet[k] {
			t.Errorf("%s was not pruned (got %v)", k, pruned)
		}
	}
	if second.Unclaimed() != 0 {
		t.Errorf("%d still unclaimed after pruning", second.Unclaimed())
	}
	if n := countEntries(t, dp.Maps[bpfmaps.MapVIP]); n != 3 { // a, c, fresh
		t.Errorf("vip_map holds %d entries after pruning, want 3", n)
	}
	if n := countEntries(t, dp.Maps[bpfmaps.MapVIP6]); n != 0 {
		t.Errorf("vip_map6 still holds %d entries", n)
	}
	if n := countEntries(t, second.rangeMap(true)); n != 0 {
		t.Errorf("the range VIP left %d trie entries behind", n)
	}
	if n := countEntries(t, dp.Maps[bpfmaps.MapServiceConfig]); n < 3 {
		t.Errorf("service_config lost live entries: %d", n)
	}

	// A second restart adopts exactly the survivors, under the same IDs, all unclaimed.
	third := New(config.Config{}, dp)
	if err := third.Apply(nil); err != nil {
		t.Fatal(err)
	}
	if got := third.Startup().Adopted; got != 3 {
		t.Fatalf("adopted %d after pruning, want 3 (dropped %d)", got, third.Startup().Dropped)
	}
	if got := serviceID(t, third, VIPKey(a)); got != ids[VIPKey(a)] {
		t.Errorf("a's service id changed across the second restart: %d -> %d", ids[VIPKey(a)], got)
	}
}

func TestPruneUnclaimedWithNothingAdoptedIsANoop(t *testing.T) {
	dp := kernelDatapath(t)
	d := New(config.Config{}, dp)
	if err := d.Apply(nil); err != nil {
		t.Fatal(err)
	}
	if err := d.UpsertVIP(natVIP("10.0.0.1", 80, config.ProtoTCP, "10.1.0.1")); err != nil {
		t.Fatal(err)
	}
	if got := d.PruneUnclaimed(); len(got) != 0 {
		t.Fatalf("pruned %v from a run that adopted nothing", got)
	}
	if n := countEntries(t, dp.Maps[bpfmaps.MapVIP]); n != 1 {
		t.Fatalf("a VIP this run programmed was pruned: %d left", n)
	}
}
