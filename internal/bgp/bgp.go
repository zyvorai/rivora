// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package bgp is rivorad's opt-in BGP+BFD speaker for active/active ECMP
// HA: unlike internal/speaker's L2 responder, which answers for a VIP
// from exactly one node at a time (behind a cluster-wide Lease), this
// speaker runs unleadered on every node and independently advertises a
// /32 (IPv4) or /128 (IPv6) host route for every VIP it currently has at
// least one healthy backend for — withdrawing it the instant that stops
// being true. BGP itself (via ECMP on the upstream routers) is what
// arbitrates which node(s) traffic actually lands on; Rivora's only job
// is to tell the truth about which VIPs this node can currently serve.
//
// Caveat, not hidden: in full-NAT mode a flow's connection state lives
// only on the node that first received it. If the router's ECMP hash
// rebalances — a peer flaps, a node's route flaps, a node joins or
// leaves — in-flight NAT'd connections on a rebalanced-away node can be
// disrupted, since there's no cluster-shared connection table. DSR mode
// doesn't have this problem (the backend itself owns the reply path, so
// the LB is only ever in the forward path). This is the same tradeoff
// MetalLB's BGP mode documents.
package bgp

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"

	"github.com/zyvorai/rivora/internal/config"
	"github.com/zyvorai/rivora/internal/dataplane"
)

// resyncInterval is the periodic health-gated re-evaluation, on top of
// the immediate resync AnnounceNow triggers. Tighter than the ARP
// speaker's 30s announceInterval — a stale BGP advertisement for a VIP
// with no healthy backend is a live traffic-blackholing bug, not just a
// stale ARP cache entry, so the periodic backstop matters more here even
// though AnnounceNow (wired to the health-checker's own callback) is the
// path that normally reacts within a health-check cycle.
const resyncInterval = 10 * time.Second

// vipSource is the subset of *dataplane.Dataplane this speaker needs —
// same narrow-interface pattern internal/speaker, internal/controller,
// and internal/gatewayapi each independently establish.
type vipSource interface {
	Statuses() ([]dataplane.Status, error)
}

var _ vipSource = (*dataplane.Dataplane)(nil)

// Speaker owns one gobgp server and, for as long as Run is active, keeps
// its advertised path set in sync with which VIPs this node currently has
// a healthy backend for.
type Speaker struct {
	cfg    config.BGP
	server *server.BgpServer
	source vipSource
	logger *slog.Logger

	mu         sync.Mutex
	advertised map[netip.Prefix]advertisedRoute // prefix -> the route currently advertised for it

	// Counters for Snapshot, under their own lock so a scrape never waits on a
	// resync pass (which holds mu across gobgp calls).
	statMu          sync.Mutex
	stateChanges    map[string]uint64 // peer address -> session-state transitions seen
	advertiseErrors uint64
	withdrawErrors  uint64

	resyncNow chan struct{}
}

// New starts a gobgp server, configures its global AS/router-id, and
// establishes sessions to every configured peer (with BFD enabled
// per-peer where requested). The server starts advertising nothing until
// Run's first resync pass.
//
// Real deployments always peer on the standard BGP port 179 in both
// directions (gobgp's own default when ListenPort is left unset), so
// that's the only mode exposed publicly here — see newSpeaker.
func New(cfg config.BGP, source vipSource, logger *slog.Logger) (*Speaker, error) {
	return newSpeaker(cfg, source, logger, 0, nil)
}

// newSpeaker is New's real implementation, plus two seams only tests use:
// listenPort overrides gobgp's default passive-listener port (0 lets
// gobgp apply its own default of 179; tests pass -1 to disable listening
// entirely, since binding port 179 needs root), and peerRemotePort
// overrides the port a given peer address is dialed on (tests peer
// against an in-process fake "router" bound to an arbitrary free port
// instead of the real 179).
func newSpeaker(cfg config.BGP, source vipSource, logger *slog.Logger, listenPort int32, peerRemotePort map[string]uint32) (*Speaker, error) {
	s := server.NewBgpServer(server.LoggerOption(logger, nil))
	go s.Serve()

	ctx := context.Background()
	if err := s.StartBgp(ctx, &api.StartBgpRequest{
		Global: &api.Global{
			Asn:        cfg.ASN,
			RouterId:   cfg.RouterID,
			ListenPort: listenPort,
		},
	}); err != nil {
		s.Stop()
		return nil, fmt.Errorf("start bgp: %w", err)
	}

	for _, p := range cfg.Peers {
		peer, err := peerConfig(cfg, p)
		if err != nil {
			s.Stop()
			return nil, fmt.Errorf("bgp peer %s: %w", p.Address, err)
		}
		if port, ok := peerRemotePort[p.Address]; ok {
			peer.Transport = &api.Transport{RemotePort: port}
		}
		if err := s.AddPeer(ctx, &api.AddPeerRequest{Peer: peer}); err != nil {
			s.Stop()
			return nil, fmt.Errorf("add bgp peer %s: %w", p.Address, err)
		}
	}

	return &Speaker{
		cfg:          cfg,
		server:       s,
		source:       source,
		logger:       logger,
		advertised:   map[netip.Prefix]advertisedRoute{},
		stateChanges: map[string]uint64{},
		resyncNow:    make(chan struct{}, 1),
	}, nil
}

