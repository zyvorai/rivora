// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package dataplane

import "testing"

func TestIDAllocatorAllocIsStableAndIdempotent(t *testing.T) {
	a := newIDAllocator(10)

	id1, err := a.Alloc("10.0.0.1:80")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := a.Alloc("10.0.0.2:80")
	if err != nil {
		t.Fatal(err)
	}
	if id1 == id2 {
		t.Fatalf("distinct keys got the same ID %d", id1)
	}

	again, err := a.Alloc("10.0.0.1:80")
	if err != nil {
		t.Fatal(err)
	}
	if again != id1 {
		t.Errorf("re-allocating an existing key returned %d, want stable %d", again, id1)
	}
}

func TestIDAllocatorGet(t *testing.T) {
	a := newIDAllocator(10)
	if _, ok := a.Get("unknown"); ok {
		t.Error("Get on unallocated key returned ok=true")
	}
	id, _ := a.Alloc("known")
	got, ok := a.Get("known")
	if !ok || got != id {
		t.Errorf("Get(%q) = (%d, %v), want (%d, true)", "known", got, ok, id)
	}
}

func TestIDAllocatorReleaseAndReuse(t *testing.T) {
	a := newIDAllocator(2)

	id1, _ := a.Alloc("a")
	id2, _ := a.Alloc("b")

	if _, err := a.Alloc("c"); err == nil {
		t.Fatal("expected capacity exhaustion error allocating a 3rd ID with max=2")
	}

	a.Release("a")
	id3, err := a.Alloc("c")
	if err != nil {
		t.Fatalf("Alloc after Release: %v", err)
	}
	if id3 != id1 {
		t.Errorf("freed ID %d was not reused (got %d)", id1, id3)
	}
	if _, ok := a.Get("a"); ok {
		t.Error(`released key "a" still resolves via Get`)
	}
	_ = id2
}

func TestIDAllocatorReleaseUnknownKeyIsNoop(t *testing.T) {
	a := newIDAllocator(10)
	a.Release("never-allocated") // must not panic
}
