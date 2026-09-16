// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package dataplane wires config into the loaded BPF maps: VIP/service/
// backend/maglev population, and backend health updates as the health
// checker (or, from v0.2, a Kubernetes reconciler) reports transitions.
// It's also the source of truth the local HTTP API (internal/api) reads
// status from.
//
// v0.1 supported exactly one VIP, applied once at startup. v0.2 supports N
// VIPs — either the full static-YAML set via Apply(), or incrementally via
// UpsertVIP/RemoveVIP (what a live Kubernetes reconciler calls) — sharing
// the same global backend-ID allocator (internal/dataplane/idalloc.go) and
// Maglev-table extent allocator (internal/dataplane/maglevalloc.go), since
// backend_map/backend_health_map/stats_map and maglev_table are flat maps
// with no per-service namespacing of their own.
package dataplane

import (
	"encoding/binary"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/cilium/ebpf"

	"github.com/zyvorai/rivora/internal/bpfmaps"
	"github.com/zyvorai/rivora/internal/config"
	"github.com/zyvorai/rivora/internal/loader"
	"github.com/zyvorai/rivora/internal/maglev"
)

type BackendStatus struct {
	ID      uint32 `json:"id"`
	Address string `json:"address"`
	Port    uint16 `json:"port"`
	Weight  uint32 `json:"weight"`
	Healthy bool   `json:"healthy"`
	Packets uint64 `json:"packets"`
	Bytes   uint64 `json:"bytes"`
}

type Status struct {
	VIPAddress string          `json:"vipAddress"`
	VIPPort    uint16          `json:"vipPort"`
	Protocol   string          `json:"protocol"`
	Mode       string          `json:"mode"`
	Interface  string          `json:"interface"`
	StartedAt  time.Time       `json:"startedAt"`
	Backends   []BackendStatus `json:"backends"`
	Packets    uint64          `json:"packets"`
	Bytes      uint64          `json:"bytes"`
	// Dropped is node-wide (stats_map's global slot isn't attributable to a
	// specific VIP — some drops, like "no healthy backend", happen before
	// backend/VIP-specific accounting), not this VIP's own drop count.
	Dropped uint64 `json:"dropped"`
}

// serviceEntry is a node's live state for one VIP.
type serviceEntry struct {
	serviceID  uint32
	vip        config.VIP
	backendIDs map[string]uint32 // "addr:port" -> backend_id, this VIP's current backend set
	extent     extent            // this VIP's slice of the shared maglev_table
}

// backendState is a node's live state for one backend, shared across every
// VIP that currently references it (refCount tracks how many).
type backendState struct {
	address      string
	port         uint16
	probeHealthy bool // from internal/healthcheck's active TCP probes
	draining     bool // from a Kubernetes reconciler's EndpointSlice terminating state (v0.2+)
	refCount     int
}

type Dataplane struct {
	cfg       config.Config
	dp        *loader.Datapath
	startedAt time.Time
	iface     *net.Interface

	mu            sync.Mutex
	services      map[string]*serviceEntry // vipKeyString -> entry
	backendStates map[uint32]*backendState
	serviceAlloc  *idAllocator
	backendAlloc  *idAllocator
	maglevAlloc   *extentAllocator
}

func New(cfg config.Config, dp *loader.Datapath) *Dataplane {
	return &Dataplane{
		cfg:           cfg,
		dp:            dp,
		startedAt:     time.Now(),
		services:      map[string]*serviceEntry{},
		backendStates: map[uint32]*backendState{},
		serviceAlloc:  newIDAllocator(bpfmaps.MaxVIPs),
		backendAlloc:  newIDAllocator(bpfmaps.MaxBackends),
		maglevAlloc:   newExtentAllocator(bpfmaps.MaglevM),
	}
}

// Apply writes the full static-YAML VIP set into the BPF maps, plus (for
// DSR) the attached interface's own MAC. Internally this is just UpsertVIP
// called once per configured VIP — the same path a Kubernetes reconciler
// uses one VIP at a time.
func (d *Dataplane) Apply(iface *net.Interface) error {
	d.mu.Lock()
	d.iface = iface
	d.mu.Unlock()

	if iface != nil && len(iface.HardwareAddr) == 6 {
		if err := d.writeIfaceMAC(iface); err != nil {
			return err
		}
	}

	for _, vip := range d.cfg.VIPs {
		if err := d.UpsertVIP(vip); err != nil {
			return fmt.Errorf("vip %s:%d: %w", vip.Address, vip.Port, err)
		}
	}
	return nil
}

