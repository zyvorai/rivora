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
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"reflect"
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

	// peerMu guards peers, the configuration each live gobgp peer was built from, keyed by address.
	// SetPeers diffs against it. remotePort is a test seam (see newSpeaker), kept so peers added
	// later dial the same override.
	peerMu     sync.Mutex
	peers      map[string]config.BGPPeer
	remotePort map[string]uint32
	// restrict is the routes limited to some peers (prefix -> the peers allowed), as of the last
	// resync, and denied each peer's current deny list (see peerselect.go); both under peerMu.
	restrict map[netip.Prefix][]string
	denied   map[string][]string
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

	sp := &Speaker{
		cfg:          cfg,
		server:       s,
		source:       source,
		logger:       logger,
		advertised:   map[netip.Prefix]advertisedRoute{},
		stateChanges: map[string]uint64{},
		resyncNow:    make(chan struct{}, 1),
		peers:        map[string]config.BGPPeer{},
		remotePort:   peerRemotePort,
		restrict:     map[netip.Prefix][]string{},
		denied:       map[string][]string{},
	}
	for _, p := range cfg.Peers {
		if err := sp.addPeer(ctx, p); err != nil {
			s.Stop()
			return nil, err
		}
	}
	return sp, nil
}

// addPeer builds p's gobgp peer, starts the session and records it.
func (sp *Speaker) addPeer(ctx context.Context, p config.BGPPeer) error {
	peer, err := peerConfig(sp.cfg, p)
	if err != nil {
		return fmt.Errorf("bgp peer %s: %w", p.Address, err)
	}
	if port, ok := sp.remotePort[p.Address]; ok {
		peer.Transport = &api.Transport{RemotePort: port}
	}
	// The rule that keeps limited routes from this peer goes in before the peer does, so no route is
	// exported to it ahead of the rule.
	if err := sp.createPeerLimits(ctx, p.Address, denyList(p.Address, sp.restrict)); err != nil {
		return fmt.Errorf("bgp peer %s: %w", p.Address, err)
	}
	if err := sp.server.AddPeer(ctx, &api.AddPeerRequest{Peer: peer}); err != nil {
		sp.dropPeerLimits(ctx, p.Address)
		return fmt.Errorf("add bgp peer %s: %w", p.Address, err)
	}
	sp.peers[p.Address] = p
	return nil
}

// PeerChanges is what SetPeers did.
type PeerChanges struct {
	Added, Removed, Updated int
}

// SetPeers makes the set of BGP sessions exactly desired, keyed by address: peers not yet running
// are started, running ones no longer listed are shut down (their routes are withdrawn from the
// peer), and a peer whose settings changed is restarted, which drops and re-establishes its
// session. An unchanged peer is left alone, so its session is not disturbed. It is best-effort per
// peer: one that cannot be configured does not stop the others, and the joined error names each.
// The routes Run advertises are independent of the peers, so a peer added later receives all of
// them as soon as its session is up.
func (sp *Speaker) SetPeers(desired []config.BGPPeer) (PeerChanges, error) {
	want := make(map[string]config.BGPPeer, len(desired))
	for _, p := range desired {
		want[p.Address] = p
	}

	sp.peerMu.Lock()
	defer sp.peerMu.Unlock()
	ctx := context.Background()
	var (
		res       PeerChanges
		errs      []error
		restarted = map[string]bool{} // running peers whose settings changed, torn down to be recreated
	)
	for addr, have := range sp.peers {
		w, keep := want[addr]
		if keep && reflect.DeepEqual(w, have) {
			continue
		}
		if err := sp.server.DeletePeer(ctx, &api.DeletePeerRequest{Address: addr}); err != nil {
			errs = append(errs, fmt.Errorf("remove bgp peer %s: %w", addr, err))
			continue
		}
		sp.dropPeerLimits(ctx, addr)
		delete(sp.peers, addr)
		if keep {
			restarted[addr] = true
		} else {
			res.Removed++
		}
	}
	for addr, w := range want {
		if _, running := sp.peers[addr]; running {
			continue
		}
		if err := sp.addPeer(ctx, w); err != nil {
			errs = append(errs, err)
			continue
		}
		if restarted[addr] {
			res.Updated++
		} else {
			res.Added++
		}
	}
	return res, errors.Join(errs...)
}

