// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package bgp

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"

	"github.com/zyvorai/rivora/internal/config"
	"github.com/zyvorai/rivora/internal/dataplane"
)

// fakeSource is a test-controlled vipSource: real production code always
// goes through *dataplane.Dataplane, but the BGP speaker only ever needs
// Statuses(), so a fake here lets the test flip backend health directly
// without needing a real BPF dataplane (which this repo's other BGP-
// adjacent tests, e.g. internal/speaker, also avoid the same way).
type fakeSource struct {
	mu       sync.Mutex
	statuses []dataplane.Status
}

func (f *fakeSource) Statuses() ([]dataplane.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]dataplane.Status, len(f.statuses))
	copy(out, f.statuses)
	return out, nil
}

func (f *fakeSource) set(statuses []dataplane.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = statuses
}

// freePort asks the OS for an unused TCP port on 127.0.0.1. There's a
// theoretical race (something else could grab it between this closing
// and gobgp binding it), but that's the standard trick every Go test
// suite doing this uses, including gobgp's own (see its bfd_server_test.go).
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startFakePeer starts a real, second in-process gobgp server standing in
// for an upstream router: it listens on a free loopback port (Transport
// PassiveMode — it waits for our Speaker to dial in, exactly the shape a
// real router configured with a static peer typically takes), and peers
// with ourASN, expecting the incoming TCP session to originate from
// 127.0.0.1 (where our Speaker's outbound loopback connection will
// actually come from — note this is independent of our Speaker's BGP
// router-id, which is just an opaque 32-bit identifier, not a real
// address gobgp binds to or dials from). Returns the server and the port
// it's listening on.
func startFakePeer(t *testing.T, peerASN, ourASN uint32) (*server.BgpServer, int) {
	return startFakePeerAt(t, "127.0.0.1", peerASN, ourASN)
}

// startFakePeerAt is startFakePeer expecting our session to come from neighbor, which lets a test
// run two fake routers at once: one reached over 127.0.0.1, the other over ::1.
func startFakePeerAt(t *testing.T, neighbor string, peerASN, ourASN uint32) (*server.BgpServer, int) {
	t.Helper()
	port := freePort(t)

	s := server.NewBgpServer()
	go s.Serve()
	t.Cleanup(s.Stop)

	ctx := context.Background()
	if err := s.StartBgp(ctx, &api.StartBgpRequest{
		Global: &api.Global{Asn: peerASN, RouterId: "2.2.2.2", ListenPort: int32(port)},
	}); err != nil {
		t.Fatalf("start fake peer bgp: %v", err)
	}

	if err := s.AddPeer(ctx, &api.AddPeerRequest{
		Peer: &api.Peer{
			Conf:      &api.PeerConf{NeighborAddress: neighbor, PeerAsn: ourASN},
			Transport: &api.Transport{PassiveMode: true},
			AfiSafis:  dualStackAfiSafis(),
		},
	}); err != nil {
		t.Fatalf("fake peer add our speaker as a peer: %v", err)
	}

	return s, port
}

// receivedPrefixes lists host-route prefix strings currently in the fake
// peer's global RIB for family (IPv4 /32 or IPv6 /128).
func receivedPrefixes(t *testing.T, peer *server.BgpServer) map[string]bool {
	return receivedPrefixesFamily(t, peer, bgp.RF_IPv4_UC)
}

func receivedPrefixesFamily(t *testing.T, peer *server.BgpServer, family bgp.Family) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	err := peer.ListPath(apiutil.ListPathRequest{
		TableType: api.TableType_TABLE_TYPE_GLOBAL,
		Family:    family,
	}, func(prefix bgp.NLRI, paths []*apiutil.Path) {
		if len(paths) > 0 {
			out[prefix.String()] = true
		}
	})
	if err != nil {
		t.Fatalf("list peer rib: %v", err)
	}
	return out
}

// waitFor polls cond until it returns true or timeout elapses, failing
// the test with msg otherwise. BGP session establishment and route
// propagation are inherently asynchronous even over loopback, so this
// (not a fixed sleep) is what the test waits on throughout.
func waitFor(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", msg)
}

const (
	testTimeout = 15 * time.Second
	ourASN      = 65001
	ourRouterID = "127.0.0.2"
	peerASN     = 65002
)

func vipStatus(addr string, healthy bool) dataplane.Status {
	return dataplane.Status{
		VIPAddress: addr,
		Protocol:   "tcp",
		Mode:       "nat",
		Backends:   []dataplane.BackendStatus{{ID: 1, Address: "10.0.0.11", Port: 8080, Healthy: healthy}},
	}
}