func (d *Dataplane) writeIfaceMAC(iface *net.Interface) error {
	var mac [6]byte
	copy(mac[:], iface.HardwareAddr)
	ifidx := uint32(iface.Index)
	if err := d.dp.Maps[bpfmaps.MapIfaceMAC].Update(&ifidx, &mac, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("update iface_mac_map: %w", err)
	}
	return nil
}

// UpsertVIP creates or updates one VIP's state: service_config_map,
// backend_map/backend_health_map for its backends, and its slice of
// maglev_table. Safe to call repeatedly — only touches the maglev table
// when the backend *set* actually changed (membership, not just health),
// and only reallocates its maglev extent when the backend count outgrows
// its current size class; an in-place rebuild otherwise.
func (d *Dataplane) UpsertVIP(vip config.VIP) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	key := vipKeyString(vip)
	entry, existed := d.services[key]
	if !existed {
		id, err := d.serviceAlloc.Alloc(key)
		if err != nil {
			return fmt.Errorf("vip %s: %w", key, err)
		}
		entry = &serviceEntry{serviceID: id, backendIDs: map[string]uint32{}}
		d.services[key] = entry
	}

	desired := make(map[string]config.Backend, len(vip.Backends))
	for _, b := range vip.Backends {
		desired[backendName(b)] = b
	}

	backendSetChanged := false
	for name, id := range entry.backendIDs {
		if _, keep := desired[name]; keep {
			continue
		}
		d.releaseBackendLocked(name, id)
		delete(entry.backendIDs, name)
		backendSetChanged = true
	}
	for name, b := range desired {
		if id, already := entry.backendIDs[name]; already {
			if err := d.writeBackendInfoLocked(id, b); err != nil {
				return err
			}
			continue
		}
		id, err := d.acquireBackendLocked(name, b)
		if err != nil {
			return fmt.Errorf("vip %s: %w", key, err)
		}
		entry.backendIDs[name] = id
		backendSetChanged = true
	}

	oldWeights := make(map[string]uint32, len(entry.vip.Backends))
	for _, b := range entry.vip.Backends {
		oldWeights[backendName(b)] = maglev.NormalizeWeight(b.Weight)
	}
	weightsChanged := false
	for name, b := range desired {
		if oldWeights[name] != maglev.NormalizeWeight(b.Weight) {
			weightsChanged = true
			break
		}
	}

	if backendSetChanged || weightsChanged || !existed {
		if err := d.rebuildMaglevLocked(entry, desired); err != nil {
			return fmt.Errorf("vip %s: %w", key, err)
		}
	}

	mode := uint8(bpfmaps.ModeDSR)
	if vip.Mode == config.ModeNAT {
		mode = bpfmaps.ModeNAT
	}
	sc := bpfmaps.ServiceConfig{
		BackendCount: uint32(len(entry.backendIDs)),
		MaglevOffset: entry.extent.offset,
		MaglevSize:   entry.extent.size,
		Mode:         mode,
	}
	if err := d.dp.Maps[bpfmaps.MapServiceConfig].Update(&entry.serviceID, &sc, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("update service_config_map: %w", err)
	}

	vk, err := vipKeyBPF(vip)
	if err != nil {
		return fmt.Errorf("vip %s: %w", key, err)
	}
	if err := d.dp.Maps[bpfmaps.MapVIP].Update(&vk, entry.serviceID, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("update vip_map: %w", err)
	}

	entry.vip = vip
	return nil
}

