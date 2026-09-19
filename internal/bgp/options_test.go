// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package bgp

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"

	"github.com/zyvorai/rivora/internal/config"
	"github.com/zyvorai/rivora/internal/dataplane"
)

// startFakePeerWith is startFakePeer with a hook to adjust the fake router's side of the
// session (a password, graceful restart) before it is added.
func startFakePeerWith(t *testing.T, peerAS, ourAS uint32, tweak func(*api.Peer)) (*server.BgpServer, int) {
	t.Helper()
	port := freePort(t)
	s := server.NewBgpServer()
	go s.Serve()
	t.Cleanup(s.Stop)
	ctx := context.Background()
	if err := s.StartBgp(ctx, &api.StartBgpRequest{
		Global: &api.Global{Asn: peerAS, RouterId: "2.2.2.2", ListenPort: int32(port)},
	}); err != nil {
		t.Fatalf("start fake peer: %v", err)
	}
	p := &api.Peer{
		Conf:      &api.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: ourAS},
		Transport: &api.Transport{PassiveMode: true},
		AfiSafis:  dualStackAfiSafis(),
	}
	if tweak != nil {
		tweak(p)
	}
	if err := s.AddPeer(ctx, &api.AddPeerRequest{Peer: p}); err != nil {
		t.Fatalf("fake peer add neighbour: %v", err)
	}
	return s, port
}

// attrsOf returns the path attributes the fake peer holds for prefix, or nil if it has none.
func attrsOf(t *testing.T, peer *server.BgpServer, family bgp.Family, prefix string) []bgp.PathAttributeInterface {
	t.Helper()
	var out []bgp.PathAttributeInterface
	if err := peer.ListPath(apiutil.ListPathRequest{TableType: api.TableType_TABLE_TYPE_GLOBAL, Family: family},
		func(nlri bgp.NLRI, paths []*apiutil.Path) {
			if nlri.String() == prefix && len(paths) > 0 {
				out = paths[0].Attrs
			}
		}); err != nil {
		t.Fatalf("list peer rib: %v", err)
	}
	return out
}

func communitiesIn(attrs []bgp.PathAttributeInterface) []uint32 {
	for _, a := range attrs {
		if c, ok := a.(*bgp.PathAttributeCommunities); ok {
			return c.Value
		}
	}
	return nil
}

func localPrefIn(attrs []bgp.PathAttributeInterface) (uint32, bool) {
	for _, a := range attrs {
		if l, ok := a.(*bgp.PathAttributeLocalPref); ok {
			return l.Value, true
		}
	}
	return 0, false
}

