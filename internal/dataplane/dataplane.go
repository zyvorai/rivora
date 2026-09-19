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
	"errors"
	"fmt"
	"net"
	"reflect"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/cilium/ebpf"

	"github.com/zyvorai/rivora/internal/bpfmaps"
	"github.com/zyvorai/rivora/internal/config"
	"github.com/zyvorai/rivora/internal/loader"
	"github.com/zyvorai/rivora/internal/maglev"
)

// Errors returned by the operator-facing admin calls (SetBackendAdminDraining,
// SetBackendWeight) so the API layer can map them to 404 vs 500.
var (
	ErrBackendNotFound = errors.New("backend not found")
	ErrVIPNotFound     = errors.New("vip not found")
	ErrInvalidWeight   = errors.New("invalid weight")
)

// MaxBackendWeight bounds an operator-supplied weight. BuildTable creates one
// virtual entry per unit of weight, so an unbounded value would let a single
// API call make a Maglev rebuild arbitrarily expensive.
const MaxBackendWeight = 1000

type BackendStatus struct {
	ID      uint32 `json:"id"`
	Address string `json:"address"`
	Port    uint16 `json:"port"`
	Weight  uint32 `json:"weight"`
	// Healthy is true only for "healthy": a draining or down backend is not
	// taking new flows. State distinguishes the two ("healthy", "draining",
	// "down"), and AdminDraining is set when an operator (rivoractl drain)
	// rather than a Kubernetes EndpointSlice asked for the drain.
	Healthy       bool   `json:"healthy"`
	State         string `json:"state"`
	AdminDraining bool   `json:"adminDraining,omitempty"`
	Packets       uint64 `json:"packets"`
	Bytes         uint64 `json:"bytes"`
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
	// Dropped is node-wide (stats_map's global slot), the sum of every VIP's
	// drops, not this VIP's own count — the fields below are this VIP's own.
	Dropped uint64 `json:"dropped"`

	// This VIP's own drops by reason, plus packets that matched the VIP but
	// bypassed the load balancer (drop_stats_map, summed across CPUs). Like the
	// other counters they run from when the BPF maps were pinned.
	DroppedRateLimited uint64 `json:"droppedRateLimited"` // SYNs over the per-source rate limit
	DroppedNoBackend   uint64 `json:"droppedNoBackend"`   // no healthy backend in the probe window
	Unserved           uint64 `json:"unserved"`           // XDP_PASS: VIP matched but couldn't be served
}

// serviceEntry is a node's live state for one VIP.
type serviceEntry struct {
	serviceID uint32
	// vipSpec is the VIP exactly as the last UpsertVIP caller (config or a
	// Kubernetes reconciler) supplied it; vip is what is actually programmed,
	// i.e. vipSpec with weightOverrides applied. Keeping both lets an operator
	// override survive reconciler re-syncs and lets a reset restore the
	// caller's original weight.
	vipSpec         config.VIP
	vip             config.VIP
	weightOverrides map[string]uint32 // "addr:port" -> operator-set weight
	backendIDs      map[string]uint32 // "addr:port" -> backend_id, this VIP's current backend set
	extent          extent            // this VIP's slice of the shared maglev_table
}

