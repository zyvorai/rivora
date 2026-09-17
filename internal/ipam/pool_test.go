// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package ipam

import (
	"net/netip"
	"reflect"
	"testing"
)

func TestExpandPoolCIDR(t *testing.T) {
	got, err := ExpandPool([]string{"10.0.0.0/30"}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.0.0.0", "10.0.0.1", "10.0.0.2", "10.0.0.3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExpandPoolCIDRAvoidBuggyIPs(t *testing.T) {
	got, err := ExpandPool([]string{"10.0.0.0/29"}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, ip := range got {
		if ip == "10.0.0.0" {
			t.Error("avoidBuggyIPs should have skipped the network address 10.0.0.0")
		}
	}
	// /29 = 8 addresses, minus .0 = 7.
	if len(got) != 7 {
		t.Errorf("len(got) = %d, want 7", len(got))
	}
}

func TestExpandPoolRange(t *testing.T) {
	got, err := ExpandPool([]string{"10.0.0.10-10.0.0.12"}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.0.0.10", "10.0.0.11", "10.0.0.12"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExpandPoolSingleAddress(t *testing.T) {
	got, err := ExpandPool([]string{"10.0.0.5"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"10.0.0.5"}) {
		t.Errorf("got %v", got)
	}
}

func TestExpandPoolDedupsAcrossOverlappingSpecs(t *testing.T) {
	got, err := ExpandPool([]string{"10.0.0.0/30", "10.0.0.2-10.0.0.5"}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.0.0.0", "10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExpandPoolRejectsInvalidRange(t *testing.T) {
	if _, err := ExpandPool([]string{"10.0.0.20-10.0.0.10"}, false); err == nil {
		t.Error("expected error for start > end")
	}
	if _, err := ExpandPool([]string{"not-an-ip"}, false); err == nil {
		t.Error("expected error for garbage input")
	}
}

func TestParsePoolIPv6SmallPrefix(t *testing.T) {
	got, err := ParsePool([]string{"2001:db8::/126"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Prefixes) != 0 {
		t.Fatalf("expected eager expand for /126, got prefixes %v", got.Prefixes)
	}
	if len(got.Addresses) != 4 {
		t.Fatalf("got %d addresses, want 4: %v", len(got.Addresses), got.Addresses)
	}
}

func TestParsePoolIPv6SparsePrefix(t *testing.T) {
	got, err := ParsePool([]string{"2001:db8::/64"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Addresses) != 0 {
		t.Fatalf("expected no eager addresses for /64, got %v", got.Addresses)
	}
	if len(got.Prefixes) != 1 || got.Prefixes[0].String() != "2001:db8::/64" {
		t.Fatalf("got prefixes %v, want 2001:db8::/64", got.Prefixes)
	}
}

func TestExpandPoolRejectsSparseIPv6(t *testing.T) {
	if _, err := ExpandPool([]string{"2001:db8::/64"}, false); err == nil {
		t.Error("expected ExpandPool to reject sparse IPv6 /64")
	}
}

func TestAllocatorSparseIPv6(t *testing.T) {
	a := NewAllocator()
	a.SetPools(map[string]PoolSpec{"v6": poolOf(t, "2001:db8::/64", true)})
	ip, err := a.Allocate("ns/svc", "", "")
	if err != nil {
		t.Fatal(err)
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil || !addr.Is6() {
		t.Fatalf("expected an IPv6 allocation, got %q", ip)
	}
	if !netip.MustParsePrefix("2001:db8::/64").Contains(addr) {
		t.Fatalf("allocated %s outside 2001:db8::/64", ip)
	}
}
