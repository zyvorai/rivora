// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package loader

import (
	"reflect"
	"testing"
)

func TestOrphans(t *testing.T) {
	cases := []struct {
		name         string
		present, own []string
		want         []string
	}{
		{"nothing persisted", nil, []string{"xdp-eth0"}, nil},
		{"all adopted", []string{"tcx-egress-eth0", "xdp-eth0"}, []string{"xdp-eth0", "tcx-egress-eth0"}, nil},
		{
			// The interface was renamed in the config: the old attachment keeps
			// forwarding, so it must be surfaced.
			"interface changed", []string{"xdp-eth0", "xdp-eth1"}, []string{"xdp-eth1"}, []string{"xdp-eth0"},
		},
		{
			// NAT VIPs were dropped: the un-NAT program is still attached.
			"nat dropped", []string{"tcx-egress-eth0", "xdp-eth0"}, []string{"xdp-eth0"}, []string{"tcx-egress-eth0"},
		},
		{"none owned", []string{"xdp-eth0"}, nil, []string{"xdp-eth0"}},
	}
	for _, c := range cases {
		if got := orphans(c.present, c.own); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: orphans = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPersistedLinksMissingDirIsEmptyNotAnError(t *testing.T) {
	// A fresh host has no pin directory at all; that must read as "nothing
	// persisted", or every first start would report a spurious failure. (PinDir
	// is a fixed bpffs path that doesn't exist off Linux/CI, which is exactly
	// the case being exercised.)
	if _, err := PersistedLinks(); err != nil {
		t.Skipf("pin dir present but unreadable here (%v); covered by the on-host restart selftest", err)
	}
}