// PeerAddresses returns the addresses of the peers currently configured, sorted.
func (sp *Speaker) PeerAddresses() []string {
	sp.peerMu.Lock()
	defer sp.peerMu.Unlock()
	out := make([]string, 0, len(sp.peers))
	for a := range sp.peers {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
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
	// peers limits the route to the peers with these (normalised, sorted) addresses; nil means all.
	peers []string
}

func (r route) sig() string {
	parts := make([]string, len(r.communities))
	for i, c := range r.communities {
		parts[i] = fmt.Sprintf("%d", c)
	}
	sig := strings.Join(parts, ",")
	if r.peers != nil {
		sig += "|" + strings.Join(r.peers, ",")
	}
	return sig
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

	// Bring each peer's list of routes it must not receive up to date before anything is advertised
	// or changed, so a limited route never reaches a peer that must not have it. If that fails,
	// limited routes are not advertised this pass (withdrawals still go out); the next pass retries.
	denyOK := true
	if err := sp.syncDenySets(context.Background(), desired); err != nil {
		denyOK = false
		sp.logger.Error("bgp per-peer route limits", "err", err)
		sp.countAdvertiseError()
	}

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
		if r.peers != nil && !denyOK {
			continue
		}
		id, err := sp.advertise(prefix, r)
		if err != nil {
			sp.logger.Error("advertise bgp route", "prefix", prefix, "err", err)
			sp.countAdvertiseError()
			continue
		}
		sp.advertised[prefix] = advertisedRoute{id: id, sig: r.sig()}
		sp.logger.Info("advertised bgp route", "prefix", prefix, "communities", len(r.communities), "peers", peersLabel(r.peers))
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
	vipPeers := map[netip.Addr]*peerScope{}
	for _, st := range statuses {
		if !anyHealthy(st.Backends) {
			continue
		}
		if addr, err := netip.ParseAddr(st.VIPAddress); err == nil {
			vipComms[addr] = append(vipComms[addr], st.BGPCommunities...)
			if vipPeers[addr] == nil {
				vipPeers[addr] = &peerScope{}
			}
			vipPeers[addr].add(st.BGPPeers)
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
		scope := &peerScope{}
		scope.add(a.Peers)
		desired[prefix] = route{communities: mergeCommunities(global, specific), peers: scope.list()}
	}
	for addr, names := range vipComms {
		if suppressed[addr] {
			continue
		}
		specific, err := config.ParseCommunities(names)
		if err != nil {
			return nil, err
		}
		desired[netip.PrefixFrom(addr, addr.BitLen())] = route{communities: mergeCommunities(global, specific), peers: vipPeers[addr].list()}
	}
	return desired, nil
}

// peerScope accumulates which peers a route goes to when several VIPs share its address: every peer
// any of them names, or all peers as soon as one names none.
type peerScope struct {
	all   bool
	named map[string]bool
}

func (ps *peerScope) add(peers []string) {
	if len(peers) == 0 {
		ps.all = true
		return
	}
	if ps.named == nil {
		ps.named = map[string]bool{}
	}
	for _, p := range peers {
		ps.named[normAddr(p)] = true
	}
}

// list is the sorted peer list, or nil for "every peer".
func (ps *peerScope) list() []string {
	if ps == nil || ps.all || len(ps.named) == 0 {
		return nil
	}
	out := make([]string, 0, len(ps.named))
	for p := range ps.named {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func peersLabel(peers []string) string {
	if peers == nil {
		return "all"
	}
	return strings.Join(peers, ",")
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
