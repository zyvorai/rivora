// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package ipam

import (
	"fmt"
	"slices"
	"sync"
)

// PoolSpec is the allocator's view of one AddressPool — already expanded
// to a concrete address list (see ExpandPool).
type PoolSpec struct {
	Addresses  []string
	AutoAssign bool
}

// Allocator tracks which addresses, across every pool, are assigned to
// which Service. It has no Kubernetes client dependency — the caller
// (internal/controller's IPAM reconciler) is responsible for calling
// SetPools when AddressPool objects change and Reserve for every already-
// assigned Service at startup, so a restart doesn't lose track of live
// allocations and double-allocate.
type Allocator struct {
	mu        sync.Mutex
	pools     map[string]PoolSpec // pool name -> spec
	addrOwner map[string]string   // ip -> "namespace/name"
	ownerAddr map[string]string   // "namespace/name" -> ip
	addrPool  map[string]string   // ip -> pool name (for reverse lookup / counts)
}

func NewAllocator() *Allocator {
	return &Allocator{
		pools:     map[string]PoolSpec{},
		addrOwner: map[string]string{},
		ownerAddr: map[string]string{},
		addrPool:  map[string]string{},
	}
}

// SetPools replaces the full desired pool set (called whenever AddressPool
// objects are added/updated/deleted). Existing allocations from a pool
// that's removed or shrunk are left alone — they just stop being
// re-allocatable — the reconciler surfaces that as a condition rather than
// silently evicting a live VIP.
func (a *Allocator) SetPools(pools map[string]PoolSpec) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pools = pools
}

// Reserve records that ip is already assigned to key, without going
// through the normal allocation path — used to rebuild state from
// existing Service.Status assignments on startup. Returns an error if ip
// is already reserved by a *different* key.
func (a *Allocator) Reserve(ip, key string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if owner, ok := a.addrOwner[ip]; ok && owner != key {
		return fmt.Errorf("address %s already reserved by %s", ip, owner)
	}
	a.addrOwner[ip] = key
	a.ownerAddr[key] = ip
	if pool := a.poolFor(ip); pool != "" {
		a.addrPool[ip] = pool
	}
	return nil
}

// Allocate assigns an address to key, preferring pinnedIP if set (must
// fall within a known pool and be free), else the named pool, else the
// first auto-assign pool with room. Idempotent: if key already has an
// address, that address is returned unchanged.
func (a *Allocator) Allocate(key, preferredPool, pinnedIP string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if existing, ok := a.ownerAddr[key]; ok {
		return existing, nil
	}

	if pinnedIP != "" {
		if owner, taken := a.addrOwner[pinnedIP]; taken && owner != key {
			return "", fmt.Errorf("requested address %s is already assigned to %s", pinnedIP, owner)
		}
		if a.poolFor(pinnedIP) == "" {
			return "", fmt.Errorf("requested address %s is not in any configured pool", pinnedIP)
		}
		a.commit(key, pinnedIP)
		return pinnedIP, nil
	}

	pools := a.candidatePools(preferredPool)
	if len(pools) == 0 {
		if preferredPool != "" {
			return "", fmt.Errorf("pool %q not found", preferredPool)
		}
		return "", fmt.Errorf("no auto-assign address pool with room")
	}

	for _, name := range pools {
		for _, ip := range a.pools[name].Addresses {
			if _, taken := a.addrOwner[ip]; taken {
				continue
			}
			a.commit(key, ip)
			a.addrPool[ip] = name
			return ip, nil
		}
	}
	return "", fmt.Errorf("no free address in pool %q", firstNonEmpty(preferredPool, "auto-assign"))
}

// Release frees whatever address key holds. A no-op if key has none.
func (a *Allocator) Release(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ip, ok := a.ownerAddr[key]
	if !ok {
		return
	}
	delete(a.ownerAddr, key)
	delete(a.addrOwner, ip)
	delete(a.addrPool, ip)
}

// Counts returns (available, assigned) for a named pool.
func (a *Allocator) Counts(pool string) (available, assigned int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	spec, ok := a.pools[pool]
	if !ok {
		return 0, 0
	}
	for _, ip := range spec.Addresses {
		if _, taken := a.addrOwner[ip]; taken {
			assigned++
		} else {
			available++
		}
	}
	return available, assigned
}

func (a *Allocator) commit(key, ip string) {
	a.addrOwner[ip] = key
	a.ownerAddr[key] = ip
}

func (a *Allocator) poolFor(ip string) string {
	for name, spec := range a.pools {
		if slices.Contains(spec.Addresses, ip) {
			return name
		}
	}
	return ""
}

func (a *Allocator) candidatePools(preferred string) []string {
	if preferred != "" {
		if _, ok := a.pools[preferred]; ok {
			return []string{preferred}
		}
		return nil
	}
	var out []string
	for name, spec := range a.pools {
		if spec.AutoAssign {
			out = append(out, name)
		}
	}
	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