// backendState is a node's live state for one backend, shared across every
// VIP that currently references it (refCount tracks how many).
type backendState struct {
	address      string
	port         uint16
	isV4         bool // which of backend_map/backend_map6 this ID lives in (v0.3)
	probeHealthy bool // from internal/healthcheck's active TCP probes
	draining     bool // from a Kubernetes reconciler's EndpointSlice terminating state (v0.2+)
	// probe is how the health checker should check this backend, taken from the
	// VIP that lists it. A backend shared by several VIPs is probed once, and
	// config validation guarantees those VIPs agree, so any of them will do.
	probe config.ProbeSpec
	// adminDraining is an operator's drain (rivoractl drain). Tracked apart
	// from draining so a reconciler clearing its own terminating state can't
	// silently undo an operator's drain, and vice versa.
	adminDraining bool
	refCount      int
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

	startup StartupSummary
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

	if err := d.applyRateLimit(); err != nil {
		return err
	}

	// Static-YAML mode: the file is the whole desired VIP set, so recover what
	// a previous run left in the pinned maps and reconcile it against the file
	// (see adopt.go) instead of programming on top of leftovers. Kubernetes mode
	// also calls Apply, but with no VIPs — its reconcilers supply them later, so
	// there is nothing to reconcile against, and adopting-then-reloading to an
	// empty set would tear down every persisted VIP. It keeps the old path.
	if len(d.cfg.VIPs) == 0 {
		return nil
	}
	d.mu.Lock()
	sum, err := d.adoptLocked()
	d.mu.Unlock()
	if err != nil {
		return fmt.Errorf("adopt existing datapath state: %w", err)
	}
	res, err := d.ReloadVIPs(d.cfg.VIPs)
	d.mu.Lock()
	d.startup = StartupSummary{AdoptSummary: sum, ReloadResult: res}
	d.mu.Unlock()
	return err
}

// StartupSummary is what Apply found already programmed and what it did about
// it: Adopted VIPs recovered from the pinned maps, Dropped entries it couldn't
// trust, and the add/update/remove/unchanged split of reconciling them against
// the config (Removed are VIPs left over from a previous config).
type StartupSummary struct {
	AdoptSummary
	ReloadResult
}