// advertisedRoute is what is currently in gobgp for one prefix: its path UUID and a signature of
// the attributes it was advertised with, so a change of communities re-advertises it.
type advertisedRoute struct {
	id  uuid.UUID
	sig string
}

// route is one desired advertisement.
type route struct {
	communities []uint32
}

func (r route) sig() string {
	parts := make([]string, len(r.communities))
	for i, c := range r.communities {
		parts[i] = fmt.Sprintf("%d", c)
	}
	return strings.Join(parts, ",")
}

// peerConfig builds the gobgp peer for p. It is separate from newSpeaker so the mapping from
// rivora's peer options to gobgp's can be tested without a session.
func peerConfig(local config.BGP, p config.BGPPeer) (*api.Peer, error) {
	password := p.Password
	if p.PasswordFile != "" {
		raw, err := os.ReadFile(p.PasswordFile)
		if err != nil {
			return nil, fmt.Errorf("read passwordFile: %w", err)
		}
		password = strings.TrimRight(string(raw), "\r\n")
		if password == "" {
			return nil, fmt.Errorf("passwordFile %s is empty", p.PasswordFile)
		}
		if len(password) > 80 {
			return nil, fmt.Errorf("the password in %s is longer than the 80 bytes TCP MD5 allows", p.PasswordFile)
		}
	}
	peer := &api.Peer{
		Conf: &api.PeerConf{
			NeighborAddress: p.Address,
			PeerAsn:         p.ASN,
			LocalAsn:        local.ASN,
			AuthPassword:    password,
		},
		// Negotiate both unicast families so IPv4 /32 and IPv6 /128
		// VIP host routes can be advertised on the same session.
		AfiSafis: dualStackAfiSafis(),
	}
	if p.BFD {
		peer.Bfd = &api.BfdPeerConfig{Enabled: true}
	}
	if p.Multihop != 0 {
		peer.EbgpMultihop = &api.EbgpMultihop{Enabled: true, MultihopTtl: p.Multihop}
	}
	if g := p.GracefulRestart; g != nil && g.Enabled {
		rt := g.RestartTime
		if rt == 0 {
			rt = 120
		}
		peer.GracefulRestart = &api.GracefulRestart{Enabled: true, RestartTime: rt, NotificationEnabled: true}
		for _, af := range peer.AfiSafis {
			af.MpGracefulRestart = &api.MpGracefulRestart{Config: &api.MpGracefulRestartConfig{Enabled: true}}
		}
	}
	return peer, nil
}

func dualStackAfiSafis() []*api.AfiSafi {
	return []*api.AfiSafi{
		{Config: &api.AfiSafiConfig{Family: &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST}}},
		{Config: &api.AfiSafiConfig{Family: &api.Family{Afi: api.Family_AFI_IP6, Safi: api.Family_SAFI_UNICAST}}},
	}
}

// Close stops the gobgp server, withdrawing every advertised route as
// part of session teardown.
func (sp *Speaker) Close() error {
	sp.server.Stop()
	return nil
}

// AnnounceNow requests an out-of-cycle resync pass — rivorad calls this
// from both a Kubernetes reconciler's OnChange hook (a VIP was added or
// removed) and, more importantly for this package's whole reason to
// exist, the health-checker's per-backend callback (a backend just
// flipped healthy/unhealthy). Non-blocking: a pass already pending
// coalesces with this one.
func (sp *Speaker) AnnounceNow() {
	select {
	case sp.resyncNow <- struct{}{}:
	default:
	}
}

