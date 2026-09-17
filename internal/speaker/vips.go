// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package speaker

import (
	"net/netip"

	"github.com/zyvorai/rivora/internal/config"
	"github.com/zyvorai/rivora/internal/dataplane"
)

// natVIPAddresses extracts the deduped set of addresses this node should
// defend at L2 for: NAT-mode VIPs only. DSR-mode VIP ownership belongs to
// the backend that answers on the VIP directly, not the load balancer.
// Both IPv4 (ARP) and IPv6 (NDP) VIPs are included; the speaker's ARP and
// NDP loops each filter by family.
func natVIPAddresses(statuses []dataplane.Status) []netip.Addr {
	return natVIPAddressesFamily(statuses, 0) // 0 = both
}

// natVIP4 / natVIP6 are the family-filtered views the ARP and NDP loops use.
func natVIP4(statuses []dataplane.Status) []netip.Addr {
	return natVIPAddressesFamily(statuses, 4)
}

func natVIP6(statuses []dataplane.Status) []netip.Addr {
	return natVIPAddressesFamily(statuses, 6)
}

func natVIPAddressesFamily(statuses []dataplane.Status, family int) []netip.Addr {
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
		if err != nil {
			continue
		}
		switch family {
		case 4:
			if !addr.Is4() {
				continue
			}
		case 6:
			if !addr.Is6() {
				continue
			}
		}
		seen[st.VIPAddress] = true
		out = append(out, addr)
	}
	return out
}
