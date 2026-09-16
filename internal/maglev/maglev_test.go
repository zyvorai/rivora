// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package maglev

import "testing"

func TestBuildTableFillsEverySlot(t *testing.T) {
	table, err := BuildTable(1009, []string{"10.0.0.11:80", "10.0.0.12:80", "10.0.0.13:80"})
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
	names := []string{"10.0.0.11:80", "10.0.0.12:80", "10.0.0.13:80"}
	t1, err := BuildTable(1009, names)
	if err != nil {
		t.Fatal(err)
	}
	t2, err := BuildTable(1009, names)
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
	if _, err := BuildTable(2, []string{"a", "b", "c"}); err == nil {
		t.Fatal("expected error when table size <= backend count")
	}
}
