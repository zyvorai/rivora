// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package bgp

import (
	"context"
	"net/netip"
	"reflect"
	"testing"

	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"

	"github.com/zyvorai/rivora/internal/config"
	"github.com/zyvorai/rivora/internal/dataplane"
)

func hasRoute(t *testing.T, r *server.BgpServer, prefix string) bool {
	return receivedPrefixesFamily(t, r, bgp.RF_IPv6_UC)[prefix]
}

func limitedVIP(addr string, peers ...string) dataplane.Status {
	st := vipStatus6(addr, true)
	st.BGPPeers = peers
	return st
}

// A route limited to some peers must reach those routers and no others, through changes to the
// limit and through a peer added after the route is already being advertised.
func TestRoutesAreLimitedToTheirPeers(t *testing.T) {
	r1, port1 := startFakePeerAt(t, "127.0.0.1", peerASN, ourASN)
	r2, port2 := startFakePeerAt(t, "::1", peerASN, ourASN)

	const (
		toR1  = "2001:db8:9::1"
		toR2  = "2001:db8:9::2"
		toAll = "2001:db8:9::3"
	)
	source := &fakeSource{}
	v4Other, v4Open := vipStatus("10.9.5.1", true), vipStatus("10.9.5.2", true)
	v4Other.BGPPeers = []string{"::1"} // an IPv4 route meant for the other router
	source.set([]dataplane.Status{limitedVIP(toR1, "127.0.0.1"), limitedVIP(toR2, "::1"), limitedVIP(toAll), v4Other, v4Open})

	cfg := config.BGP{Enabled: true, ASN: ourASN, RouterID: ourRouterID, IPv6NextHop: "2001:db8::1",
		Peers: []config.BGPPeer{{Address: "127.0.0.1", ASN: peerASN}}}
	sp, err := newSpeaker(cfg, source, testLogger(), -1, map[string]uint32{"127.0.0.1": uint32(port1), "::1": uint32(port2)})
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sp.Run(ctx)

	// Only r1 is a peer so far: it gets the route meant for it and the unlimited one, and never the
	// one limited to ::1 even though that peer does not exist yet.
	waitFor(t, testTimeout, "r1 gets its own and the unlimited route", func() bool {
		return hasRoute(t, r1, toR1+"/128") && hasRoute(t, r1, toAll+"/128")
	})
	if hasRoute(t, r1, toR2+"/128") {
		t.Fatal("a route limited to another peer reached r1")
	}
	// The same for IPv4, whose prefixes live in a separate set.
	waitFor(t, testTimeout, "r1 gets the unlimited IPv4 route", func() bool { return receivedPrefixes(t, r1)["10.9.5.2/32"] })
	if receivedPrefixes(t, r1)["10.9.5.1/32"] {
		t.Fatal("an IPv4 route limited to another peer reached r1")
	}

	// Adding the second peer later: it gets the routes for it and the unlimited one, and none that
	// are limited to r1 — its policy must be in place before its session sends anything.
	if _, err := sp.SetPeers([]config.BGPPeer{{Address: "127.0.0.1", ASN: peerASN}, {Address: "::1", ASN: peerASN}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, testTimeout, "r2 gets its own and the unlimited route", func() bool {
		return hasRoute(t, r2, toR2+"/128") && hasRoute(t, r2, toAll+"/128")
	})
	if hasRoute(t, r2, toR1+"/128") {
		t.Fatal("a route limited to r1 reached the peer that was added later")
	}
	if !hasRoute(t, r1, toR1+"/128") || hasRoute(t, r1, toR2+"/128") {
		t.Error("adding r2 disturbed what r1 has")
	}

	// Move the first route from r1 to r2: r1 must lose it and r2 gain it.
	source.set([]dataplane.Status{limitedVIP(toR1, "::1"), limitedVIP(toR2, "::1"), limitedVIP(toAll)})
	sp.AnnounceNow()
	waitFor(t, testTimeout, "the route moves from r1 to r2", func() bool {
		return !hasRoute(t, r1, toR1+"/128") && hasRoute(t, r2, toR1+"/128")
	})

	// Lift every limit: everything goes to both.
	source.set([]dataplane.Status{limitedVIP(toR1), limitedVIP(toR2), limitedVIP(toAll)})
	sp.AnnounceNow()
	waitFor(t, testTimeout, "with no limits every route reaches both routers", func() bool {
		return hasRoute(t, r1, toR1+"/128") && hasRoute(t, r1, toR2+"/128") && hasRoute(t, r2, toR1+"/128") && hasRoute(t, r2, toR2+"/128")
	})

	// Limit one back to r1 alone: r2 loses it.
	source.set([]dataplane.Status{limitedVIP(toR1, "127.0.0.1"), limitedVIP(toR2), limitedVIP(toAll)})
	sp.AnnounceNow()
	waitFor(t, testTimeout, "a route limited again leaves the router it no longer applies to", func() bool {
		return !hasRoute(t, r2, toR1+"/128") && hasRoute(t, r1, toR1+"/128")
	})
}

func TestPeerScopeMergesVIPsSharingAnAddress(t *testing.T) {
	var s *peerScope
	if s.list() != nil {
		t.Error("no VIPs: no limit")
	}
	s = &peerScope{}
	s.add([]string{"10.0.0.2"})
	s.add([]string{"10.0.0.1", "10.0.0.2"})
	if got := s.list(); !reflect.DeepEqual(got, []string{"10.0.0.1", "10.0.0.2"}) {
		t.Errorf("union of the limited VIPs = %v", got)
	}
	s.add(nil) // a VIP on the same address that is not limited
	if s.list() != nil {
		t.Errorf("one unlimited VIP makes the whole route unlimited, got %v", s.list())
	}
	s = &peerScope{}
	s.add([]string{"2001:DB8::1"})
	if got := s.list(); !reflect.DeepEqual(got, []string{"2001:db8::1"}) {
		t.Errorf("peer addresses are normalised: %v", got)
	}
}

func TestDenyListIsWhatAPeerMustNotReceive(t *testing.T) {
	restrict := map[netip.Prefix][]string{
		netip.MustParsePrefix("10.0.0.1/32"): {"192.0.2.1"},
		netip.MustParsePrefix("10.0.0.2/32"): {"192.0.2.2"},
		netip.MustParsePrefix("10.0.0.3/32"): {"192.0.2.1", "192.0.2.2"},
	}
	if got := denyList("192.0.2.1", restrict); !reflect.DeepEqual(got, []string{"10.0.0.2/32"}) {
		t.Errorf("peer .1 must not get %v", got)
	}
	if got := denyList("192.0.2.9", restrict); len(got) != 3 {
		t.Errorf("a peer named by no route must get none of the limited ones, deny=%v", got)
	}
}
