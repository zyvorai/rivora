// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package maglev

import "testing"

func unweighted(names ...string) []Backend {
	out := make([]Backend, len(names))
	for i, n := range names {
		out[i] = Backend{Name: n}
	}
	return out
}

func TestBuildTableFillsEverySlot(t *testing.T) {
	table, err := BuildTable(1009, unweighted("10.0.0.11:80", "10.0.0.12:80", "10.0.0.13:80"))
	if err != nil {
		t.Fatal(err)
	}
	if len(table) != 1009 {
		t.Fatalf("len(table) = %d, want 1009", len(table))
	}
	counts := map[int]int{}
	for _, backend := range table {
		if backend < 0 || backend >= 3 {
			t.Fatalf("slot assigned out-of-range backend %d", backend)
		}
		counts[backend]++
	}
	if len(counts) != 3 {
		t.Fatalf("expected all 3 backends represented, got %v", counts)
	}
	for b, c := range counts {
		if c < 300 || c > 400 {
			t.Errorf("backend %d got %d slots of 1009, distribution too skewed", b, c)
		}
	}
}

func TestBuildTableStableAcrossRebuilds(t *testing.T) {
	backends := unweighted("10.0.0.11:80", "10.0.0.12:80", "10.0.0.13:80")
	t1, err := BuildTable(1009, backends)
	if err != nil {
		t.Fatal(err)
	}
	t2, err := BuildTable(1009, backends)
	if err != nil {
		t.Fatal(err)
	}
	for i := range t1 {
		if t1[i] != t2[i] {
			t.Fatalf("slot %d differs across rebuilds: %d vs %d", i, t1[i], t2[i])
		}
	}
}

func TestBuildTableRejectsTooFewSlots(t *testing.T) {
	if _, err := BuildTable(2, unweighted("a", "b", "c")); err == nil {
		t.Fatal("expected error when table size <= backend count")
	}
}

func TestBuildTableWeightZeroMatchesWeightOne(t *testing.T) {
	explicit := []Backend{{Name: "10.0.0.11:80", Weight: 1}, {Name: "10.0.0.12:80", Weight: 1}}
	implicit := []Backend{{Name: "10.0.0.11:80"}, {Name: "10.0.0.12:80"}}
	t1, err := BuildTable(1009, explicit)
	if err != nil {
		t.Fatal(err)
	}
	t2, err := BuildTable(1009, implicit)
	if err != nil {
		t.Fatal(err)
	}
	for i := range t1 {
		if t1[i] != t2[i] {
			t.Fatalf("slot %d differs between weight=0 and weight=1: %d vs %d", i, t1[i], t2[i])
		}
	}
}

func TestBuildTableWeightedSplit(t *testing.T) {
	backends := []Backend{
		{Name: "canary-new", Weight: 1},
		{Name: "stable-old", Weight: 9},
	}
	table, err := BuildTable(1009, backends)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[int]int{}
	for _, b := range table {
		counts[b]++
	}
	// Expect roughly a 10%/90% split (1009/10 ~= 101, 1009*9/10 ~= 908),
	// with a generous tolerance band since Maglev's distribution is
	// statistical, not exact.
	newShare, oldShare := counts[0], counts[1]
	if newShare < 60 || newShare > 160 {
		t.Errorf("weight=1 backend got %d/1009 slots, want roughly ~100 (10%%)", newShare)
	}
	if oldShare < 850 || oldShare > 960 {
		t.Errorf("weight=9 backend got %d/1009 slots, want roughly ~908 (90%%)", oldShare)
	}
	if newShare+oldShare != 1009 {
		t.Fatalf("counts don't sum to table size: %d + %d != 1009", newShare, oldShare)
	}
}

func TestBuildTableRejectsWeightSumExceedingTableSize(t *testing.T) {
	backends := []Backend{{Name: "a", Weight: 5}, {Name: "b", Weight: 5}}
	if _, err := BuildTable(9, backends); err == nil {
		t.Fatal("expected error when total backend weight >= table size")
	}
}
