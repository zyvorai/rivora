// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package bgp

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/osrg/gobgp/v4/api"

	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"

	"github.com/zyvorai/rivora/internal/config"
	"github.com/zyvorai/rivora/internal/dataplane"
)

// stateChanges is how many session-state transitions the speaker has seen for peer addr.
func stateChanges(sp *Speaker, addr string) uint64 {
	sp.statMu.Lock()
	defer sp.statMu.Unlock()
	return sp.stateChanges[addr]
}

// has reports whether router r has the test VIP's /128 in its RIB.
func has(t *testing.T, r *server.BgpServer) bool {
	return receivedPrefixesFamily(t, r, bgp.RF_IPv6_UC)["2001:db8:9::1/128"]
}

func TestSetPeersAddsRemovesAndRestartsWithoutTouchingTheRest(t *testing.T) {
	r1, port1 := startFakePeerAt(t, "127.0.0.1", peerASN, ourASN)
	r2, port2 := startFakePeerAt(t, "::1", peerASN, ourASN)

	source := &fakeSource{}
	source.set([]dataplane.Status{vipStatus6("2001:db8:9::1", true)})
	cfg := config.BGP{Enabled: true, ASN: ourASN, RouterID: ourRouterID, IPv6NextHop: "2001:db8::1"}
	sp, err := newSpeaker(cfg, source, testLogger(), -1, map[string]uint32{"127.0.0.1": uint32(port1), "::1": uint32(port2)})
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sp.Run(ctx)

	p1 := config.BGPPeer{Address: "127.0.0.1", ASN: peerASN}
	p2 := config.BGPPeer{Address: "::1", ASN: peerASN}

	// Nothing configured: nothing advertised anywhere.
	if got := sp.PeerAddresses(); len(got) != 0 {
		t.Fatalf("peers before SetPeers: %v", got)
	}

	// Add the first peer.
	if res, err := sp.SetPeers([]config.BGPPeer{p1}); err != nil || res != (PeerChanges{Added: 1}) {
		t.Fatalf("SetPeers(p1) = %+v, %v", res, err)
	}
	waitFor(t, testTimeout, "the route reaches r1 once its peer is added", func() bool { return has(t, r1) })
	if has(t, r2) {
		t.Fatal("r2 has no peer yet but received the route")
	}

	// Add the second: the first session must not be disturbed.
	flapsBefore := stateChanges(sp, "127.0.0.1")
	if res, err := sp.SetPeers([]config.BGPPeer{p1, p2}); err != nil || res != (PeerChanges{Added: 1}) {
		t.Fatalf("SetPeers(p1,p2) = %+v, %v", res, err)
	}
	waitFor(t, testTimeout, "a peer added later still receives the routes already being advertised", func() bool { return has(t, r2) })
	if got := stateChanges(sp, "127.0.0.1"); got != flapsBefore {
		t.Errorf("the untouched peer's session changed state %d time(s) while another peer was added", got-flapsBefore)
	}

	// Change the first peer's settings: it is restarted (Updated), the second is not.
	flaps2 := stateChanges(sp, "::1")
	p1b := p1
	p1b.GracefulRestart = &config.BGPGracefulRestart{Enabled: true}
	if res, err := sp.SetPeers([]config.BGPPeer{p1b, p2}); err != nil || res != (PeerChanges{Updated: 1}) {
		t.Fatalf("SetPeers(p1 changed, p2) = %+v, %v", res, err)
	}
	waitFor(t, testTimeout, "the route is back at r1 after its peer was restarted", func() bool { return has(t, r1) })
	if got := stateChanges(sp, "::1"); got != flaps2 {
		t.Errorf("the untouched second peer's session changed state while the first was restarted")
	}
	// The same list again changes nothing.
	if res, err := sp.SetPeers([]config.BGPPeer{p1b, p2}); err != nil || res != (PeerChanges{}) {
		t.Fatalf("SetPeers with no change = %+v, %v", res, err)
	}

	// Remove the first: its router loses the route, the second keeps it.
	if res, err := sp.SetPeers([]config.BGPPeer{p2}); err != nil || res != (PeerChanges{Removed: 1}) {
		t.Fatalf("SetPeers(p2) = %+v, %v", res, err)
	}
	waitFor(t, testTimeout, "r1 loses the route once its peer is removed", func() bool { return !has(t, r1) })
	if !has(t, r2) {
		t.Error("removing the first peer took the route away from the second")
	}
	if got, want := sp.PeerAddresses(), []string{"::1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("peers = %v, want %v", got, want)
	}

	// What the removed peer's route limits were built from goes with it: only ::1's remain.
	if got := limitPolicies(sp); got != 1 {
		t.Errorf("%d per-peer export policies remain, want 1 (for ::1)", got)
	}
	// And the address can be added back cleanly.
	if _, err := sp.SetPeers([]config.BGPPeer{p1, p2}); err != nil {
		t.Fatalf("adding back a removed peer: %v", err)
	}
	if got := limitPolicies(sp); got != 2 {
		t.Errorf("%d per-peer export policies after re-adding, want 2", got)
	}
}

// limitPolicies counts the per-peer export policies gobgp holds.
func limitPolicies(sp *Speaker) int {
	n := 0
	_ = sp.server.ListPolicy(context.Background(), &api.ListPolicyRequest{}, func(p *api.Policy) {
		if strings.HasPrefix(p.Name, "rivora-export-") {
			n++
		}
	})
	return n
}
