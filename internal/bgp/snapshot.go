// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package bgp

import (
	"context"
	"strings"
	"time"

	"github.com/osrg/gobgp/v4/api"
)

// PeerStatus is one configured peer's session, as seen at the moment of a
// Snapshot.
type PeerStatus struct {
	Address string
	ASN     uint32
	// State is gobgp's session state ("ESTABLISHED", "ACTIVE", "IDLE", ...), or
	// "UNKNOWN" if gobgp didn't report the peer at all.
	State       string
	Established bool
	// StateChanges counts session-state transitions observed since the speaker
	// started (established -> idle -> active -> ... each count). A peer that
	// flaps shows as a rising count while Established is still 1 at most scrapes.
	StateChanges uint64
}

// Snapshot is what an operator needs to judge BGP health: are the sessions up,
// how many VIP routes are actually advertised, and are route updates failing.
type Snapshot struct {
	Peers          []PeerStatus
	AdvertisedIPv4 int // /32 VIP routes currently advertised
	AdvertisedIPv6 int // /128 VIP routes currently advertised
	// Advertise/withdraw failures since start. A resync retries on its next pass,
	// so a nonzero value that stops rising was transient; one that keeps rising
	// means routes are not reaching the peer.
	AdvertiseErrors uint64
	WithdrawErrors  uint64
}

// snapshotPeerTimeout bounds the ListPeer call so a wedged gobgp can't hang a
// Prometheus scrape.
const snapshotPeerTimeout = 2 * time.Second

// Snapshot reads the speaker's current state. Session states come from gobgp at
// call time; the counters and the advertised set are the speaker's own.
func (sp *Speaker) Snapshot() Snapshot {
	states := map[string]*api.Peer{}
	ctx, cancel := context.WithTimeout(context.Background(), snapshotPeerTimeout)
	defer cancel()
	_ = sp.server.ListPeer(ctx, &api.ListPeerRequest{}, func(p *api.Peer) {
		if p != nil && p.State != nil {
			states[p.State.NeighborAddress] = p
		}
	})

	sp.statMu.Lock()
	changes := make(map[string]uint64, len(sp.stateChanges))
	for k, v := range sp.stateChanges {
		changes[k] = v
	}
	snap := Snapshot{AdvertiseErrors: sp.advertiseErrors, WithdrawErrors: sp.withdrawErrors}
	sp.statMu.Unlock()

	// Configured peers in configured order, so the output is stable and a peer
	// gobgp failed to report still appears (as down) instead of vanishing.
	for _, cp := range sp.cfg.Peers {
		ps := PeerStatus{Address: cp.Address, ASN: cp.ASN, State: "UNKNOWN", StateChanges: changes[cp.Address]}
		if p, ok := states[cp.Address]; ok {
			// gobgp's enum prints as SESSION_STATE_ESTABLISHED; drop the prefix.
			ps.State = strings.TrimPrefix(p.State.SessionState.String(), "SESSION_STATE_")
			ps.Established = p.State.SessionState == api.PeerState_SESSION_STATE_ESTABLISHED
		}
		snap.Peers = append(snap.Peers, ps)
	}

	sp.mu.Lock()
	for addr := range sp.advertised {
		if addr.Is6() {
			snap.AdvertisedIPv6++
		} else {
			snap.AdvertisedIPv4++
		}
	}
	sp.mu.Unlock()
	return snap
}
