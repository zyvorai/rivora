// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package dataplane

import (
	"fmt"
	"sort"
)

// extent is a (offset, size) range within the single flat maglev_table —
// one VIP's slice of the shared 65537-slot budget.
type extent struct {
	offset uint32
	size   uint32
}

// maglevSizeClasses are the extent sizes a VIP can be allocated, smallest
// class that comfortably fits its backend count. Maglev's own distribution
// quality needs the table size well above 100x the backend count (see
// internal/maglev's doc comment); fixed classes (rather than exact sizing)
// give ordinary pod-scaling headroom to stay in the same extent instead of
// triggering a reallocation on every replica-count change.
var maglevSizeClasses = []uint32{1031, 4099, 16411, 65537}

// maglevClassFor returns the smallest size class that fits backendCount
// backends with good Maglev distribution.
func maglevClassFor(backendCount int) uint32 {
	want := uint32(backendCount) * 100
	if want < 1024 {
		want = 1024
	}
	for _, c := range maglevSizeClasses {
		if c >= want {
			return c
		}
	}
	return maglevSizeClasses[len(maglevSizeClasses)-1]
}

// extentAllocator is a first-fit, coalescing allocator over [0, total) —
// the maglev_table's shared slot space. Every VIP on the node gets a
// non-overlapping extent; extents are freed back to the pool (and merged
// with adjacent free extents) when a VIP is removed or outgrows its
// current extent and moves to a new one.
type extentAllocator struct {
	total uint32
	free  []extent // sorted by offset, non-overlapping
}

func newExtentAllocator(total uint32) *extentAllocator {
	return &extentAllocator{total: total, free: []extent{{offset: 0, size: total}}}
}

func (a *extentAllocator) Alloc(size uint32) (extent, error) {
	for i, e := range a.free {
		if e.size < size {
			continue
		}
		alloc := extent{offset: e.offset, size: size}
		if remaining := e.size - size; remaining == 0 {
			a.free = append(a.free[:i], a.free[i+1:]...)
		} else {
			a.free[i] = extent{offset: e.offset + size, size: remaining}
		}
		return alloc, nil
	}
	return extent{}, fmt.Errorf("no free maglev_table extent of size %d available (table exhausted)", size)
}

func (a *extentAllocator) Free(e extent) {
	if e.size == 0 {
		return
	}
	a.free = append(a.free, e)
	sort.Slice(a.free, func(i, j int) bool { return a.free[i].offset < a.free[j].offset })

	merged := a.free[:1]
	for _, cur := range a.free[1:] {
		last := &merged[len(merged)-1]
		if last.offset+last.size == cur.offset {
			last.size += cur.size
		} else {
			merged = append(merged, cur)
		}
	}
	a.free = merged
}
