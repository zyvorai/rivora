// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package bgp

import (
	"net/netip"
	"testing"

	"github.com/zyvorai/rivora/internal/dataplane"
)

func addrs(ss ...string) []netip.Addr {
	out := make([]netip.Addr, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParseAddr(s)
	}
	return out
}

func TestHealthyVIPAddressesEmpty(t *testing.T) {
	got := healthyVIPAddresses(nil)
	if len(got) != 0 {
		t.Fatalf("expected no addresses for nil statuses, got %v", got)
	}
}

func TestHealthyVIPAddressesAllUnhealthy(t *testing.T) {
	statuses := []dataplane.Status{
		{VIPAddress: "10.0.0.1", Backends: []dataplane.BackendStatus{{Healthy: false}, {Healthy: false}}},
	}
	got := healthyVIPAddresses(statuses)
	if len(got) != 0 {
		t.Fatalf("expected no addresses when every backend is unhealthy, got %v", got)
	}
}

func TestHealthyVIPAddressesNoBackends(t *testing.T) {
	statuses := []dataplane.Status{
		{VIPAddress: "10.0.0.1", Backends: nil},
	}
	got := healthyVIPAddresses(statuses)
	if len(got) != 0 {
		t.Fatalf("expected no addresses for a VIP with zero backends, got %v", got)
	}
}

func TestHealthyVIPAddressesMixed(t *testing.T) {
	statuses := []dataplane.Status{
		{VIPAddress: "10.0.0.1", Backends: []dataplane.BackendStatus{{Healthy: false}, {Healthy: true}}},
		{VIPAddress: "10.0.0.2", Backends: []dataplane.BackendStatus{{Healthy: false}}},
	}
	got := healthyVIPAddresses(statuses)
	want := addrs("10.0.0.1")
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestHealthyVIPAddressesDedupesAcrossStatuses(t *testing.T) {
	statuses := []dataplane.Status{
		{VIPAddress: "10.0.0.1", Protocol: "tcp", Backends: []dataplane.BackendStatus{{Healthy: true}}},
		{VIPAddress: "10.0.0.1", Protocol: "udp", Backends: []dataplane.BackendStatus{{Healthy: true}}},
	}
	got := healthyVIPAddresses(statuses)
	if len(got) != 1 {
		t.Fatalf("expected the same VIP address across two protocols to dedupe to one entry, got %v", got)
	}
}

func TestHealthyVIPAddressesIncludesIPv6(t *testing.T) {
	statuses := []dataplane.Status{
		{VIPAddress: "2001:db8::1", Backends: []dataplane.BackendStatus{{Healthy: true}}},
		{VIPAddress: "10.0.0.1", Backends: []dataplane.BackendStatus{{Healthy: true}}},
	}
	got := healthyVIPAddresses(statuses)
	if len(got) != 2 {
		t.Fatalf("got %v, want both IPv4 and IPv6 addresses", got)
	}
}

func TestHealthyVIPAddressesBothModesEligible(t *testing.T) {
	statuses := []dataplane.Status{
		{VIPAddress: "10.0.0.1", Mode: "dsr", Backends: []dataplane.BackendStatus{{Healthy: true}}},
		{VIPAddress: "10.0.0.2", Mode: "nat", Backends: []dataplane.BackendStatus{{Healthy: true}}},
	}
	got := healthyVIPAddresses(statuses)
	if len(got) != 2 {
		t.Fatalf("expected both DSR- and NAT-mode healthy VIPs to be eligible, got %v", got)
	}
}
