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

// BuildTable computes the Maglev lookup table of size m for the given
// backend identifiers (their order determines round-robin fill priority,
// but not the resulting distribution). names must be unique and stable
// across rebuilds (e.g. "addr:port") so existing flows keep their backend
// whenever the backend set is unchanged.
func BuildTable(m int, names []string) ([]int, error) {
	n := len(names)
	if n == 0 {
		return nil, fmt.Errorf("maglev: no backends")
	}
	if m <= n {
		return nil, fmt.Errorf("maglev: table size %d must exceed backend count %d", m, n)
	}

	offset := make([]int, n)
	skip := make([]int, n)
	for i, name := range names {
		offset[i] = int(hash(name, 0xd15ea5e) % uint64(m))
		skip[i] = int(hash(name, 0x5eedbeef)%uint64(m-1)) + 1
	}

	table := make([]int, m)
	for i := range table {
		table[i] = -1
	}

	next := make([]int, n)
	filled := 0
	for filled < m {
		for i := 0; i < n && filled < m; i++ {
			slot := (offset[i] + next[i]*skip[i]) % m
			for table[slot] != -1 {
				next[i]++
				slot = (offset[i] + next[i]*skip[i]) % m
			}
			table[slot] = i
			next[i]++
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
