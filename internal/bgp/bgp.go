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
	advertised map[netip.Addr]uuid.UUID // VIP address -> the gobgp path UUID currently advertised for it

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
		peer := &api.Peer{
			Conf: &api.PeerConf{
				NeighborAddress: p.Address,
				PeerAsn:         p.ASN,
				LocalAsn:        cfg.ASN,
			},
			// Negotiate both unicast families so IPv4 /32 and IPv6 /128
			// VIP host routes can be advertised on the same session.
			AfiSafis: dualStackAfiSafis(),
		}
		if p.BFD {
			peer.Bfd = &api.BfdPeerConfig{Enabled: true}
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
		advertised:   map[netip.Addr]uuid.UUID{},
		stateChanges: map[string]uint64{},
		resyncNow:    make(chan struct{}, 1),
	}, nil
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

// resync diffs the currently-healthy VIP set against what's advertised
// and advertises/withdraws the difference.
func (sp *Speaker) resync() {
	statuses, err := sp.source.Statuses()
	if err != nil {
		sp.logger.Error("list vip statuses for bgp resync", "err", err)
		return
	}
	desired := map[netip.Addr]bool{}
	for _, addr := range healthyVIPAddresses(statuses) {
		desired[addr] = true
	}

	sp.mu.Lock()
	defer sp.mu.Unlock()

	for addr := range desired {
		if _, ok := sp.advertised[addr]; ok {
			continue
		}
		id, err := sp.advertise(addr)
		if err != nil {
			sp.logger.Error("advertise bgp route", "vip", addr, "err", err)
			sp.countAdvertiseError()
			continue
		}
		sp.advertised[addr] = id
		sp.logger.Info("advertised bgp route", "vip", addr)
	}

	for addr, id := range sp.advertised {
		if desired[addr] {
			continue
		}
		if err := sp.withdraw(id); err != nil {
			sp.logger.Error("withdraw bgp route", "vip", addr, "err", err)
			sp.countWithdrawError()
			continue
		}
		delete(sp.advertised, addr)
		sp.logger.Info("withdrew bgp route", "vip", addr)
	}
}

func (sp *Speaker) advertise(addr netip.Addr) (uuid.UUID, error) {
	bits := 32
	family := bgp.RF_IPv4_UC
	if addr.Is6() {
		bits = 128
		family = bgp.RF_IPv6_UC
	}
	nlri, err := bgp.NewIPAddrPrefix(netip.PrefixFrom(addr, bits))
	if err != nil {
		return uuid.UUID{}, err
	}
	nextHop, err := sp.nextHop(addr)
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
