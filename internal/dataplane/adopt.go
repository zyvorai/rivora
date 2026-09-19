// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package dataplane

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sort"

	"github.com/cilium/ebpf"

	"github.com/zyvorai/rivora/internal/bpfmaps"
	"github.com/zyvorai/rivora/internal/config"
)

// Adoption exists because the BPF maps are pinned and outlive rivorad, but the
// ID allocators and per-VIP bookkeeping are in-memory and start empty. Without
// it a restart programs the new config on top of the previous run's leftovers:
//
//   - a VIP removed from the config while rivorad was down keeps its vip_map
//     entry, so it stays programmed; worse, its service ID is free as far as the
//     new process knows, so the first VIP in the new config is handed that ID
//     and the stale VIP's traffic is forwarded to the wrong VIP's backends;
//   - reordering or inserting a VIP shifts every ID after it, so for the
//     moment between one VIP's service_config being rewritten and the next
//     VIP's vip_map entry being rewritten, traffic is steered by mismatched
//     IDs.
//
// adoptLocked rebuilds the in-memory state from the maps — service IDs, Maglev
// extents and backend IDs go back into the allocators, each VIP's entry is
// recreated — so the config can then be applied through the same reload path a
// SIGHUP uses: stale VIPs are removed (properly, freeing their IDs and extents)
// and kept VIPs are updated in place under the IDs they already had.

// vipFromKey4 and vipFromKey6 invert vipMapWrite: they turn a vip_map key back
// into the address/port/protocol a config.VIP would carry. ok is false for a
// protocol rivorad never writes, which marks the entry as not ours.
func vipFromKey4(k bpfmaps.VipKey) (addr string, port uint16, proto config.Protocol, ok bool) {
	ip := make(net.IP, 4)
	binary.LittleEndian.PutUint32(ip, k.Addr) // inverse of ip4ToBE32
	proto, ok = protoFromByte(k.Proto)
	return ip.String(), htons(k.Port), proto, ok // htons is its own inverse
}

func vipFromKey6(k bpfmaps.VipKey6) (addr string, port uint16, proto config.Protocol, ok bool) {
	proto, ok = protoFromByte(k.Proto)
	return net.IP(k.Addr[:]).String(), htons(k.Port), proto, ok
}

func protoFromByte(b uint8) (config.Protocol, bool) {
	switch b {
	case 6:
		return config.ProtoTCP, true
	case 17:
		return config.ProtoUDP, true
	}
	return "", false
}