// rebuildMaglevLocked rebuilds entry's slice of maglev_table for its
// current backend set and weights, reusing its existing extent if the
// backend count still fits that extent's size class, otherwise allocating
// a new extent, writing the new table into it, and only then (the atomic
// cutover) and only after that succeeds freeing the old one — so a lookup
// mid-rebuild either sees the fully-old or fully-new (offset, table)
// pair, never a mix. desired is this call's freshly-computed "addr:port"
// -> config.Backend map (built by UpsertVIP) — weight is read from it
// rather than from any shared/global state, since the same backend can
// carry a different weight in a different VIP's table.
func (d *Dataplane) rebuildMaglevLocked(entry *serviceEntry, desired map[string]config.Backend) error {
	names := make([]string, 0, len(entry.backendIDs))
	for name := range entry.backendIDs {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic BuildTable input across reconciles

	backends := make([]maglev.Backend, len(names))
	for i, name := range names {
		backends[i] = maglev.Backend{Name: name, Weight: desired[name].Weight}
	}

	size := maglevClassFor(len(names))
	needNewExtent := entry.extent.size == 0 || size > entry.extent.size
	target := entry.extent
	if needNewExtent {
		ext, err := d.maglevAlloc.Alloc(size)
		if err != nil {
			return err
		}
		target = ext
	}

	table, err := maglev.BuildTable(int(target.size), backends)
	if err != nil {
		if needNewExtent {
			d.maglevAlloc.Free(target)
		}
		return fmt.Errorf("build maglev table: %w", err)
	}

	maglevMap := d.dp.Maps[bpfmaps.MapMaglevTable]
	for slot, nameIdx := range table {
		globalSlot := target.offset + uint32(slot)
		bid := entry.backendIDs[names[nameIdx]]
		if err := maglevMap.Update(&globalSlot, &bid, ebpf.UpdateAny); err != nil {
			if needNewExtent {
				d.maglevAlloc.Free(target)
			}
			return fmt.Errorf("update maglev_table[%d]: %w", globalSlot, err)
		}
	}

	old := entry.extent
	entry.extent = target
	if needNewExtent && old.size > 0 {
		d.maglevAlloc.Free(old)
	}
	return nil
}

// acquireBackendLocked assigns (or reuses, if another VIP already
// references this addr:port) a backend_id, initializing its shared health
// state on first sight.
func (d *Dataplane) acquireBackendLocked(name string, b config.Backend) (uint32, error) {
	id, err := d.backendAlloc.Alloc(name)
	if err != nil {
		return 0, err
	}
	st, exists := d.backendStates[id]
	if !exists {
		st = &backendState{address: b.Address, port: b.Port, probeHealthy: true}
		d.backendStates[id] = st
	}
	st.refCount++

	if err := d.writeBackendInfoLocked(id, b); err != nil {
		return 0, err
	}
	if err := d.writeHealthLocked(id); err != nil {
		return 0, err
	}
	return id, nil
}

func (d *Dataplane) writeBackendInfoLocked(id uint32, b config.Backend) error {
	beIP := net.ParseIP(b.Address).To4()
	if beIP == nil {
		return fmt.Errorf("backend %s is not IPv4", b.Address)
	}
	var mac [6]byte
	if b.MAC != "" {
		hw, err := net.ParseMAC(b.MAC)
		if err != nil {
			return fmt.Errorf("backend %s: %w", b.Address, err)
		}
		copy(mac[:], hw)
	}
	bi := bpfmaps.BackendInfo{Addr: ip4ToBE32(beIP), Port: htons(b.Port), Mac: mac}
	if err := d.dp.Maps[bpfmaps.MapBackend].Update(&id, &bi, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("update backend_map[%d]: %w", id, err)
	}
	return nil
}

// releaseBackendLocked drops one VIP's reference to a backend; the
// backend_map/backend_health_map entries and its ID are only actually
// freed once no VIP references it anymore (refCount reaches 0) — e.g. a
// backend shared by two VIPs, or one still draining in another VIP.
func (d *Dataplane) releaseBackendLocked(name string, id uint32) {
	st := d.backendStates[id]
	if st == nil {
		return
	}
	st.refCount--
	if st.refCount > 0 {
		return
	}
	delete(d.backendStates, id)
	_ = d.dp.Maps[bpfmaps.MapBackend].Delete(&id)
	down := uint8(bpfmaps.HealthDown)
	_ = d.dp.Maps[bpfmaps.MapBackendHealth].Update(&id, &down, ebpf.UpdateAny) // ARRAY map: zero, can't delete
	d.backendAlloc.Release(name)
}

func (d *Dataplane) writeHealthLocked(id uint32) error {
	st := d.backendStates[id]
	if st == nil {
		return nil
	}
	v := uint8(bpfmaps.HealthHealthy)
	switch {
	case !st.probeHealthy:
		v = bpfmaps.HealthDown
	case st.draining:
		v = bpfmaps.HealthDraining
	}
	return d.dp.Maps[bpfmaps.MapBackendHealth].Update(&id, &v, ebpf.UpdateAny)
}

// RemoveVIP tears down a VIP entirely: vip_map entry, service_config_map
// slot (zeroed — it's an ARRAY map, entries can't be deleted), its maglev
// extent freed back to the allocator, and each of its backends released
// (see releaseBackendLocked — only actually removed once unreferenced by
// every VIP). key is the same "addr:port:proto" form UpsertVIP derives
// from a config.VIP internally.
func (d *Dataplane) RemoveVIP(key string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	entry, ok := d.services[key]
	if !ok {
		return nil
	}

	for name, id := range entry.backendIDs {
		d.releaseBackendLocked(name, id)
	}

	if vk, err := vipKeyBPF(entry.vip); err == nil {
		_ = d.dp.Maps[bpfmaps.MapVIP].Delete(&vk)
	}
	var zero bpfmaps.ServiceConfig
	_ = d.dp.Maps[bpfmaps.MapServiceConfig].Update(&entry.serviceID, &zero, ebpf.UpdateAny)

	if entry.extent.size > 0 {
		d.maglevAlloc.Free(entry.extent)
	}
	d.serviceAlloc.Release(key)
	delete(d.services, key)
	return nil
}

// SetBackendHealth is internal/healthcheck's OnChange callback: records the
// active-probe result for backendID and recomputes its 3-state
// backend_health_map value. A failed probe always wins over "draining" —
// never route to something that isn't actually answering.
func (d *Dataplane) SetBackendHealth(backendID uint32, healthy bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.backendStates[backendID]
	if st == nil {
		return nil // stale callback for an already-removed backend
	}
	st.probeHealthy = healthy
	return d.writeHealthLocked(backendID)
}

// SetBackendDraining is a Kubernetes reconciler's hook (v0.2+) for
// EndpointSlice terminating state: excludes backendID from *new* Maglev
// selection while leaving its already-established connections flowing.
func (d *Dataplane) SetBackendDraining(backendID uint32, draining bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.backendStates[backendID]
	if st == nil {
		return nil
	}
	st.draining = draining
	return d.writeHealthLocked(backendID)
}

// SetBackendDrainingByKey is internal/controller's EndpointSlice-driven
// hook: it resolves backendKey (BackendKey(b)) within vipKey's current
// backend set to a backend_id and applies the same draining transition as
// SetBackendDraining. A miss (VIP or backend not currently owned by this
// node — an event arriving after the corresponding RemoveVIP/backend
// removal already processed) is a silent no-op, not an error: reconcile
// order between Service and EndpointSlice events isn't guaranteed.
func (d *Dataplane) SetBackendDrainingByKey(vipKey, backendKey string, draining bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	entry, ok := d.services[vipKey]
	if !ok {
		return nil
	}
	id, ok := entry.backendIDs[backendKey]
	if !ok {
		return nil
	}
	st := d.backendStates[id]
	if st == nil {
		return nil
	}
	st.draining = draining
	return d.writeHealthLocked(id)
}

// Targets returns every backend across every VIP this node owns, deduped
// by ID (a backend shared by two VIPs is probed once, not twice) — the
// health checker's target set.
func (d *Dataplane) Targets() []struct {
	ID      uint32
	Address string
	Port    uint16
} {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]struct {
		ID      uint32
		Address string
		Port    uint16
	}, 0, len(d.backendStates))
	for id, st := range d.backendStates {
		out = append(out, struct {
			ID      uint32
			Address string
			Port    uint16
		}{ID: id, Address: st.address, Port: st.port})
	}
	return out
}

