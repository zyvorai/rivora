// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package dataplane

import "fmt"

// idAllocator assigns stable, small integer IDs to string keys — used for
// both service IDs (keyed by "addr:port:proto") and backend IDs (keyed by
// "addr:port") across every VIP a node owns, since backend_map/
// backend_health_map/stats_map are flat maps with no per-service
// namespacing. Freed IDs are reused before growing, so a long-running
// rivorad doesn't exhaust the map capacity through churn alone.
type idAllocator struct {
	max  uint32
	ids  map[string]uint32
	free []uint32
	next uint32
}

func newIDAllocator(max uint32) *idAllocator {
	return &idAllocator{max: max, ids: map[string]uint32{}}
}

// Alloc returns key's ID, assigning a new one if key hasn't been seen (or
// has been Released) since the allocator was created.
func (a *idAllocator) Alloc(key string) (uint32, error) {
	if id, ok := a.ids[key]; ok {
		return id, nil
	}
	var id uint32
	if n := len(a.free); n > 0 {
		id = a.free[n-1]
		a.free = a.free[:n-1]
	} else {
		if a.next >= a.max {
			return 0, fmt.Errorf("no IDs left (capacity %d exhausted)", a.max)
		}
		id = a.next
		a.next++
	}
	a.ids[key] = id
	return id, nil
}

// Get returns key's current ID without allocating one.
func (a *idAllocator) Get(key string) (uint32, bool) {
	id, ok := a.ids[key]
	return id, ok
}

// Release returns key's ID to the free list for reuse. A no-op if key was
// never allocated.
func (a *idAllocator) Release(key string) {
	id, ok := a.ids[key]
	if !ok {
		return
	}
	delete(a.ids, key)
	a.free = append(a.free, id)
}
