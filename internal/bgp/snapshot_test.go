// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package bgp

import (
	"context"
	"testing"

	"github.com/zyvorai/rivora/internal/config"
	"github.com/zyvorai/rivora/internal/dataplane"
)

func startSpeaker(t *testing.T, cfg config.BGP, source vipSource, peerPorts map[string]uint32) (*Speaker, context.CancelFunc) {
	t.Helper()
	sp, err := newSpeaker(cfg, source, testLogger(), -1, peerPorts)
	if err != nil {
		t.Fatalf("new speaker: %v", err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go sp.Run(ctx)
	return sp, cancel
}

// Against a real second gobgp server: the session comes up, the healthy VIP is
// advertised and counted, and when the peer goes away the snapshot says so and
// counts the transition.
func TestSnapshotTracksARealSession(t *testing.T) {
	peer, peerPort := startFakePeer(t, peerASN, ourASN)
	source := &fakeSource{}
	source.set([]dataplane.Status{vipStatus("10.9.0.1", true), vipStatus("10.9.0.2", false)})

	cfg := config.BGP{
		Enabled: true, ASN: ourASN, RouterID: ourRouterID,
		Peers: []config.BGPPeer{{Address: "127.0.0.1", ASN: peerASN}},
	}
	sp, _ := startSpeaker(t, cfg, source, map[string]uint32{"127.0.0.1": uint32(peerPort)})

	waitFor(t, testTimeout, "session established and the healthy VIP advertised", func() bool {
		s := sp.Snapshot()
		return len(s.Peers) == 1 && s.Peers[0].Established && s.AdvertisedIPv4 == 1
	})
	s := sp.Snapshot()
	if p := s.Peers[0]; p.Address != "127.0.0.1" || p.ASN != peerASN || p.State != "ESTABLISHED" {
		t.Errorf("peer = %+v, want 127.0.0.1 AS%d ESTABLISHED", p, peerASN)
	}
	if s.AdvertisedIPv4 != 1 || s.AdvertisedIPv6 != 0 {
		t.Errorf("advertised v4/v6 = %d/%d, want 1/0 (the unhealthy VIP must not count)", s.AdvertisedIPv4, s.AdvertisedIPv6)
	}
	if s.AdvertiseErrors != 0 || s.WithdrawErrors != 0 {
		t.Errorf("unexpected route-update errors: %+v", s)
	}
	before := s.Peers[0].StateChanges

	// The router goes away: the session must show as down, and the drop counted.
	peer.Stop()
	waitFor(t, testTimeout, "snapshot to show the session down after the peer stopped", func() bool {
		s := sp.Snapshot()
		return !s.Peers[0].Established
	})
	waitFor(t, testTimeout, "the down transition to be counted", func() bool {
		return sp.Snapshot().Peers[0].StateChanges > before
	})
}

func TestSnapshotListsAnUnreachablePeerAsDown(t *testing.T) {
	// Nothing listens on this port, so the session never establishes. The peer must
	// still be reported, as down, rather than silently missing from the output.
	dead := freePort(t)
	source := &fakeSource{}
	source.set([]dataplane.Status{vipStatus("10.9.0.1", true)})
	cfg := config.BGP{
		Enabled: true, ASN: ourASN, RouterID: ourRouterID,
		Peers: []config.BGPPeer{{Address: "127.0.0.1", ASN: peerASN}},
	}
	sp, _ := startSpeaker(t, cfg, source, map[string]uint32{"127.0.0.1": uint32(dead)})

	s := sp.Snapshot()
	if len(s.Peers) != 1 {
		t.Fatalf("got %d peers, want the 1 configured", len(s.Peers))
	}
	if s.Peers[0].Established {
		t.Error("a peer nothing is listening for reported as ESTABLISHED")
	}
	if s.Peers[0].State == "" {
		t.Error("peer state empty; want gobgp's state or UNKNOWN")
	}
}

func TestSnapshotCountsAdvertiseFailures(t *testing.T) {
	// An IPv6 VIP can only be advertised with an ipv6NextHop; without one every
	// resync fails. That must be visible as a rising error counter, not just a log.
	peer, peerPort := startFakePeer(t, peerASN, ourASN)
	_ = peer
	source := &fakeSource{}
	source.set([]dataplane.Status{vipStatus6("fd00::1", true)})
	cfg := config.BGP{ // deliberately no IPv6NextHop
		Enabled: true, ASN: ourASN, RouterID: ourRouterID,
		Peers: []config.BGPPeer{{Address: "127.0.0.1", ASN: peerASN}},
	}
	sp, _ := startSpeaker(t, cfg, source, map[string]uint32{"127.0.0.1": uint32(peerPort)})

	waitFor(t, testTimeout, "the failed IPv6 advertise to be counted", func() bool {
		return sp.Snapshot().AdvertiseErrors >= 1
	})
	if s := sp.Snapshot(); s.AdvertisedIPv6 != 0 {
		t.Errorf("AdvertisedIPv6 = %d, want 0: the route was never accepted", s.AdvertisedIPv6)
	}
}
