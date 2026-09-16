// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package bgp

import (
	"net/netip"

	"github.com/zyvorai/rivora/internal/dataplane"
)

// healthyVIPAddresses extracts the deduped set of IPv4 addresses this node
// should currently be advertising: every VIP with at least one healthy
// backend, regardless of DSR/NAT mode — unlike internal/speaker's
// mode-gated natVIPAddresses, BGP+ECMP operates at the routing layer
// above either forwarding mode, so both are eligible here. A VIP with
// zero backends, or with backends that are all unhealthy, is excluded —
// that's the health-gating this whole package exists to implement.
func healthyVIPAddresses(statuses []dataplane.Status) []netip.Addr {
	seen := map[string]bool{}
	var out []netip.Addr
	for _, st := range statuses {
		if !anyHealthy(st.Backends) {
			continue
		}
		if seen[st.VIPAddress] {
			continue
		}
		addr, err := netip.ParseAddr(st.VIPAddress)
		if err != nil || !addr.Is4() {
			continue
		}
		seen[st.VIPAddress] = true
		out = append(out, addr)
	}
	return out
}

func anyHealthy(backends []dataplane.BackendStatus) bool {
	for _, b := range backends {
		if b.Healthy {
			return true
		}
	}
	return false
}
