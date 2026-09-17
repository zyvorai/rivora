// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package ipam

import (
	"crypto/rand"
	"fmt"
	"net/netip"
	"slices"
	"sync"
)

// PoolSpec is the allocator's view of one AddressPool — either an
// eagerly-expanded address list (IPv4 and small IPv6 prefixes) and/or
// sparse Prefixes for large IPv6 blocks that cannot be enumerated.
type PoolSpec struct {
	Addresses  []string
	Prefixes   []netip.Prefix
	AutoAssign bool
}

// Allocator tracks which addresses, across every pool, are assigned to
// which Service. It has no Kubernetes client dependency — the caller
// (internal/ipamctrl) is responsible for calling SetPools when AddressPool
// objects change and Reserve for every already-assigned Service at
// startup, so a restart doesn't lose track of live allocations and
// double-allocate.
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
		spec := a.pools[name]
		for _, ip := range spec.Addresses {
			if _, taken := a.addrOwner[ip]; taken {
				continue
			}
			a.commit(key, ip)
			a.addrPool[ip] = name
			return ip, nil
		}
		for _, pfx := range spec.Prefixes {
			ip, ok := a.allocFromPrefix(pfx)
			if !ok {
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

// PoolNames returns every pool name currently known to the allocator.
func (a *Allocator) PoolNames() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	names := make([]string, 0, len(a.pools))
	for name := range a.pools {
		names = append(names, name)
	}
	return names
}

// Counts returns (available, assigned) for a named pool. Sparse (prefix)
// pools report available as a large sentinel minus assigned — the true
// remaining capacity of a /64 cannot fit in a useful counter.
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
	// Count assigned addresses that belong to this pool via addrPool
	// (covers sparse-prefix allocations that aren't in Addresses).
	var prefixAssigned int64
	for ip, pname := range a.addrPool {
		if pname != pool {
			continue
		}
		if slices.Contains(spec.Addresses, ip) {
			continue // already counted above
		}
		prefixAssigned++
	}
	assigned += prefixAssigned
	if len(spec.Prefixes) > 0 {
		const sparseCapacity int64 = 1 << 30
		available += sparseCapacity - prefixAssigned
		if available < 0 {
			available = 0
		}
	}
	return available, assigned
}

func (a *Allocator) commit(key, ip string) {
	a.addrOwner[ip] = key
	a.ownerAddr[key] = ip
}

func (a *Allocator) poolFor(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ""
	}
	for name, spec := range a.pools {
		if slices.Contains(spec.Addresses, ip) || slices.Contains(spec.Addresses, addr.String()) {
			return name
		}
		for _, pfx := range spec.Prefixes {
			if pfx.Contains(addr) {
				return name
			}
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

// allocFromPrefix picks a free address inside pfx at random. Returns
// false if several attempts all collide with existing allocations (or the
// prefix is too small / degenerate).
func (a *Allocator) allocFromPrefix(pfx netip.Prefix) (string, bool) {
	hostBits := pfx.Addr().BitLen() - pfx.Bits()
	network := pfx.Masked().Addr()
	if hostBits <= 0 {
		ip := network.String()
		if _, taken := a.addrOwner[ip]; !taken {
			return ip, true
		}
		return "", false
	}
	for attempt := 0; attempt < 64; attempt++ {
		addr, ok := randomInPrefix(pfx)
		if !ok {
			return "", false
		}
		// Skip the network address itself when the prefix has room.
		if addr == network {
			continue
		}
		s := addr.String()
		if _, taken := a.addrOwner[s]; taken {
			continue
		}
		return s, true
	}
	return "", false
}

func randomInPrefix(pfx netip.Prefix) (netip.Addr, bool) {
	base := pfx.Masked().Addr().AsSlice()
	out := make([]byte, len(base))
	copy(out, base)
	hostBits := pfx.Addr().BitLen() - pfx.Bits()
	hostBytes := (hostBits + 7) / 8
	rnd := make([]byte, hostBytes)
	if _, err := rand.Read(rnd); err != nil {
		return netip.Addr{}, false
	}
	// Overlay random host bits onto the masked network prefix.
	bitOffset := pfx.Bits()
	for i := 0; i < hostBits; i++ {
		byteIdx := (bitOffset + i) / 8
		bitIdx := 7 - ((bitOffset + i) % 8)
		srcByte := rnd[i/8]
		srcBit := 7 - (i % 8)
		if srcByte&(1<<srcBit) != 0 {
			out[byteIdx] |= 1 << bitIdx
		} else {
			out[byteIdx] &^= 1 << bitIdx
		}
	}
	addr, ok := netip.AddrFromSlice(out)
	return addr, ok
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