// distinctIDs returns the sorted unique values of slots — the set of backend IDs
// a VIP's Maglev extent references, which is the only place the maps record
// which backends belong to a VIP.
func distinctIDs(slots []uint32) []uint32 {
	seen := make(map[uint32]bool, 8)
	out := make([]uint32, 0, 8)
	for _, s := range slots {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// reserve marks id as already taken by key, as if Alloc had returned it. It
// refuses a conflict (key held under another ID, or id held by another key), so
// a corrupt or inconsistent map is rejected instead of silently aliasing two
// VIPs onto one ID.
func (a *idAllocator) reserve(key string, id uint32) error {
	if id >= a.max {
		return fmt.Errorf("id %d out of range (capacity %d)", id, a.max)
	}
	if cur, ok := a.ids[key]; ok {
		if cur == id {
			return nil
		}
		return fmt.Errorf("%q already holds id %d, not %d", key, cur, id)
	}
	for k, v := range a.ids {
		if v == id {
			return fmt.Errorf("id %d already held by %q, not %q", id, k, key)
		}
	}
	// Anything between next and id was never handed out but is now below the
	// high-water mark; make it available for reuse rather than lose it.
	for a.next < id {
		a.free = append(a.free, a.next)
		a.next++
	}
	if id == a.next {
		a.next++
	} else {
		for i, f := range a.free {
			if f == id {
				a.free = append(a.free[:i], a.free[i+1:]...)
				break
			}
		}
	}
	a.ids[key] = id
	return nil
}

// reserve carves e out of the free space, as if Alloc had returned it. It fails
// if any part of e is already allocated or lies outside the table, which for
// adoption means two VIPs' extents overlap (a corrupt map).
func (a *extentAllocator) reserve(e extent) error {
	if e.size == 0 || e.offset+e.size > a.total || e.offset+e.size < e.offset {
		return fmt.Errorf("extent [%d,+%d) outside the %d-slot table", e.offset, e.size, a.total)
	}
	for i, f := range a.free {
		if e.offset < f.offset || e.offset+e.size > f.offset+f.size {
			continue
		}
		var rest []extent
		if e.offset > f.offset {
			rest = append(rest, extent{offset: f.offset, size: e.offset - f.offset})
		}
		if end := e.offset + e.size; end < f.offset+f.size {
			rest = append(rest, extent{offset: end, size: f.offset + f.size - end})
		}
		a.free = append(a.free[:i], append(rest, a.free[i+1:]...)...)
		return nil
	}
	return fmt.Errorf("extent [%d,+%d) overlaps space that is already allocated", e.offset, e.size)
}

// adoptedBackend is a backend read back from backend_map/backend_map6.
type adoptedBackend struct {
	id      uint32
	address string
	port    uint16
	isV4    bool
}

// rawVIP is one vip_map / vip_map6 entry before validation.
type rawVIP struct {
	addr  string
	port  uint16
	proto config.Protocol
	sid   uint32
	is4   bool
	key4  bpfmaps.VipKey
	key6  bpfmaps.VipKey6
}

// AdoptSummary reports what start-up found already programmed.
type AdoptSummary struct {
	Adopted int `json:"adopted"` // VIPs recovered from the maps
	Dropped int `json:"dropped"` // entries that were inconsistent and were deleted
}

// adoptLocked rebuilds d's in-memory state from the pinned maps. It must run
// before anything is programmed and before the API or health checker start, on a
// Dataplane that has no services yet. An entry it can't make sense of (an empty
// or overlapping extent, a Maglev slot naming a backend that doesn't exist, an
// ID that collides with another) is deleted rather than adopted: forwarding
// through a half-understood entry is worse than the gap while the config
// re-creates it.
func (d *Dataplane) adoptLocked() (AdoptSummary, error) {
	var sum AdoptSummary
	var raws []rawVIP

	vm := d.dp.Maps[bpfmaps.MapVIP]
	var k4 bpfmaps.VipKey
	var sid uint32
	it := vm.Iterate()
	for it.Next(&k4, &sid) {
		addr, port, proto, ok := vipFromKey4(k4)
		if !ok {
			continue
		}
		raws = append(raws, rawVIP{addr: addr, port: port, proto: proto, sid: sid, is4: true, key4: k4})
	}
	// A scan that stops early would leave some pinned VIPs unadopted, and the
	// ID collisions adoption exists to prevent would come straight back; refuse
	// to proceed rather than continue with a partial picture.
	if err := it.Err(); err != nil {
		return sum, fmt.Errorf("scan vip_map: %w", err)
	}
	if vm6 := d.dp.Maps[bpfmaps.MapVIP6]; vm6 != nil {
		var k6 bpfmaps.VipKey6
		it6 := vm6.Iterate()
		for it6.Next(&k6, &sid) {
			addr, port, proto, ok := vipFromKey6(k6)
			if !ok {
				continue
			}
			raws = append(raws, rawVIP{addr: addr, port: port, proto: proto, sid: sid, key6: k6})
		}
		if err := it6.Err(); err != nil {
			return sum, fmt.Errorf("scan vip_map6: %w", err)
		}
	}
	// Deterministic order so a run with a corrupt map behaves the same twice.
	sort.Slice(raws, func(i, j int) bool { return raws[i].sid < raws[j].sid })

	for _, r := range raws {
		if err := d.adoptOneLocked(r); err != nil {
			d.dropRawVIP(r)
			sum.Dropped++
			continue
		}
		sum.Adopted++
	}
	return sum, nil
}

func (d *Dataplane) adoptOneLocked(r rawVIP) error {
	key := fmt.Sprintf("%s:%d:%s", r.addr, r.port, r.proto)

	var sc bpfmaps.ServiceConfig
	if err := d.dp.Maps[bpfmaps.MapServiceConfig].Lookup(&r.sid, &sc); err != nil {
		return fmt.Errorf("service_config[%d]: %w", r.sid, err)
	}
	ext := extent{offset: sc.MaglevOffset, size: sc.MaglevSize}
	if ext.size == 0 {
		return errors.New("empty maglev extent")
	}

	slots := make([]uint32, ext.size)
	mt := d.dp.Maps[bpfmaps.MapMaglevTable]
	for i := range slots {
		slot := ext.offset + uint32(i)
		if err := mt.Lookup(&slot, &slots[i]); err != nil {
			return fmt.Errorf("maglev_table[%d]: %w", slot, err)
		}
	}

	backends := map[string]adoptedBackend{}
	for _, id := range distinctIDs(slots) {
		b, err := d.readBackend(id)
		if err != nil {
			return err
		}
		backends[fmt.Sprintf("%s:%d", b.address, b.port)] = b
	}

	// Everything readable and self-consistent; now claim the IDs. Claims are
	// undone on conflict so a rejected VIP doesn't leave partial reservations.
	if err := d.maglevAlloc.reserve(ext); err != nil {
		return err
	}
	if err := d.serviceAlloc.reserve(key, r.sid); err != nil {
		d.maglevAlloc.Free(ext)
		return err
	}
	// A backend can be shared by several VIPs, so one already claimed by an
	// earlier adopted VIP is not ours to roll back.
	var claimed []string
	for name, b := range backends {
		_, already := d.backendAlloc.Get(name)
		if err := d.backendAlloc.reserve(name, b.id); err != nil {
			for _, c := range claimed {
				d.backendAlloc.Release(c)
			}
			d.serviceAlloc.Release(key)
			d.maglevAlloc.Free(ext)
			return err
		}
		if !already {
			claimed = append(claimed, name)
		}
	}

	entry := &serviceEntry{
		serviceID:  r.sid,
		backendIDs: make(map[string]uint32, len(backends)),
		extent:     ext,
		vip: config.VIP{
			Address: r.addr, Port: r.port, Protocol: r.proto,
			Mode: modeFromByte(sc.Mode),
		},
	}
	for name, b := range backends {
		entry.backendIDs[name] = b.id
		st := d.backendStates[b.id]
		if st == nil {
			// probeHealthy=true, like a freshly acquired backend: the health
			// checker starts every target healthy and only reports
			// transitions, so a backend left Down in the map would never be
			// flipped back up if it had since recovered.
			st = &backendState{address: b.address, port: b.port, isV4: b.isV4, probeHealthy: true}
			d.backendStates[b.id] = st
		}
		st.refCount++
	}
	d.services[key] = entry
	// Best-effort: by this point the entry is registered, so a failure here must
	// not un-adopt it. A map that can't take this write will fail the config
	// apply that follows, loudly.
	for _, b := range backends {
		_ = d.writeHealthLocked(b.id)
	}
	return nil
}

// readBackend reads backend id back from backend_map, or backend_map6 if it
// isn't an IPv4 backend.
func (d *Dataplane) readBackend(id uint32) (adoptedBackend, error) {
	var bi bpfmaps.BackendInfo
	err := d.dp.Maps[bpfmaps.MapBackend].Lookup(&id, &bi)
	if err == nil {
		ip := make(net.IP, 4)
		binary.LittleEndian.PutUint32(ip, bi.Addr)
		return adoptedBackend{id: id, address: ip.String(), port: htons(bi.Port), isV4: true}, nil
	}
	if !errors.Is(err, ebpf.ErrKeyNotExist) {
		return adoptedBackend{}, fmt.Errorf("backend_map[%d]: %w", id, err)
	}
	if m6 := d.dp.Maps[bpfmaps.MapBackend6]; m6 != nil {
		var bi6 bpfmaps.BackendInfo6
		if err6 := m6.Lookup(&id, &bi6); err6 == nil {
			return adoptedBackend{id: id, address: net.IP(bi6.Addr[:]).String(), port: htons(bi6.Port)}, nil
		}
	}
	return adoptedBackend{}, fmt.Errorf("maglev slot names backend %d but no backend_map entry exists", id)
}

// dropRawVIP deletes an entry adoption couldn't trust. Best-effort, like the
// other teardown paths.
func (d *Dataplane) dropRawVIP(r rawVIP) {
	if r.is4 {
		_ = d.dp.Maps[bpfmaps.MapVIP].Delete(&r.key4)
		return
	}
	_ = d.dp.Maps[bpfmaps.MapVIP6].Delete(&r.key6)
}

func modeFromByte(m uint8) config.Mode {
	if m == bpfmaps.ModeNAT {
		return config.ModeNAT
	}
	return config.ModeDSR
}