func runSpeaker(t *testing.T, cfg config.BGP, source vipSource, peerPort int) *Speaker {
	t.Helper()
	sp, err := newSpeaker(cfg, source, testLogger(), -1, map[string]uint32{"127.0.0.1": uint32(peerPort)})
	if err != nil {
		t.Fatalf("new speaker: %v", err)
	}
	t.Cleanup(func() { sp.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go sp.Run(ctx)
	return sp
}

func vipWithCommunities(addr string, healthy bool, comms ...string) dataplane.Status {
	st := vipStatus(addr, healthy)
	st.BGPCommunities = comms
	return st
}

func TestCommunitiesAndLocalPrefReachThePeer(t *testing.T) {
	// iBGP (the same AS on both sides), because LOCAL_PREF is never sent to an eBGP peer.
	const as = 65001
	peer, port := startFakePeerWith(t, as, as, nil)
	source := &fakeSource{}
	source.set([]dataplane.Status{
		vipWithCommunities("10.9.0.1", true, "65000:7", "no-export"), // its own communities
		vipStatus("10.9.0.2", true),                                  // only the global ones
	})
	lp := uint32(250)
	sp := runSpeaker(t, config.BGP{
		Enabled: true, ASN: as, RouterID: ourRouterID, LocalPref: &lp,
		Communities: []string{"65000:1", "65000:100"},
		Peers:       []config.BGPPeer{{Address: "127.0.0.1", ASN: as}},
	}, source, port)

	waitFor(t, testTimeout, "both host routes advertised", func() bool {
		r := receivedPrefixes(t, peer)
		return r["10.9.0.1/32"] && r["10.9.0.2/32"]
	})
	c1 := communitiesIn(attrsOf(t, peer, bgp.RF_IPv4_UC, "10.9.0.1/32"))
	want1 := []uint32{65000<<16 | 1, 65000<<16 | 100, 65000<<16 | 7, 0xFFFFFF01} // global, then the VIP's own
	if !equalU32(c1, want1) {
		t.Errorf("10.9.0.1 communities = %v, want %v", c1, want1)
	}
	c2 := communitiesIn(attrsOf(t, peer, bgp.RF_IPv4_UC, "10.9.0.2/32"))
	if !equalU32(c2, []uint32{65000<<16 | 1, 65000<<16 | 100}) {
		t.Errorf("10.9.0.2 communities = %v, want only the global ones", c2)
	}
	if got, ok := localPrefIn(attrsOf(t, peer, bgp.RF_IPv4_UC, "10.9.0.1/32")); !ok || got != 250 {
		t.Errorf("local-pref = %d (present %v), want 250", got, ok)
	}

	// Changing a VIP's communities re-advertises its route with the new ones.
	source.set([]dataplane.Status{
		vipWithCommunities("10.9.0.1", true, "65000:8"),
		vipStatus("10.9.0.2", true),
	})
	sp.AnnounceNow()
	waitFor(t, testTimeout, "the changed communities reach the peer", func() bool {
		return equalU32(communitiesIn(attrsOf(t, peer, bgp.RF_IPv4_UC, "10.9.0.1/32")),
			[]uint32{65000<<16 | 1, 65000<<16 | 100, 65000<<16 | 8})
	})
	if !receivedPrefixes(t, peer)["10.9.0.1/32"] {
		t.Error("the route must still be advertised after its communities changed")
	}
}

func equalU32(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestNoCommunitiesOrLocalPrefUnlessConfigured(t *testing.T) {
	const as = 65001
	peer, port := startFakePeerWith(t, as, as, nil)
	source := &fakeSource{}
	source.set([]dataplane.Status{vipStatus("10.9.0.1", true)})
	runSpeaker(t, config.BGP{Enabled: true, ASN: as, RouterID: ourRouterID,
		Peers: []config.BGPPeer{{Address: "127.0.0.1", ASN: as}}}, source, port)
	waitFor(t, testTimeout, "route advertised", func() bool { return receivedPrefixes(t, peer)["10.9.0.1/32"] })
	attrs := attrsOf(t, peer, bgp.RF_IPv4_UC, "10.9.0.1/32")
	if c := communitiesIn(attrs); len(c) != 0 {
		t.Errorf("no communities configured, but the route carries %v", c)
	}
	// gobgp gives an iBGP route a default local-pref of 100 when none is set; what matters is that
	// we did not choose a value.
	if lp, ok := localPrefIn(attrs); ok && lp != 100 {
		t.Errorf("no localPref configured, but the route carries %d", lp)
	}
}

func TestAggregateFollowsHealthOfItsVIPs(t *testing.T) {
	for _, suppress := range []bool{false, true} {
		name := "keeps the specifics"
		if suppress {
			name = "suppresses the specifics"
		}
		t.Run(name, func(t *testing.T) {
			peer, port := startFakePeer(t, peerASN, ourASN)
			source := &fakeSource{}
			source.set([]dataplane.Status{vipStatus("10.9.0.1", true), vipStatus("10.9.0.2", true), vipStatus("10.10.0.1", true)})
			sp := runSpeaker(t, config.BGP{
				Enabled: true, ASN: ourASN, RouterID: ourRouterID,
				Peers: []config.BGPPeer{{Address: "127.0.0.1", ASN: peerASN}},
				Aggregates: []config.BGPAggregate{{
					Prefix: "10.9.0.0/24", SuppressSpecifics: suppress, Communities: []string{"65000:24"},
				}},
			}, source, port)

			waitFor(t, testTimeout, "the aggregate is advertised", func() bool { return receivedPrefixes(t, peer)["10.9.0.0/24"] })
			got := receivedPrefixes(t, peer)
			if !got["10.10.0.1/32"] {
				t.Error("a VIP outside the aggregate must still be advertised as a host route")
			}
			if suppress {
				waitFor(t, testTimeout, "the covered host routes are suppressed", func() bool {
					r := receivedPrefixes(t, peer)
					return !r["10.9.0.1/32"] && !r["10.9.0.2/32"]
				})
			} else if !got["10.9.0.1/32"] || !got["10.9.0.2/32"] {
				t.Error("without suppressSpecifics the covered host routes must still be advertised")
			}
			if c := communitiesIn(attrsOf(t, peer, bgp.RF_IPv4_UC, "10.9.0.0/24")); !equalU32(c, []uint32{65000<<16 | 24}) {
				t.Errorf("aggregate communities = %v, want [65000:24]", c)
			}

			// One healthy VIP is enough to keep the aggregate.
			source.set([]dataplane.Status{vipStatus("10.9.0.1", false), vipStatus("10.9.0.2", true), vipStatus("10.10.0.1", true)})
			sp.AnnounceNow()
			waitFor(t, testTimeout, "the unhealthy VIP is gone but the aggregate stays", func() bool {
				r := receivedPrefixes(t, peer)
				return !r["10.9.0.1/32"] && r["10.9.0.0/24"]
			})
			// None healthy: the aggregate is withdrawn, so the upstream stops sending us traffic.
			source.set([]dataplane.Status{vipStatus("10.9.0.1", false), vipStatus("10.9.0.2", false), vipStatus("10.10.0.1", true)})
			sp.AnnounceNow()
			waitFor(t, testTimeout, "the aggregate is withdrawn when no VIP under it is healthy", func() bool {
				return !receivedPrefixes(t, peer)["10.9.0.0/24"]
			})
			if !receivedPrefixes(t, peer)["10.10.0.1/32"] {
				t.Error("an unrelated VIP's route must survive the aggregate's withdrawal")
			}
			// And it comes back.
			source.set([]dataplane.Status{vipStatus("10.9.0.1", true), vipStatus("10.10.0.1", true)})
			sp.AnnounceNow()
			waitFor(t, testTimeout, "the aggregate returns on recovery", func() bool { return receivedPrefixes(t, peer)["10.9.0.0/24"] })
		})
	}
}

func TestPeerConfigMapsEveryOption(t *testing.T) {
	dir := t.TempDir()
	pw := filepath.Join(dir, "pw")
	if err := os.WriteFile(pw, []byte("from-a-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	local := config.BGP{ASN: 65000}

	p, err := peerConfig(local, config.BGPPeer{Address: "192.0.2.1", ASN: 65100, BFD: true, Multihop: 5, Password: "inline",
		GracefulRestart: &config.BGPGracefulRestart{Enabled: true, RestartTime: 90}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Conf.AuthPassword != "inline" || p.Conf.PeerAsn != 65100 || p.Conf.LocalAsn != 65000 {
		t.Errorf("conf = %+v", p.Conf)
	}
	if p.EbgpMultihop == nil || !p.EbgpMultihop.Enabled || p.EbgpMultihop.MultihopTtl != 5 {
		t.Errorf("multihop = %+v, want enabled with TTL 5", p.EbgpMultihop)
	}
	if p.Bfd == nil || !p.Bfd.Enabled {
		t.Error("bfd not enabled")
	}
	if p.GracefulRestart == nil || !p.GracefulRestart.Enabled || p.GracefulRestart.RestartTime != 90 {
		t.Errorf("graceful restart = %+v, want enabled, 90s", p.GracefulRestart)
	}
	for _, af := range p.AfiSafis {
		if af.MpGracefulRestart == nil || !af.MpGracefulRestart.Config.Enabled {
			t.Errorf("graceful restart not enabled for family %v", af.Config.Family)
		}
	}

	// Defaults: nothing optional is switched on, and graceful restart defaults to 120s.
	plain, err := peerConfig(local, config.BGPPeer{Address: "192.0.2.1", ASN: 65100})
	if err != nil {
		t.Fatal(err)
	}
	if plain.Conf.AuthPassword != "" || plain.EbgpMultihop != nil || plain.GracefulRestart != nil || plain.Bfd != nil {
		t.Errorf("a plain peer must enable no options: %+v", plain)
	}
	gr, _ := peerConfig(local, config.BGPPeer{Address: "192.0.2.1", ASN: 65100, GracefulRestart: &config.BGPGracefulRestart{Enabled: true}})
	if gr.GracefulRestart.RestartTime != 120 {
		t.Errorf("default restart time = %d, want 120", gr.GracefulRestart.RestartTime)
	}
	off, _ := peerConfig(local, config.BGPPeer{Address: "192.0.2.1", ASN: 65100, GracefulRestart: &config.BGPGracefulRestart{Enabled: false}})
	if off.GracefulRestart != nil {
		t.Error("gracefulRestart.enabled=false must not enable it")
	}

	// A password from a file loses its trailing newline; bad files are refused with a reason.
	pf, err := peerConfig(local, config.BGPPeer{Address: "192.0.2.1", ASN: 65100, PasswordFile: pw})
	if err != nil || pf.Conf.AuthPassword != "from-a-file" {
		t.Errorf("passwordFile: got %+v, %v; want password from-a-file", pf, err)
	}
	if _, err := peerConfig(local, config.BGPPeer{Address: "192.0.2.1", ASN: 65100, PasswordFile: filepath.Join(dir, "missing")}); err == nil {
		t.Error("a missing passwordFile must be an error")
	}
	empty := filepath.Join(dir, "empty")
	os.WriteFile(empty, []byte("\n"), 0o600)
	if _, err := peerConfig(local, config.BGPPeer{Address: "192.0.2.1", ASN: 65100, PasswordFile: empty}); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("an empty passwordFile must be refused, got %v", err)
	}
	long := filepath.Join(dir, "long")
	os.WriteFile(long, []byte(strings.Repeat("x", 81)), 0o600)
	if _, err := peerConfig(local, config.BGPPeer{Address: "192.0.2.1", ASN: 65100, PasswordFile: long}); err == nil || !strings.Contains(err.Error(), "80") {
		t.Errorf("an over-long password must be refused, got %v", err)
	}
}

// sessionState returns the fake peer's view of the session with our speaker.
func sessionState(t *testing.T, peer *server.BgpServer) api.PeerState_SessionState {
	t.Helper()
	var st api.PeerState_SessionState
	if err := peer.ListPeer(context.Background(), &api.ListPeerRequest{Address: "127.0.0.1"}, func(p *api.Peer) {
		st = p.State.SessionState
	}); err != nil {
		t.Fatalf("list peer: %v", err)
	}
	return st
}

// TCP MD5 is a kernel socket option that needs CAP_NET_ADMIN, which an unprivileged test process
// does not have (and macOS does not have the option at all), so these run as root on Linux: on
// the test host and in CI's privileged step.
func requireMD5(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("TCP MD5 needs Linux and root (CAP_NET_ADMIN)")
	}
}

func TestMD5SessionEstablishesOnlyWithTheSamePassword(t *testing.T) {
	requireMD5(t)

	t.Run("same password", func(t *testing.T) {
		peer, port := startFakePeerWith(t, peerASN, ourASN, func(p *api.Peer) { p.Conf.AuthPassword = "correct horse" })
		source := &fakeSource{}
		source.set([]dataplane.Status{vipStatus("10.9.0.1", true)})
		runSpeaker(t, config.BGP{Enabled: true, ASN: ourASN, RouterID: ourRouterID,
			Peers: []config.BGPPeer{{Address: "127.0.0.1", ASN: peerASN, Password: "correct horse"}}}, source, port)
		waitFor(t, testTimeout, "an MD5-authenticated session carries the route", func() bool { return receivedPrefixes(t, peer)["10.9.0.1/32"] })
	})

	t.Run("different password", func(t *testing.T) {
		peer, port := startFakePeerWith(t, peerASN, ourASN, func(p *api.Peer) { p.Conf.AuthPassword = "the router's password" })
		source := &fakeSource{}
		source.set([]dataplane.Status{vipStatus("10.9.0.1", true)})
		runSpeaker(t, config.BGP{Enabled: true, ASN: ourASN, RouterID: ourRouterID,
			Peers: []config.BGPPeer{{Address: "127.0.0.1", ASN: peerASN, Password: "not the same"}}}, source, port)
		// A session that cannot authenticate never gets past the TCP handshake, so the
		// route must not arrive however long we wait for a moment.
		waitFor(t, testTimeout, "the session to be attempted at all", func() bool {
			return sessionState(t, peer) != api.PeerState_SESSION_STATE_UNSPECIFIED
		})
		deadline := testTimeout / 3
		start := timeNow()
		for timeSince(start) < deadline {
			if sessionState(t, peer) == api.PeerState_SESSION_STATE_ESTABLISHED || receivedPrefixes(t, peer)["10.9.0.1/32"] {
				t.Fatal("a session with a mismatched MD5 password was established")
			}
			sleepFor(100)
		}
	})

	t.Run("password from a file", func(t *testing.T) {
		pw := filepath.Join(t.TempDir(), "pw")
		os.WriteFile(pw, []byte("filepass\n"), 0o600)
		peer, port := startFakePeerWith(t, peerASN, ourASN, func(p *api.Peer) { p.Conf.AuthPassword = "filepass" })
		source := &fakeSource{}
		source.set([]dataplane.Status{vipStatus("10.9.0.1", true)})
		runSpeaker(t, config.BGP{Enabled: true, ASN: ourASN, RouterID: ourRouterID,
			Peers: []config.BGPPeer{{Address: "127.0.0.1", ASN: peerASN, PasswordFile: pw}}}, source, port)
		waitFor(t, testTimeout, "a session keyed from passwordFile carries the route", func() bool { return receivedPrefixes(t, peer)["10.9.0.1/32"] })
	})
}

// A graceful-restart peer must see the capability we negotiate, with our restart time.
func TestGracefulRestartIsNegotiated(t *testing.T) {
	peer, port := startFakePeerWith(t, peerASN, ourASN, func(p *api.Peer) {
		p.GracefulRestart = &api.GracefulRestart{Enabled: true, RestartTime: 30}
		for _, af := range p.AfiSafis {
			af.MpGracefulRestart = &api.MpGracefulRestart{Config: &api.MpGracefulRestartConfig{Enabled: true}}
		}
	})
	source := &fakeSource{}
	source.set([]dataplane.Status{vipStatus("10.9.0.1", true)})
	runSpeaker(t, config.BGP{Enabled: true, ASN: ourASN, RouterID: ourRouterID,
		Peers: []config.BGPPeer{{Address: "127.0.0.1", ASN: peerASN,
			GracefulRestart: &config.BGPGracefulRestart{Enabled: true, RestartTime: 77}}}}, source, port)
	waitFor(t, testTimeout, "the session to establish", func() bool { return receivedPrefixes(t, peer)["10.9.0.1/32"] })

	var seen uint32
	if err := peer.ListPeer(context.Background(), &api.ListPeerRequest{Address: "127.0.0.1"}, func(p *api.Peer) {
		if p.GracefulRestart != nil {
			seen = p.GracefulRestart.PeerRestartTime
		}
	}); err != nil {
		t.Fatal(err)
	}
	if seen != 77 {
		t.Errorf("the peer learned a restart time of %d from us, want 77: graceful restart was not negotiated", seen)
	}
}

func timeNow() time.Time                  { return time.Now() }
func timeSince(t time.Time) time.Duration { return time.Since(t) }
func sleepFor(ms int)                     { time.Sleep(time.Duration(ms) * time.Millisecond) }
