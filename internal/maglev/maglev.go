// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package maglev builds a Maglev consistent-hashing lookup table: Google's
// "Maglev: A Fast and Reliable Software Network Load Balancer" (NSDI 2016),
// section 3.4. A small permutation table size (M) is used so tests run
// quickly; production sizing should use a prime well above 100x the largest
// expected backend count (the BPF-side table is fixed at rivora.MaglevM).
package maglev

import (
	"fmt"
	"hash/fnv"
)

// Backend is one entry BuildTable distributes table slots to. Weight is
// its relative share of the table (0 is normalized to 1, matching an
// unweighted backend) — see BuildTable's doc for how it's implemented.
type Backend struct {
	Name   string
	Weight uint32
}

// NormalizeWeight applies BuildTable's "0 means 1" rule — exported so
// callers that need to know a backend's *effective* weight outside of
// BuildTable itself (change detection, status reporting) use the same
// rule rather than duplicating it.
func NormalizeWeight(w uint32) uint32 {
	if w == 0 {
		return 1
	}
	return w
}

// BuildTable computes the Maglev lookup table of size m for the given
// backends (their order determines round-robin fill priority, but not the
// resulting distribution). Name must be unique and stable across rebuilds
// (e.g. "addr:port") so existing flows keep their backend whenever the
// backend set and weights are unchanged.
//
// Weighting is implemented by giving backend i's Weight virtual entries
// in the permutation table instead of the usual one — each with its own
// (offset, skip) pair — so it wins roughly Weight times as many slots in
// the same fill loop the unweighted algorithm already uses. A backend's
// first virtual entry reuses today's unweighted hash input (bare Name, no
// suffix), so an all-weight-1 call produces a byte-identical table to the
// pre-weighting algorithm; only a backend's 2nd-and-later virtual entries
// (Weight >= 2) use a distinguishing suffix.
func BuildTable(m int, backends []Backend) ([]int, error) {
	n := len(backends)
	if n == 0 {
		return nil, fmt.Errorf("maglev: no backends")
	}

	type virtualEntry struct {
		offset, skip int
		backendIdx   int
	}
	virtuals := make([]virtualEntry, 0, n)
	for i, b := range backends {
		w := NormalizeWeight(b.Weight)
		for c := range w {
			key := b.Name
			if c > 0 {
				key = fmt.Sprintf("%s#%d", b.Name, c)
			}
			virtuals = append(virtuals, virtualEntry{
				offset:     int(hash(key, 0xd15ea5e) % uint64(m)),
				skip:       int(hash(key, 0x5eedbeef)%uint64(m-1)) + 1,
				backendIdx: i,
			})
		}
	}

	total := len(virtuals)
	if m <= total {
		return nil, fmt.Errorf("maglev: table size %d must exceed total backend weight %d", m, total)
	}

	table := make([]int, m)
	for i := range table {
		table[i] = -1
	}

	next := make([]int, total)
	filled := 0
	for filled < m {
		for v := 0; v < total && filled < m; v++ {
			ve := &virtuals[v]
			slot := (ve.offset + next[v]*ve.skip) % m
			for table[slot] != -1 {
				next[v]++
				slot = (ve.offset + next[v]*ve.skip) % m
			}
			table[slot] = ve.backendIdx
			next[v]++
			filled++
		}
	}
	return table, nil
}

func hash(s string, seed uint32) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte{byte(seed), byte(seed >> 8), byte(seed >> 16), byte(seed >> 24)})
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}