// Statuses returns one Status per VIP this node owns, sorted by
// address:port for stable output.
func (d *Dataplane) Statuses() ([]Status, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	healthMap := d.dp.Maps[bpfmaps.MapBackendHealth]
	statsMap := d.dp.Maps[bpfmaps.MapStats]

	var globalDropped uint64
	if s, err := sumStats(statsMap, bpfmaps.StatsGlobalIdx); err == nil {
		globalDropped = s.Dropped
	}

	out := make([]Status, 0, len(d.services))
	for _, entry := range d.services {
		vip := entry.vip
		st := Status{
			VIPAddress: vip.Address,
			VIPPort:    vip.Port,
			Protocol:   string(vip.Protocol),
			Mode:       string(vip.Mode),
			Interface:  d.cfg.Interface,
			StartedAt:  d.startedAt,
			Dropped:    globalDropped,
		}
		weightByName := make(map[string]uint32, len(vip.Backends))
		for _, b := range vip.Backends {
			weightByName[backendName(b)] = maglev.NormalizeWeight(b.Weight)
		}
		for name, id := range entry.backendIDs {
			var healthy uint8
			_ = healthMap.Lookup(&id, &healthy)
			bs := BackendStatus{ID: id, Weight: weightByName[name], Healthy: healthy == bpfmaps.HealthHealthy}
			if bst := d.backendStates[id]; bst != nil {
				bs.Address, bs.Port = bst.address, bst.port
			}
			if s, err := sumStats(statsMap, 1+id); err == nil {
				bs.Packets, bs.Bytes = s.Packets, s.Bytes
			}
			st.Packets += bs.Packets
			st.Bytes += bs.Bytes
			st.Backends = append(st.Backends, bs)
		}
		sort.Slice(st.Backends, func(i, j int) bool { return st.Backends[i].ID < st.Backends[j].ID })
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].VIPAddress != out[j].VIPAddress {
			return out[i].VIPAddress < out[j].VIPAddress
		}
		return out[i].VIPPort < out[j].VIPPort
	})
	return out, nil
}