// TestSpeakerAdvertisesAndWithdrawsOnHealthChange is the load-bearing
// regression test for this whole package: a VIP with a healthy backend
// must actually reach a real BGP peer's RIB as an advertised route, and a
// VIP whose backend goes unhealthy must be withdrawn from it — proving
// the health-gated model end-to-end over a real (loopback) BGP session,
// not just the pure healthyVIPAddresses diff logic vips_test.go covers.
func TestSpeakerAdvertisesAndWithdrawsOnHealthChange(t *testing.T) {
	peer, peerPort := startFakePeer(t, peerASN, ourASN)

	source := &fakeSource{}
	// vip1 starts healthy, vip2 never has a healthy backend at all.
	source.set([]dataplane.Status{
		vipStatus("10.9.0.1", true),
		vipStatus("10.9.0.2", false),
	})

	cfg := config.BGP{
		Enabled:  true,
		ASN:      ourASN,
		RouterID: ourRouterID,
		Peers:    []config.BGPPeer{{Address: "127.0.0.1", ASN: peerASN}},
	}
	sp, err := newSpeaker(cfg, source, testLogger(), -1, map[string]uint32{"127.0.0.1": uint32(peerPort)})
	if err != nil {
		t.Fatalf("new speaker: %v", err)
	}
	defer sp.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sp.Run(ctx)

	waitFor(t, testTimeout, "vip1 (healthy) advertised to the peer", func() bool {
		return receivedPrefixes(t, peer)["10.9.0.1/32"]
	})
	if got := receivedPrefixes(t, peer); got["10.9.0.2/32"] {
		t.Fatal("vip2 (never healthy) must never be advertised, but the peer received it")
	}

	// Flip vip1 unhealthy and confirm the peer sees the route withdrawn.
	source.set([]dataplane.Status{
		vipStatus("10.9.0.1", false),
		vipStatus("10.9.0.2", false),
	})
	sp.AnnounceNow()
	waitFor(t, testTimeout, "vip1 withdrawn after its only backend went unhealthy", func() bool {
		return !receivedPrefixes(t, peer)["10.9.0.1/32"]
	})

	// And confirm re-advertisement on recovery, closing the loop.
	source.set([]dataplane.Status{
		vipStatus("10.9.0.1", true),
	})
	sp.AnnounceNow()
	waitFor(t, testTimeout, "vip1 re-advertised after its backend recovered", func() bool {
		return receivedPrefixes(t, peer)["10.9.0.1/32"]
	})
}

func vipStatus6(addr string, healthy bool) dataplane.Status {
	return dataplane.Status{
		VIPAddress: addr,
		Protocol:   "tcp",
		Mode:       "nat",
		Backends:   []dataplane.BackendStatus{{ID: 1, Address: "2001:db8::11", Port: 8080, Healthy: healthy}},
	}
}

// TestSpeakerAdvertisesIPv6HostRoute mirrors the IPv4 health-gated test
// for an IPv6 VIP: with ipv6NextHop set, a healthy VIP must appear on the
// peer as a /128 (RF_IPv6_UC) and withdraw when the backend goes down.
func TestSpeakerAdvertisesIPv6HostRoute(t *testing.T) {
	peer, peerPort := startFakePeer(t, peerASN, ourASN)

	source := &fakeSource{}
	source.set([]dataplane.Status{
		vipStatus6("2001:db8:9::1", true),
		vipStatus6("2001:db8:9::2", false),
	})

	cfg := config.BGP{
		Enabled:     true,
		ASN:         ourASN,
		RouterID:    ourRouterID,
		IPv6NextHop: "2001:db8::1",
		Peers:       []config.BGPPeer{{Address: "127.0.0.1", ASN: peerASN}},
	}
	sp, err := newSpeaker(cfg, source, testLogger(), -1, map[string]uint32{"127.0.0.1": uint32(peerPort)})
	if err != nil {
		t.Fatalf("new speaker: %v", err)
	}
	defer sp.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sp.Run(ctx)

	waitFor(t, testTimeout, "ipv6 vip1 advertised as /128", func() bool {
		return receivedPrefixesFamily(t, peer, bgp.RF_IPv6_UC)["2001:db8:9::1/128"]
	})
	if got := receivedPrefixesFamily(t, peer, bgp.RF_IPv6_UC); got["2001:db8:9::2/128"] {
		t.Fatal("unhealthy ipv6 vip must never be advertised")
	}

	source.set([]dataplane.Status{vipStatus6("2001:db8:9::1", false)})
	sp.AnnounceNow()
	waitFor(t, testTimeout, "ipv6 vip1 withdrawn", func() bool {
		return !receivedPrefixesFamily(t, peer, bgp.RF_IPv6_UC)["2001:db8:9::1/128"]
	})
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
