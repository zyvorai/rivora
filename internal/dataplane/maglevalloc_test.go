// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package dataplane

import "testing"

func TestMaglevClassFor(t *testing.T) {
	cases := []struct {
		backends int
		want     uint32
	}{
		{0, 1031},
		{1, 1031},
		{10, 1031},      // 10*100=1000 < 1024 floor -> smallest class
		{11, 4099},      // 11*100=1100 > 1031 -> next class up
		{100, 16411},    // 100*100=10000 -> smallest class >= 10000
		{1000, 65537},   // 1000*100=100000, exceeds every class but the last
		{100000, 65537}, // exceeds every class -> largest available, not an error
	}
	for _, c := range cases {
		if got := maglevClassFor(c.backends); got != c.want {
			t.Errorf("maglevClassFor(%d) = %d, want %d", c.backends, got, c.want)
		}
	}
}

func TestExtentAllocatorAllocDisjoint(t *testing.T) {
	a := newExtentAllocator(1031)

	e1, err := a.Alloc(400)
	if err != nil {
		t.Fatal(err)
	}
	e2, err := a.Alloc(400)
	if err != nil {
		t.Fatal(err)
	}
	if e1.offset == e2.offset {
		t.Fatalf("two allocations got the same offset %d", e1.offset)
	}
	if overlap(e1, e2) {
		t.Errorf("extents overlap: %+v, %+v", e1, e2)
	}
}

func TestExtentAllocatorExhaustion(t *testing.T) {
	a := newExtentAllocator(1000)
	if _, err := a.Alloc(600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Alloc(500); err == nil {
		t.Fatal("expected exhaustion error allocating 500 when only 400 remain")
	}
}

func TestExtentAllocatorFreeCoalescesAdjacentExtents(t *testing.T) {
	a := newExtentAllocator(1000)

	e1, _ := a.Alloc(300)
	e2, _ := a.Alloc(300)
	e3, _ := a.Alloc(300)

	a.Free(e1)
	a.Free(e2)
	a.Free(e3)

	// The whole range should be one coalesced free extent again, so a
	// single allocation of the full original size must succeed.
	if _, err := a.Alloc(1000); err != nil {
		t.Errorf("expected freed extents to coalesce back into one 1000-sized block: %v", err)
	}
}

func TestExtentAllocatorFreeThenReallocDoesNotOverlapLiveExtent(t *testing.T) {
	a := newExtentAllocator(1000)

	e1, _ := a.Alloc(300) // [0,300)
	e2, _ := a.Alloc(300) // [300,600), stays live
	a.Free(e1)

	e3, err := a.Alloc(300) // should reuse [0,300), not touch e2's range
	if err != nil {
		t.Fatal(err)
	}
	if overlap(e3, e2) {
		t.Errorf("reallocated extent %+v overlaps still-live extent %+v", e3, e2)
	}
}

func overlap(a, b extent) bool {
	return a.offset < b.offset+b.size && b.offset < a.offset+a.size
}