// Status returns the single VIP's status for the common single-VIP case.
// With zero or multiple VIPs configured, callers (the /api/v1/status
// endpoint, `rivoractl status`) should use Statuses()/`rivoractl vips`
// instead — returning an error here rather than silently picking one VIP
// or a misleading aggregate.
func (d *Dataplane) Status() (Status, error) {
	all, err := d.Statuses()
	if err != nil {
		return Status{}, err
	}
	switch len(all) {
	case 0:
		return Status{}, fmt.Errorf("no VIPs configured")
	case 1:
		return all[0], nil
	default:
		return Status{}, fmt.Errorf("%d VIPs configured; use the vips list (rivoractl vips / /api/v1/vips) instead of status", len(all))
	}
}

func sumStats(m *ebpf.Map, idx uint32) (bpfmaps.LBStats, error) {
	var perCPU []bpfmaps.LBStats
	if err := m.Lookup(&idx, &perCPU); err != nil {
		return bpfmaps.LBStats{}, err
	}
	var total bpfmaps.LBStats
	for _, s := range perCPU {
		total.Packets += s.Packets
		total.Bytes += s.Bytes
		total.Dropped += s.Dropped
	}
	return total, nil
}

func vipKeyString(vip config.VIP) string {
	return fmt.Sprintf("%s:%d:%s", vip.Address, vip.Port, vip.Protocol)
}

func backendName(b config.Backend) string {
	return fmt.Sprintf("%s:%d", b.Address, b.Port)
}

// VIPKey and BackendKey are vipKeyString/backendName exported for callers
// outside this package (internal/controller's reconciler) that need to
// derive the same key RemoveVIP/SetBackendDrainingByKey expect, without
// duplicating the "addr:port[:proto]" format here and there.
func VIPKey(vip config.VIP) string       { return vipKeyString(vip) }
func BackendKey(b config.Backend) string { return backendName(b) }

func vipKeyBPF(vip config.VIP) (bpfmaps.VipKey, error) {
	vipIP := net.ParseIP(vip.Address).To4()
	if vipIP == nil {
		return bpfmaps.VipKey{}, fmt.Errorf("vip %s is not IPv4 — v0.1/v0.2 are IPv4-only", vip.Address)
	}
	proto := uint8(6)
	if vip.Protocol == config.ProtoUDP {
		proto = 17
	}
	return bpfmaps.VipKey{Addr: ip4ToBE32(vipIP), Port: htons(vip.Port), Proto: proto}, nil
}

// ip4ToBE32 converts an IPv4 address into the uint32 that, once cilium/ebpf
// serializes it in the host's native (little-endian) byte order for the BPF
// map, reproduces the address's raw network-order bytes — the same bytes
// iphdr->daddr holds on the wire. This is binary.LittleEndian, not
// BigEndian, precisely because the map encoding is little-endian: reading
// network-order bytes back with LittleEndian.Uint32 is what undoes it.
func ip4ToBE32(ip net.IP) uint32 { return binary.LittleEndian.Uint32(ip) }

// htons mirrors C's htons on a little-endian host: the byte-swapped value,
// once serialized little-endian by cilium/ebpf, reproduces the port's
// network-order bytes — matching what tcphdr/udphdr's source/dest hold.
func htons(port uint16) uint16 { return (port << 8) | (port >> 8) }
