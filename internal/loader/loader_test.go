// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package loader

import (
	"reflect"
	"testing"

	"github.com/zyvorai/rivora/internal/config"
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

func modes(ms ...config.XDPMode) []config.XDPMode { return ms }

func TestPlanXDP(t *testing.T) {
	n, g, a := config.XDPNative, config.XDPGeneric, config.XDPAuto
	cases := []struct {
		name      string
		persist   bool
		req       config.XDPMode
		present   map[config.XDPMode]bool
		wantAdopt []config.XDPMode
		wantFresh []config.XDPMode
	}{
		{"generic, nothing pinned", true, g, nil, nil, modes(g)},
		{"default (empty) means generic", true, "", nil, nil, modes(g)},
		{"generic adopts its own pinned link", true, g, map[config.XDPMode]bool{g: true}, modes(g), modes(g)},
		{
			// Native asked for, only a generic link pinned: the mode can't change in
			// place, so nothing is adopted and the generic link is dropped by the
			// caller before a fresh native attach.
			"mode change generic->native adopts nothing", true, n, map[config.XDPMode]bool{g: true}, nil, modes(n),
		},
		{"mode change native->generic adopts nothing", true, g, map[config.XDPMode]bool{n: true}, nil, modes(g)},
		{"auto, nothing pinned: native then generic", true, a, nil, nil, modes(n, g)},
		{"auto prefers a pinned native link", true, a, map[config.XDPMode]bool{n: true}, modes(n), modes(n, g)},
		{
			// Auto fell back to generic last time. Keep that link; retrying native
			// would detach it (a gap) just to fail the same way.
			"auto keeps a pinned generic link", true, a, map[config.XDPMode]bool{g: true}, modes(g), modes(n, g),
		},
		{"auto with both pinned prefers native", true, a, map[config.XDPMode]bool{n: true, g: true}, modes(n, g), modes(n, g)},
		{"without persist nothing is ever adopted", false, a, map[config.XDPMode]bool{n: true, g: true}, nil, modes(n, g)},
		{"without persist, generic", false, g, map[config.XDPMode]bool{g: true}, nil, modes(g)},
	}
	for _, c := range cases {
		got := planXDP(c.persist, c.req, c.present)
		if !reflect.DeepEqual(got.adopt, c.wantAdopt) || !reflect.DeepEqual(got.fresh, c.wantFresh) {
			t.Errorf("%s: adopt=%v fresh=%v, want adopt=%v fresh=%v", c.name, got.adopt, got.fresh, c.wantAdopt, c.wantFresh)
		}
	}
}

func TestXDPPinNameCarriesTheMode(t *testing.T) {
	// The mode is in the name so generic and native links to one interface are
	// distinct pins: a mode change must not be mistaken for the same link.
	g, n := xdpPinName(config.XDPGeneric, "eth0"), xdpPinName(config.XDPNative, "eth0")
	if g == n {
		t.Fatal("generic and native pins for one interface share a name")
	}
	if g != "xdp-generic-eth0" || n != "xdp-native-eth0" {
		t.Errorf("pin names = %q, %q", g, n)
	}
}