// Run blocks until ctx is cancelled, periodically (and on-demand via
// AnnounceNow) reconciling the advertised path set against which VIPs
// currently have a healthy backend.
func (sp *Speaker) Run(ctx context.Context) error {
	go sp.watchPeerEvents(ctx)

	ticker := time.NewTicker(resyncInterval)
	defer ticker.Stop()

	sp.resync()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			sp.resync()
		case <-sp.resyncNow:
			sp.resync()
		}
	}
}

// resync diffs the routes that should be advertised against what is, and advertises, re-advertises
// (when a route's attributes changed) or withdraws the difference.
func (sp *Speaker) resync() {
	statuses, err := sp.source.Statuses()
	if err != nil {
		sp.logger.Error("list vip statuses for bgp resync", "err", err)
		return
	}
	desired, err := sp.desiredRoutes(statuses)
	if err != nil {
		sp.logger.Error("compute bgp routes", "err", err)
		return
	}

	sp.mu.Lock()
	defer sp.mu.Unlock()

	// Withdraw first: a route whose attributes changed is withdrawn and re-added below, and one
	// whose covering aggregate just appeared is withdrawn before the aggregate goes out.
	for prefix, cur := range sp.advertised {
		want, keep := desired[prefix]
		if keep && want.sig() == cur.sig {
			continue
		}
		if err := sp.withdraw(cur.id); err != nil {
			sp.logger.Error("withdraw bgp route", "prefix", prefix, "err", err)
			sp.countWithdrawError()
			continue
		}
		delete(sp.advertised, prefix)
		if !keep {
			sp.logger.Info("withdrew bgp route", "prefix", prefix)
		}
	}

	prefixes := make([]netip.Prefix, 0, len(desired))
	for p := range desired {
		prefixes = append(prefixes, p)
	}
	sort.Slice(prefixes, func(i, j int) bool { return prefixes[i].String() < prefixes[j].String() })
	for _, prefix := range prefixes {
		if _, ok := sp.advertised[prefix]; ok {
			continue
		}
		r := desired[prefix]
		id, err := sp.advertise(prefix, r)
		if err != nil {
			sp.logger.Error("advertise bgp route", "prefix", prefix, "err", err)
			sp.countAdvertiseError()
			continue
		}
		sp.advertised[prefix] = advertisedRoute{id: id, sig: r.sig()}
		sp.logger.Info("advertised bgp route", "prefix", prefix, "communities", len(r.communities))
	}
}

// desiredRoutes is every route that should be advertised right now: a host route per VIP with a
// healthy backend (unless a covering aggregate suppresses it), and each aggregate that has at
// least one healthy VIP under it. Communities are the global ones plus the VIP's (or aggregate's).
func (sp *Speaker) desiredRoutes(statuses []dataplane.Status) (map[netip.Prefix]route, error) {
	global, err := config.ParseCommunities(sp.cfg.Communities)
	if err != nil {
		return nil, err
	}
	vipComms := map[netip.Addr][]string{}
	for _, st := range statuses {
		if !anyHealthy(st.Backends) {
			continue
		}
		if addr, err := netip.ParseAddr(st.VIPAddress); err == nil {
			vipComms[addr] = append(vipComms[addr], st.BGPCommunities...)
		}
	}

	desired := map[netip.Prefix]route{}
	suppressed := map[netip.Addr]bool{}
	for _, a := range sp.cfg.Aggregates {
		prefix, err := netip.ParsePrefix(a.Prefix)
		if err != nil {
			return nil, err
		}
		prefix = prefix.Masked()
		covered := false
		for addr := range vipComms {
			if prefix.Contains(addr) {
				covered = true
				if a.SuppressSpecifics {
					suppressed[addr] = true
				}
			}
		}
		if !covered {
			continue
		}
		specific, err := config.ParseCommunities(a.Communities)
		if err != nil {
			return nil, err
		}
		desired[prefix] = route{communities: mergeCommunities(global, specific)}
	}
	for addr, names := range vipComms {
		if suppressed[addr] {
			continue
		}
		specific, err := config.ParseCommunities(names)
		if err != nil {
			return nil, err
		}
		desired[netip.PrefixFrom(addr, addr.BitLen())] = route{communities: mergeCommunities(global, specific)}
	}
	return desired, nil
}