// Startup returns the summary of the last Apply. Zero in Kubernetes mode.
func (d *Dataplane) Startup() StartupSummary {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.startup
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

// applyRateLimit writes rl_config_map[0] from d.cfg.RateLimit. Runs
// unconditionally (both the static-YAML and Kubernetes-mode startup
// paths call Apply()) — a zero-value RateLimit (Enabled: false, the
// default) writes an all-zero, disabled entry, matching the "opt-in,
// zero-cost-when-off" contract rate_limit_exceeded() in bpf/xdp_ingress.c
// documents (it returns immediately on !enabled, before ever touching
// rl_buckets_map).
//
// rate_limit_exceeded() runs once per CPU independently (rl_buckets_map
// is BPF_MAP_TYPE_LRU_PERCPU_HASH, avoiding cross-CPU lock/atomic
// contention exactly where a real flood would create the most of it) —
// so the *effective* global rate is roughly configured-rate times
// however many CPUs end up processing a given source's traffic. Dividing
// by runtime.NumCPU() here is a best-effort approximation of that CPU
// count (XDP's actual RX-queue-to-CPU spread isn't introspected), so the
// value in rivorad's config means what it says despite per-CPU
// enforcement underneath.
func (d *Dataplane) applyRateLimit() error {
	rl := bpfmaps.RLConfig{}
	if d.cfg.RateLimit.Enabled {
		cpus := uint64(runtime.NumCPU())
		if cpus == 0 {
			cpus = 1
		}
		rate := d.cfg.RateLimit.PerSourcePacketsPerSecond / cpus
		burst := d.cfg.RateLimit.Burst / cpus
		if rate == 0 {
			rate = 1
		}
		if burst == 0 {
			burst = 1
		}
		rl = bpfmaps.RLConfig{RatePerSec: rate, Burst: burst, Enabled: 1}
	}
	var zero uint32
	if err := d.dp.Maps[bpfmaps.MapRateLimitConfig].Update(&zero, &rl, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("update rl_config_map: %w", err)
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
	return d.upsertVIPLocked(vip)
}

func (d *Dataplane) upsertVIPLocked(spec config.VIP) error {
	key := vipKeyString(spec)
	entry, existed := d.services[key]
	if !existed {
		id, err := d.serviceAlloc.Alloc(key)
		if err != nil {
			return fmt.Errorf("vip %s: %w", key, err)
		}
		entry = &serviceEntry{serviceID: id, backendIDs: map[string]uint32{}}
		d.services[key] = entry
		// drop_stats_map is pinned and outlives VIPs, so a freed ID may still
		// carry the previous VIP's counts; a new VIP starts from zero.
		d.resetDropStatsLocked(id)
	}

	vip := entry.applyWeightOverrides(spec)
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

	d.applyProbeLocked(entry, vip.HealthCheck)

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

	if err := d.vipMapWrite(vip, entry.serviceID); err != nil {
		return fmt.Errorf("vip %s: %w", key, err)
	}

	entry.vipSpec = spec
	entry.vip = vip
	return nil
}

// applyProbeLocked records probe as the health-check spec of every backend entry
// lists, so a probe edit in the config (or a reload) reaches the checker on its
// next Targets() call.
func (d *Dataplane) applyProbeLocked(entry *serviceEntry, probe config.ProbeSpec) {
	for _, id := range entry.backendIDs {
		if st := d.backendStates[id]; st != nil {
			st.probe = probe
		}
	}
}

// applyWeightOverrides returns spec with each operator-overridden backend's
// weight replaced. Overrides for backends spec no longer contains are dropped,
// so a backend that leaves and later rejoins starts from its configured
// weight rather than a stale override. spec's own Backends slice is never
// mutated (it belongs to the caller).
func (e *serviceEntry) applyWeightOverrides(spec config.VIP) config.VIP {
	if len(e.weightOverrides) == 0 {
		return spec
	}
	present := make(map[string]bool, len(spec.Backends))
	out := spec
	out.Backends = make([]config.Backend, len(spec.Backends))
	for i, b := range spec.Backends {
		name := backendName(b)
		present[name] = true
		if w, ok := e.weightOverrides[name]; ok {
			b.Weight = w
		}
		out.Backends[i] = b
	}
	for name := range e.weightOverrides {
		if !present[name] {
			delete(e.weightOverrides, name)
		}
	}
	return out
}

// ReloadResult summarises what ReloadVIPs changed.
type ReloadResult struct {
	Added     int `json:"added"`
	Updated   int `json:"updated"`
	Removed   int `json:"removed"`
	Unchanged int `json:"unchanged"`
}

// reloadPlan is what ReloadVIPs will do, computed without touching any map.
type reloadPlan struct {
	upserts   []config.VIP // new or changed, in desired order
	removes   []string     // vip keys no longer wanted, sorted
	added     int
	updated   int
	unchanged int
}

// planReload diffs the VIP specs currently programmed (current: key -> spec as
// last supplied by the caller, i.e. without operator weight overrides) against
// the desired set. A VIP whose spec is deep-equal is left alone entirely, so a
// reload of an unchanged file rewrites no BPF map. If desired lists the same
// key twice the later entry wins, matching how map writes would resolve it.
func planReload(current map[string]config.VIP, desired []config.VIP) reloadPlan {
	var plan reloadPlan
	want := make(map[string]config.VIP, len(desired))
	order := make([]string, 0, len(desired))
	for _, v := range desired {
		k := vipKeyString(v)
		if _, dup := want[k]; !dup {
			order = append(order, k)
		}
		want[k] = v
	}
	for _, k := range order {
		v := want[k]
		cur, exists := current[k]
		switch {
		case !exists:
			plan.added++
			plan.upserts = append(plan.upserts, v)
		case !reflect.DeepEqual(cur, v):
			plan.updated++
			plan.upserts = append(plan.upserts, v)
		default:
			plan.unchanged++
		}
	}
	for k := range current {
		if _, keep := want[k]; !keep {
			plan.removes = append(plan.removes, k)
		}
	}
	sort.Strings(plan.removes)
	return plan
}

// ReloadVIPs makes the programmed VIP set equal desired: it removes VIPs that
// are gone, then adds or updates the rest (removals first, so their backend IDs
// and Maglev extents are free for the additions). It is only meaningful for
// static-YAML mode, where the config file is the sole source of VIPs — with
// -kubernetes the reconcilers own the set and would be undone by this.
//
// It is best-effort per VIP: one VIP failing (say the Maglev table is full)
// does not stop the others, and the joined error names each failure. A VIP whose
// update fails can be left partially updated, but its recorded spec is not
// advanced, so the next reload sees it as changed and retries it. Operator
// weight overrides and drains survive, because UpsertVIP re-applies them.
func (d *Dataplane) ReloadVIPs(desired []config.VIP) (ReloadResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	current := make(map[string]config.VIP, len(d.services))
	for k, e := range d.services {
		current[k] = e.vipSpec
	}
	plan := planReload(current, desired)

	var errs []error
	res := ReloadResult{Unchanged: plan.unchanged}
	for _, k := range plan.removes {
		if err := d.removeVIPLocked(k); err != nil {
			errs = append(errs, fmt.Errorf("remove vip %s: %w", k, err))
			continue
		}
		res.Removed++
	}
	for _, v := range plan.upserts {
		if err := d.upsertVIPLocked(v); err != nil {
			errs = append(errs, err)
			continue
		}
		if _, existed := current[vipKeyString(v)]; existed {
			res.Updated++
		} else {
			res.Added++
		}
	}
	return res, errors.Join(errs...)
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
		ip := net.ParseIP(b.Address)
		st = &backendState{address: b.Address, port: b.Port, isV4: ip != nil && ip.To4() != nil, probeHealthy: true}
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

// writeBackendInfoLocked writes id's backend_map (or, for an IPv6
// backend, backend_map6) entry — address-family selection driven by b's
// own address, the same way vipMapWrite/vipMapDelete drive it from the
// VIP's address. Config.Validate() already rejects a backend whose family
// doesn't match its VIP's, so a given backend_id is written to exactly
// one of the two maps for its whole lifetime.
func (d *Dataplane) writeBackendInfoLocked(id uint32, b config.Backend) error {
	ip := net.ParseIP(b.Address)
	if ip == nil {
		return fmt.Errorf("backend %s: invalid address", b.Address)
	}
	var mac [6]byte
	if b.MAC != "" {
		hw, err := net.ParseMAC(b.MAC)
		if err != nil {
			return fmt.Errorf("backend %s: %w", b.Address, err)
		}
		copy(mac[:], hw)
	}
	if ip4 := ip.To4(); ip4 != nil {
		bi := bpfmaps.BackendInfo{Addr: ip4ToBE32(ip4), Port: htons(b.Port), Mac: mac}
		if err := d.dp.Maps[bpfmaps.MapBackend].Update(&id, &bi, ebpf.UpdateAny); err != nil {
			return fmt.Errorf("update backend_map[%d]: %w", id, err)
		}
		return nil
	}
	bi6 := bpfmaps.BackendInfo6{Addr: ip6To16(ip), Port: htons(b.Port), Mac: mac}
	if err := d.dp.Maps[bpfmaps.MapBackend6].Update(&id, &bi6, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("update backend_map6[%d]: %w", id, err)
	}
	return nil
}

// releaseBackendLocked drops one VIP's reference to a backend; the
// backend_map(6)/backend_health_map entries and its ID are only actually
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
	if st.isV4 {
		_ = d.dp.Maps[bpfmaps.MapBackend].Delete(&id)
	} else {
		_ = d.dp.Maps[bpfmaps.MapBackend6].Delete(&id)
	}
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
	case st.draining || st.adminDraining:
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
	return d.removeVIPLocked(key)
}

func (d *Dataplane) removeVIPLocked(key string) error {
	entry, ok := d.services[key]
	if !ok {
		return nil
	}

	for name, id := range entry.backendIDs {
		d.releaseBackendLocked(name, id)
	}

	_ = d.vipMapDelete(entry.vip)
	var zero bpfmaps.ServiceConfig
	_ = d.dp.Maps[bpfmaps.MapServiceConfig].Update(&entry.serviceID, &zero, ebpf.UpdateAny)
	d.resetDropStatsLocked(entry.serviceID)

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

// SetBackendAdminDraining is the operator's drain (rivoractl drain/undrain):
// backendID stops receiving *new* flows while established ones keep flowing,
// exactly like a Kubernetes-driven drain but tracked independently of it (see
// backendState.adminDraining). A failed active probe still wins over draining.
// Returns ErrBackendNotFound if no VIP currently references backendID.
func (d *Dataplane) SetBackendAdminDraining(backendID uint32, draining bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.backendStates[backendID]
	if st == nil {
		return fmt.Errorf("backend %d: %w", backendID, ErrBackendNotFound)
	}
	st.adminDraining = draining
	return d.writeHealthLocked(backendID)
}

// SetBackendWeight overrides backendID's Maglev weight and rebuilds the
// affected VIP tables, returning how many VIPs were changed. vipKey scopes the
// change to one VIP ("addr:port:proto", as VIPKey builds it); empty applies it
// to every VIP that references the backend. weight 0 clears the override so the
// configured (or reconciler-supplied) weight applies again. The override
// survives UpsertVIP re-syncs until cleared or the backend leaves the VIP.
func (d *Dataplane) SetBackendWeight(backendID uint32, vipKey string, weight uint32) (int, error) {
	if weight > MaxBackendWeight {
		return 0, fmt.Errorf("weight %d exceeds maximum %d: %w", weight, MaxBackendWeight, ErrInvalidWeight)
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	if vipKey != "" {
		if _, ok := d.services[vipKey]; !ok {
			return 0, fmt.Errorf("vip %q: %w", vipKey, ErrVIPNotFound)
		}
	}

	// Collect targets first so a mid-loop rebuild error can't leave us
	// iterating a map we're mutating.
	type target struct {
		entry *serviceEntry
		name  string
	}
	var targets []target
	for key, entry := range d.services {
		if vipKey != "" && key != vipKey {
			continue
		}
		for name, id := range entry.backendIDs {
			if id == backendID {
				targets = append(targets, target{entry, name})
			}
		}
	}
	if len(targets) == 0 {
		return 0, fmt.Errorf("backend %d: %w", backendID, ErrBackendNotFound)
	}

	for _, t := range targets {
		if weight == 0 {
			delete(t.entry.weightOverrides, t.name)
		} else {
			if t.entry.weightOverrides == nil {
				t.entry.weightOverrides = map[string]uint32{}
			}
			t.entry.weightOverrides[t.name] = weight
		}
		if err := d.upsertVIPLocked(t.entry.vipSpec); err != nil {
			return 0, err
		}
	}
	return len(targets), nil
}

// Targets returns every backend across every VIP this node owns, deduped
// by ID (a backend shared by two VIPs is probed once, not twice) — the
// health checker's target set.
func (d *Dataplane) Targets() []struct {
	ID      uint32
	Address string
	Port    uint16
	Probe   config.ProbeSpec
} {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]struct {
		ID      uint32
		Address string
		Port    uint16
		Probe   config.ProbeSpec
	}, 0, len(d.backendStates))
	for id, st := range d.backendStates {
		out = append(out, struct {
			ID      uint32
			Address string
			Port    uint16
			Probe   config.ProbeSpec
		}{ID: id, Address: st.address, Port: st.port, Probe: st.probe})
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
	dropMap := d.dp.Maps[bpfmaps.MapDropStats] // nil for an object built before per-reason drops existed

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
		if dropMap != nil {
			if ds, err := sumDropStats(dropMap, entry.serviceID); err == nil {
				st.DroppedRateLimited = ds.ByReason[bpfmaps.DropRateLimited]
				st.DroppedNoBackend = ds.ByReason[bpfmaps.DropNoBackend]
				st.Unserved = ds.ByReason[bpfmaps.Unserved]
			}
		}
		weightByName := make(map[string]uint32, len(vip.Backends))
		for _, b := range vip.Backends {
			weightByName[backendName(b)] = maglev.NormalizeWeight(b.Weight)
		}
		for name, id := range entry.backendIDs {
			var healthy uint8
			_ = healthMap.Lookup(&id, &healthy)
			bs := BackendStatus{
				ID: id, Weight: weightByName[name],
				Healthy: healthy == bpfmaps.HealthHealthy, State: healthStateName(healthy),
			}
			if bst := d.backendStates[id]; bst != nil {
				bs.Address, bs.Port = bst.address, bst.port
				bs.AdminDraining = bst.adminDraining
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

// healthStateName maps a backend_health_map value to the label the API and
// CLI show. Anything unrecognised reads as "down" — the safe interpretation,
// since the dataplane only routes new flows to HealthHealthy.
func healthStateName(v uint8) string {
	switch v {
	case bpfmaps.HealthHealthy:
		return "healthy"
	case bpfmaps.HealthDraining:
		return "draining"
	default:
		return "down"
	}
}

// sumDropStats reads service id's per-CPU drop counters and sums them.
func sumDropStats(m *ebpf.Map, id uint32) (bpfmaps.DropStats, error) {
	var perCPU []bpfmaps.DropStats
	if err := m.Lookup(&id, &perCPU); err != nil {
		return bpfmaps.DropStats{}, err
	}
	var total bpfmaps.DropStats
	for _, s := range perCPU {
		for i := range total.ByReason {
			total.ByReason[i] += s.ByReason[i]
		}
	}
	return total, nil
}

// resetDropStatsLocked zeroes service id's drop counters on every CPU. A
// PERCPU map takes one value per possible CPU on update. Best-effort, like the
// other teardown writes: a failure leaves stale counts, not a broken dataplane.
func (d *Dataplane) resetDropStatsLocked(id uint32) {
	m := d.dp.Maps[bpfmaps.MapDropStats]
	if m == nil {
		return
	}
	n, err := ebpf.PossibleCPU()
	if err != nil {
		n = runtime.NumCPU()
	}
	_ = m.Update(&id, make([]bpfmaps.DropStats, n), ebpf.UpdateAny)
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

// vipMapWrite writes vip's vip_map (or, for an IPv6 VIP, vip_map6) entry
// mapping its key to serviceID. Address-family selection is driven
// entirely by vip.Address itself — Config.Validate() already rejects a
// VIP whose backends don't share its own family, so nothing downstream
// needs to re-derive family from anywhere but the VIP's own address.
func (d *Dataplane) vipMapWrite(vip config.VIP, serviceID uint32) error {
	ip := net.ParseIP(vip.Address)
	if ip == nil {
		return fmt.Errorf("vip %s: invalid address", vip.Address)
	}
	proto := protoByte(vip.Protocol)
	if ip4 := ip.To4(); ip4 != nil {
		vk := bpfmaps.VipKey{Addr: ip4ToBE32(ip4), Port: htons(vip.Port), Proto: proto}
		if err := d.dp.Maps[bpfmaps.MapVIP].Update(&vk, serviceID, ebpf.UpdateAny); err != nil {
			return fmt.Errorf("update vip_map: %w", err)
		}
		return nil
	}
	vk := bpfmaps.VipKey6{Addr: ip6To16(ip), Port: htons(vip.Port), Proto: proto}
	if err := d.dp.Maps[bpfmaps.MapVIP6].Update(&vk, serviceID, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("update vip_map6: %w", err)
	}
	return nil
}

// vipMapDelete removes vip's vip_map/vip_map6 entry — vipMapWrite's
// delete-path counterpart. RemoveVIP's caller intentionally ignores this
// error (matching its existing best-effort teardown style for every other
// map it touches), so it isn't wrapped/logged here either.
func (d *Dataplane) vipMapDelete(vip config.VIP) error {
	ip := net.ParseIP(vip.Address)
	if ip == nil {
		return fmt.Errorf("vip %s: invalid address", vip.Address)
	}
	proto := protoByte(vip.Protocol)
	if ip4 := ip.To4(); ip4 != nil {
		vk := bpfmaps.VipKey{Addr: ip4ToBE32(ip4), Port: htons(vip.Port), Proto: proto}
		return d.dp.Maps[bpfmaps.MapVIP].Delete(&vk)
	}
	vk := bpfmaps.VipKey6{Addr: ip6To16(ip), Port: htons(vip.Port), Proto: proto}
	return d.dp.Maps[bpfmaps.MapVIP6].Delete(&vk)
}

func protoByte(p config.Protocol) uint8 {
	if p == config.ProtoUDP {
		return 17
	}
	return 6
}

// ip4ToBE32 converts an IPv4 address into the uint32 that, once cilium/ebpf
// serializes it in the host's native (little-endian) byte order for the BPF
// map, reproduces the address's raw network-order bytes — the same bytes
// iphdr->daddr holds on the wire. This is binary.LittleEndian, not
// BigEndian, precisely because the map encoding is little-endian: reading
// network-order bytes back with LittleEndian.Uint32 is what undoes it.
func ip4ToBE32(ip net.IP) uint32 { return binary.LittleEndian.Uint32(ip) }

// ip6To16 returns ip's raw 16 network-order bytes directly — unlike
// ip4ToBE32, no endian conversion is needed: the Go struct field is
// itself a [16]byte array (not a multi-byte integer cilium/ebpf would
// otherwise serialize host-endian), so the wire bytes and the map bytes
// are identical either way.
func ip6To16(ip net.IP) [16]byte {
	var out [16]byte
	copy(out[:], ip.To16())
	return out
}

// htons mirrors C's htons on a little-endian host: the byte-swapped value,
// once serialized little-endian by cilium/ebpf, reproduces the port's
// network-order bytes — matching what tcphdr/udphdr's source/dest hold.
func htons(port uint16) uint16 { return (port << 8) | (port >> 8) }

// MapUsage is one BPF table's live occupancy, for the conntrack gauges.
type MapUsage struct {
	Table    string `json:"table"`
	Entries  uint64 `json:"entries"`
	Capacity uint64 `json:"capacity"`
}

// conntrackTables are the LRU flow tables whose fill level matters: both are
// fixed-size, and once full the kernel evicts live flows — for
// connection_affinity_map that means a flow can silently be re-hashed onto a
// different backend. IPv6 siblings are reported separately.
var conntrackTables = []struct{ label, mapName string }{
	{"affinity", bpfmaps.MapConnectionAffinity},
	{"affinity6", bpfmaps.MapConnectionAffinity6},
	{"nat_reverse", bpfmaps.MapNATReverse},
	{"nat_reverse6", bpfmaps.MapNATReverse6},
}

// ConntrackUsage counts entries in each flow table. It walks the keys (one
// syscall each), so it is O(entries) — callers on a scrape path should cache
// the result rather than call it per request. It deliberately doesn't take
// d.mu: the map handles are fixed after load and the kernel serialises map
// access, so a slow walk must not stall reconciles or health updates.
// Tables missing from the loaded object (e.g. no NAT object) are skipped.
func (d *Dataplane) ConntrackUsage() []MapUsage {
	out := make([]MapUsage, 0, len(conntrackTables))
	for _, t := range conntrackTables {
		m := d.dp.Maps[t.mapName]
		if m == nil {
			continue
		}
		n, ok := countKeys(m)
		if !ok {
			continue
		}
		out = append(out, MapUsage{Table: t.label, Entries: n, Capacity: uint64(m.MaxEntries())})
	}
	return out
}

// countKeys walks m's keys with NextKeyBytes. On an LRU map under churn a key
// can vanish between calls, which makes the kernel restart the walk from the
// first key; bounding the loop at 2x capacity turns that (rare) livelock into
// a best-effort answer instead of a hung scrape. ok is false only on a real
// error, in which case the table is left out rather than reported as empty.
func countKeys(m *ebpf.Map) (n uint64, ok bool) {
	limit := uint64(m.MaxEntries()) * 2
	key, err := m.NextKeyBytes(nil) // nil key -> first key; nil result -> done
	for err == nil && key != nil && n < limit {
		n++
		key, err = m.NextKeyBytes(key)
	}
	return n, err == nil
}
