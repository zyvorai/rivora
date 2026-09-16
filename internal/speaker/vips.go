// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package speaker

import (
	"net/netip"

	"github.com/zyvorai/rivora/internal/config"
	"github.com/zyvorai/rivora/internal/dataplane"
)

// natVIPAddresses extracts the deduped set of IPv4 addresses this node
// should defend ARP for: NAT-mode VIPs only. DSR-mode VIP ownership
// belongs to the backend pod that answers on the VIP directly (v0.1's
// model), not the load balancer, so the speaker has nothing to announce
// for those — see the package doc.
func natVIPAddresses(statuses []dataplane.Status) []netip.Addr {
	seen := map[string]bool{}
	var out []netip.Addr
	for _, st := range statuses {
		if st.Mode != string(config.ModeNAT) {
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