// mergeCommunities concatenates lists, dropping repeats, keeping first-seen order.
func mergeCommunities(lists ...[]uint32) []uint32 {
	var out []uint32
	seen := map[uint32]bool{}
	for _, l := range lists {
		for _, c := range l {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	return out
}

func (sp *Speaker) advertise(prefix netip.Prefix, r route) (uuid.UUID, error) {
	family := bgp.RF_IPv4_UC
	if prefix.Addr().Is6() {
		family = bgp.RF_IPv6_UC
	}
	nlri, err := bgp.NewIPAddrPrefix(prefix)
	if err != nil {
		return uuid.UUID{}, err
	}
	nextHop, err := sp.nextHop(prefix.Addr())
	if err != nil {
		return uuid.UUID{}, err
	}
	nh, err := bgp.NewPathAttributeNextHop(nextHop)
	if err != nil {
		return uuid.UUID{}, err
	}
	attrs := []bgp.PathAttributeInterface{
		bgp.NewPathAttributeOrigin(0), // IGP — locally-originated route
		nh,
		bgp.NewPathAttributeAsPath(nil),
	}
	if len(r.communities) > 0 {
		attrs = append(attrs, bgp.NewPathAttributeCommunities(r.communities))
	}
	if sp.cfg.LocalPref != nil {
		attrs = append(attrs, bgp.NewPathAttributeLocalPref(*sp.cfg.LocalPref))
	}
	resp, err := sp.server.AddPath(apiutil.AddPathRequest{
		Paths: []*apiutil.Path{{Family: family, Nlri: nlri, Attrs: attrs}},
	})
	if err != nil {
		return uuid.UUID{}, err
	}
	if len(resp) != 1 {
		return uuid.UUID{}, fmt.Errorf("unexpected AddPath response count: %d", len(resp))
	}
	if resp[0].Error != nil {
		return uuid.UUID{}, resp[0].Error
	}
	return resp[0].UUID, nil
}

func (sp *Speaker) withdraw(id uuid.UUID) error {
	return sp.server.DeletePath(apiutil.DeletePathRequest{UUIDs: []uuid.UUID{id}})
}

// nextHop is the address this node advertises itself as reachable at for
// the routes it originates. IPv4 VIPs use routerId (BGP convention);
// IPv6 VIPs use ipv6NextHop when set, else an error — router-id is always
// an IPv4 address and is not a valid IPv6 next-hop.
func (sp *Speaker) nextHop(vip netip.Addr) (netip.Addr, error) {
	if vip.Is6() {
		if sp.cfg.IPv6NextHop == "" {
			return netip.Addr{}, fmt.Errorf("bgp: ipv6NextHop is required to advertise IPv6 VIP %s", vip)
		}
		addr, err := netip.ParseAddr(sp.cfg.IPv6NextHop)
		if err != nil || !addr.Is6() {
			return netip.Addr{}, fmt.Errorf("bgp: ipv6NextHop %q is not a valid IPv6 address", sp.cfg.IPv6NextHop)
		}
		return addr, nil
	}
	addr, err := netip.ParseAddr(sp.cfg.RouterID)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("router id %q is not a valid next-hop address: %w", sp.cfg.RouterID, err)
	}
	return addr, nil
}

// watchPeerEvents logs peer session-state transitions — operability only,
// not load-bearing: the resync loop's health-gating is what actually
// decides correctness, this just makes a peer going down (BGP- or
// BFD-detected) visible in rivorad's logs without needing to correlate
// against gobgpd's own state separately.
func (sp *Speaker) watchPeerEvents(ctx context.Context) {
	err := sp.server.WatchEvent(ctx, server.WatchEventMessageCallbacks{
		OnPeerUpdate: func(ev *apiutil.WatchEventMessage_PeerEvent, _ time.Time) {
			if ev.Type != apiutil.PEER_EVENT_STATE {
				return
			}
			sp.countStateChange(ev.Peer.State.NeighborAddress.String())
			sp.logger.Info("bgp peer state changed",
				"peer", ev.Peer.State.NeighborAddress,
				"state", ev.Peer.State.SessionState.String(),
			)
		},
	}, server.WatchPeer())
	if err != nil && ctx.Err() == nil {
		sp.logger.Error("watch bgp peer events", "err", err)
	}
}

func (sp *Speaker) countStateChange(peer string) {
	sp.statMu.Lock()
	sp.stateChanges[peer]++
	sp.statMu.Unlock()
}

func (sp *Speaker) countAdvertiseError() {
	sp.statMu.Lock()
	sp.advertiseErrors++
	sp.statMu.Unlock()
}

func (sp *Speaker) countWithdrawError() {
	sp.statMu.Lock()
	sp.withdrawErrors++
	sp.statMu.Unlock()
}
