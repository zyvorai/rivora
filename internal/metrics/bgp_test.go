// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/zyvorai/rivora/internal/bgp"
)

type fakeBGP struct{ snap bgp.Snapshot }

func (f fakeBGP) Snapshot() bgp.Snapshot { return f.snap }

func TestBGPCollector(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewBGPCollector(fakeBGP{snap: bgp.Snapshot{
		Peers: []bgp.PeerStatus{
			{Address: "10.0.0.1", ASN: 65000, State: "ESTABLISHED", Established: true, StateChanges: 4},
			{Address: "10.0.0.2", ASN: 65001, State: "ACTIVE", Established: false, StateChanges: 11},
		},
		AdvertisedIPv4: 3, AdvertisedIPv6: 1, AdvertiseErrors: 2, WithdrawErrors: 0,
	}}))

	for name, want := range map[string]string{
		"rivora_bgp_peer_up": `
# HELP rivora_bgp_peer_up Whether the BGP session to a configured peer is ESTABLISHED (1) or not (0). A down session means that peer is not receiving this node's VIP routes.
# TYPE rivora_bgp_peer_up gauge
rivora_bgp_peer_up{asn="65000",peer="10.0.0.1"} 1
rivora_bgp_peer_up{asn="65001",peer="10.0.0.2"} 0
`,
		"rivora_bgp_peer_state_changes_total": `
# HELP rivora_bgp_peer_state_changes_total BGP session-state transitions observed for a peer since rivorad started. A rising value on a peer that is mostly up is a flapping session.
# TYPE rivora_bgp_peer_state_changes_total counter
rivora_bgp_peer_state_changes_total{peer="10.0.0.1"} 4
rivora_bgp_peer_state_changes_total{peer="10.0.0.2"} 11
`,
		"rivora_bgp_advertised_routes": `
# HELP rivora_bgp_advertised_routes VIP host routes currently advertised (a VIP is advertised only while it has a healthy backend), by address family.
# TYPE rivora_bgp_advertised_routes gauge
rivora_bgp_advertised_routes{family="ipv4"} 3
rivora_bgp_advertised_routes{family="ipv6"} 1
`,
		"rivora_bgp_route_update_errors_total": `
# HELP rivora_bgp_route_update_errors_total Failed attempts to advertise or withdraw a VIP route since rivorad started. A value that keeps rising means routes are not reaching the peers.
# TYPE rivora_bgp_route_update_errors_total counter
rivora_bgp_route_update_errors_total{op="advertise"} 2
rivora_bgp_route_update_errors_total{op="withdraw"} 0
`,
	} {
		if err := testutil.GatherAndCompare(reg, strings.NewReader(want), name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
