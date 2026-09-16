// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package ipam

import (
	"strings"
	"testing"
)

func poolOf(t *testing.T, cidr string, autoAssign bool) PoolSpec {
	t.Helper()
	addrs, err := ExpandPool([]string{cidr}, false)
	if err != nil {
		t.Fatal(err)
	}
	return PoolSpec{Addresses: addrs, AutoAssign: autoAssign}
}

func TestAllocatorAllocateIsIdempotent(t *testing.T) {
	a := NewAllocator()
	a.SetPools(map[string]PoolSpec{"default": poolOf(t, "10.0.0.0/30", true)})

	ip1, err := a.Allocate("ns/svc", "", "")
	if err != nil {
		t.Fatal(err)
	}
	ip2, err := a.Allocate("ns/svc", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if ip1 != ip2 {
		t.Errorf("re-allocating the same key returned %q then %q, want stable", ip1, ip2)
	}
}

func TestAllocatorDistinctKeysGetDistinctAddresses(t *testing.T) {
	a := NewAllocator()
	a.SetPools(map[string]PoolSpec{"default": poolOf(t, "10.0.0.0/30", true)})

	ip1, err := a.Allocate("ns/a", "", "")
	if err != nil {
		t.Fatal(err)
	}
	ip2, err := a.Allocate("ns/b", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if ip1 == ip2 {
		t.Errorf("two different keys both got %q", ip1)
	}
}

func TestAllocatorExhaustion(t *testing.T) {
	a := NewAllocator()
	a.SetPools(map[string]PoolSpec{"default": poolOf(t, "10.0.0.0/31", true)}) // 2 addresses

	if _, err := a.Allocate("ns/a", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Allocate("ns/b", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Allocate("ns/c", "", ""); err == nil {
		t.Error("expected exhaustion error allocating a 3rd address from a 2-address pool")
	}
}

func TestAllocatorReleaseFreesForReuse(t *testing.T) {
	a := NewAllocator()
	a.SetPools(map[string]PoolSpec{"default": poolOf(t, "10.0.0.0/31", true)})

	ip1, _ := a.Allocate("ns/a", "", "")
	a.Release("ns/a")
	ip2, err := a.Allocate("ns/b", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if ip2 != ip1 {
		t.Errorf("released address %q was not reused (got %q)", ip1, ip2)
	}
}

func TestAllocatorReleaseUnknownKeyIsNoop(t *testing.T) {
	a := NewAllocator()
	a.Release("never/allocated") // must not panic
}

func TestAllocatorPreferredPool(t *testing.T) {
	a := NewAllocator()
	a.SetPools(map[string]PoolSpec{
		"a": poolOf(t, "10.0.0.0/30", true),
		"b": poolOf(t, "10.0.1.0/30", true),
	})

	ip, err := a.Allocate("ns/svc", "b", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ip, "10.0.1.") {
		t.Errorf("Allocate with preferredPool=b returned %q, want an address from 10.0.1.0/30", ip)
	}
}

func TestAllocatorPreferredPoolNotFound(t *testing.T) {
	a := NewAllocator()
	a.SetPools(map[string]PoolSpec{"default": poolOf(t, "10.0.0.0/30", true)})
	if _, err := a.Allocate("ns/svc", "nonexistent", ""); err == nil {
		t.Error("expected error for a preferredPool that doesn't exist")
	}
}

func TestAllocatorPinnedIP(t *testing.T) {
	a := NewAllocator()
	a.SetPools(map[string]PoolSpec{"default": poolOf(t, "10.0.0.0/30", true)})

	ip, err := a.Allocate("ns/svc", "", "10.0.0.2")
	if err != nil {
		t.Fatal(err)
	}
	if ip != "10.0.0.2" {
		t.Errorf("pinned IP request returned %q, want 10.0.0.2", ip)
	}
}

func TestAllocatorPinnedIPOutsideAnyPoolRejected(t *testing.T) {
	a := NewAllocator()
	a.SetPools(map[string]PoolSpec{"default": poolOf(t, "10.0.0.0/30", true)})
	if _, err := a.Allocate("ns/svc", "", "192.168.1.1"); err == nil {
		t.Error("expected error for a pinned IP outside every pool")
	}
}

func TestAllocatorPinnedIPAlreadyTakenByAnotherKeyRejected(t *testing.T) {
	a := NewAllocator()
	a.SetPools(map[string]PoolSpec{"default": poolOf(t, "10.0.0.0/30", true)})
	if _, err := a.Allocate("ns/a", "", "10.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Allocate("ns/b", "", "10.0.0.1"); err == nil {
		t.Error("expected error requesting an IP already pinned to a different key")
	}
}

func TestAllocatorNonAutoAssignPoolNotUsedByDefault(t *testing.T) {
	a := NewAllocator()
	a.SetPools(map[string]PoolSpec{"manual": poolOf(t, "10.0.0.0/30", false)})
	if _, err := a.Allocate("ns/svc", "", ""); err == nil {
		t.Error("expected error: no auto-assign pool exists, so a plain Allocate() should fail")
	}
	// But requesting it explicitly by name still works.
	if _, err := a.Allocate("ns/svc", "manual", ""); err != nil {
		t.Errorf("explicit pool request to a non-auto-assign pool should still succeed: %v", err)
	}
}

func TestAllocatorReserveRebuildsStateAndBlocksReallocation(t *testing.T) {
	a := NewAllocator()
	a.SetPools(map[string]PoolSpec{"default": poolOf(t, "10.0.0.0/30", true)})

	if err := a.Reserve("10.0.0.1", "ns/existing"); err != nil {
		t.Fatal(err)
	}
	ip, err := a.Allocate("ns/new", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if ip == "10.0.0.1" {
		t.Error("Allocate handed out an address already Reserve()d by a different key")
	}

	// Reserve is idempotent for the *same* key.
	if err := a.Reserve("10.0.0.1", "ns/existing"); err != nil {
		t.Errorf("re-Reserve of the same key/address should not error: %v", err)
	}
	// But conflicts with a different key.
	if err := a.Reserve("10.0.0.1", "ns/other"); err == nil {
		t.Error("expected error reserving an address already reserved by a different key")
	}
}

func TestAllocatorCounts(t *testing.T) {
	a := NewAllocator()
	a.SetPools(map[string]PoolSpec{"default": poolOf(t, "10.0.0.0/30", true)}) // 4 addresses

	if avail, assigned := a.Counts("default"); avail != 4 || assigned != 0 {
		t.Errorf("initial Counts = (%d, %d), want (4, 0)", avail, assigned)
	}
	if _, err := a.Allocate("ns/a", "", ""); err != nil {
		t.Fatal(err)
	}
	if avail, assigned := a.Counts("default"); avail != 3 || assigned != 1 {
		t.Errorf("Counts after one allocation = (%d, %d), want (3, 1)", avail, assigned)
	}
}
