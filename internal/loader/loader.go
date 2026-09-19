// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package loader loads and attaches Rivora's compiled BPF objects, pinning
// maps under /sys/fs/bpf/rivora-lb so a rivorad restart reuses live state
// instead of dropping connection affinity and stats. Modeled on the
// load/pin pattern in netra's internal/agent (LoadCollectionSpec +
// MapReplacements from pinned maps + link.AttachXDP/AttachTCX).
package loader

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"

	"github.com/zyvorai/rivora/internal/config"
)

const PinDir = "/sys/fs/bpf/rivora-lb"

// linkPinSubdir holds the pinned XDP/TCX links of a persisted datapath, under
// PinDir. Links live apart from the maps so listing it yields exactly the
// attachments a previous run left behind.
const linkPinSubdir = "links"

// Datapath holds the loaded collection and live attachments for the ingress
// XDP program and, when NAT mode is in use, the TCX egress un-NAT program.
type Datapath struct {
	ingressColl *ebpf.Collection
	natColl     *ebpf.Collection
	links       []link.Link

	// Persist, set before AttachXDP/AttachTCXEgress, pins each link under
	// PinDir/links so it outlives this process: the kernel keeps running the
	// attached program (against the pinned maps) while rivorad is down, and the
	// next start swaps in its own program with link.Update instead of
	// detaching and re-attaching. Off by default because a persisted datapath
	// keeps forwarding after rivorad exits until DetachPersisted is called.
	Persist bool

	// XDPMode is the attach mode to request (set before AttachXDP; empty means
	// generic). XDPActive is the mode actually in use afterwards, which differs
	// from the request only for auto, when native wasn't available: then
	// XDPFallback holds why, for the caller to log.
	XDPMode     config.XDPMode
	XDPActive   config.XDPMode
	XDPFallback error

	// Swapped names the links (e.g. "xdp-eth0") whose already-attached program
	// was replaced in place by this run rather than attached fresh — the
	// gap-free path. Filled by the Attach methods; for logging.
	Swapped []string

	pinned []string // names of links this run pinned or adopted

	// ownedMaps are maps LoadMapsOnly created (nothing else closes them).
	ownedMaps []*ebpf.Map

	Maps map[string]*ebpf.Map
}

// Load compiles-in objects from ingressObj (xdp_ingress.o) and, if natObj is
// non-empty, tc_nat.o, reusing any maps already pinned under PinDir.
func Load(ingressObj, natObj string) (*Datapath, error) {
	if err := os.MkdirAll(PinDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir pin dir: %w", err)
	}

	dp := &Datapath{Maps: map[string]*ebpf.Map{}}

	ingressColl, err := loadPinned(ingressObj, dp.Maps)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", ingressObj, err)
	}
	dp.ingressColl = ingressColl

	if natObj != "" {
		natColl, err := loadPinned(natObj, dp.Maps)
		if err != nil {
			ingressColl.Close()
			return nil, fmt.Errorf("load %s: %w", natObj, err)
		}
		dp.natColl = natColl
	}

	return dp, nil
}

// LoadMapsOnly creates fresh, unpinned copies of every map the given objects declare, and loads no
// program. Nothing under PinDir is read or written, so it is safe next to a running rivorad. It is
// for tests that exercise the control plane against real kernel maps; the maps disappear on Close.
func LoadMapsOnly(objs ...string) (*Datapath, error) {
	dp := &Datapath{Maps: map[string]*ebpf.Map{}}
	for _, obj := range objs {
		spec, err := ebpf.LoadCollectionSpec(obj)
		if err != nil {
			dp.Close()
			return nil, fmt.Errorf("load %s: %w", obj, err)
		}
		for name, ms := range spec.Maps {
			if _, dup := dp.Maps[name]; dup {
				continue // a map both objects declare (nat_reverse_map) is one map
			}
			ms.Pinning = ebpf.PinNone
			m, err := ebpf.NewMap(ms)
			if err != nil {
				dp.Close()
				return nil, fmt.Errorf("create map %s: %w", name, err)
			}
			dp.Maps[name] = m
			dp.ownedMaps = append(dp.ownedMaps, m)
		}
	}
	return dp, nil
}

func loadPinned(objPath string, sharedMaps map[string]*ebpf.Map) (*ebpf.Collection, error) {
	spec, err := ebpf.LoadCollectionSpec(objPath)
	if err != nil {
		return nil, err
	}

	// Only pass replacements for maps this specific object actually
	// declares — cilium/ebpf errors ("replacement map X not found in
	// CollectionSpec") if handed a map the spec doesn't reference, which
	// xdp_ingress.o and tc_nat.o mostly don't share (only nat_reverse_map is
	// common between them).
	repl := map[string]*ebpf.Map{}
	for name := range spec.Maps {
		if m, ok := sharedMaps[name]; ok {
			repl[name] = m
			continue
		}
		if m, err := ebpf.LoadPinnedMap(filepath.Join(PinDir, name), nil); err == nil {
			repl[name] = m
		}
	}

	coll, err := ebpf.NewCollectionWithOptions(spec, ebpf.CollectionOptions{
		MapReplacements: repl,
	})
	if err != nil {
		return nil, err
	}

	for name, m := range coll.Maps {
		if _, exists := sharedMaps[name]; !exists {
			pinPath := filepath.Join(PinDir, name)
			if _, err := os.Stat(pinPath); os.IsNotExist(err) {
				if err := m.Pin(pinPath); err != nil {
					return nil, fmt.Errorf("pin map %s: %w", name, err)
				}
			}
			sharedMaps[name] = m
		}
	}

	return coll, nil
}

// AttachXDP attaches the ingress program to iface in generic (SKB) mode.
//
// veth's "native" XDP mode accepts the attach cleanly but its XDP_TX
// hairpin-to-peer delivery does not reliably cross a bridge the peer is
// attached to (verified experimentally: the rewritten frame never reaches
// the peer even though driver TX counters report zero drops) — a real,
// reproducible gap in this environment, not a program-logic bug. Generic
// mode runs XDP_TX through the normal dev_queue_xmit() path instead of
// veth's special-cased hairpin, which does deliver correctly across a
// bridge. TODO(v0.2): make this configurable and default to native mode on
// interfaces with real driver support (physical NICs, SR-IOV, etc.), where
// XDP_TX re-queues on the same wire and this gap doesn't apply.
func (d *Datapath) AttachXDP(iface *net.Interface) error {
	prog, ok := d.ingressColl.Programs["rivora_xdp_ingress"]
	if !ok {
		return fmt.Errorf("program rivora_xdp_ingress not found in object")
	}
	req := d.XDPMode.Effective()
	present := map[config.XDPMode]bool{}
	names, _ := PersistedLinks()
	for _, m := range xdpModes {
		for _, n := range names {
			if n == xdpPinName(m, iface.Name) {
				present[m] = true
			}
		}
	}
	plan := planXDP(d.Persist, req, present)

	// Adopt a link a previous run left pinned: swap the program in place, no
	// detach. Only a link in a mode this run wants is a candidate; and only one
	// mode can be attached to an interface at a time, so anything else pinned
	// for it is removed once a link is adopted.
	for _, m := range plan.adopt {
		old, err := link.LoadPinnedLink(filepath.Join(PinDir, linkPinSubdir, xdpPinName(m, iface.Name)), nil)
		if err != nil {
			continue
		}
		if uerr := old.Update(prog); uerr != nil {
			// Dead (its interface was recreated) or refuses this program.
			_ = old.Unpin()
			_ = old.Close()
			continue
		}
		d.links = append(d.links, old)
		d.pinned = append(d.pinned, xdpPinName(m, iface.Name))
		d.Swapped = append(d.Swapped, xdpPinName(m, iface.Name))
		d.XDPActive = m
		dropXDPPins(iface.Name, m)
		return nil
	}

	// Nothing adopted: whatever is still pinned for this interface is stale, in
	// the wrong mode, or (without Persist) not wanted. It must go before a fresh
	// attach, or the kernel refuses with "already attached" / mode conflict.
	dropXDPPins(iface.Name, "")

	var lastErr error
	for i, m := range plan.fresh {
		lnk, err := link.AttachXDP(link.XDPOptions{Program: prog, Interface: iface.Index, Flags: xdpFlags(m)})
		if err != nil {
			lastErr = fmt.Errorf("attach xdp to %s in %s mode: %w", iface.Name, m, err)
			if m == config.XDPNative && req == config.XDPNative {
				lastErr = fmt.Errorf("%w (the driver for %s may not support native XDP; use xdpMode: auto to fall back to generic)", lastErr, iface.Name)
			}
			if i < len(plan.fresh)-1 {
				d.XDPFallback = lastErr // auto: native failed, generic is next
			}
			continue
		}
		if d.Persist {
			path := filepath.Join(PinDir, linkPinSubdir, xdpPinName(m, iface.Name))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				_ = lnk.Close()
				return fmt.Errorf("mkdir link pin dir: %w", err)
			}
			if err := lnk.Pin(path); err != nil {
				_ = lnk.Close()
				return fmt.Errorf("pin xdp link: %w", err)
			}
			d.pinned = append(d.pinned, xdpPinName(m, iface.Name))
		}
		d.links = append(d.links, lnk)
		d.XDPActive = m
		return nil
	}
	return lastErr
}

// xdpModes are the concrete attach modes (auto is a policy, not a mode).
var xdpModes = []config.XDPMode{config.XDPNative, config.XDPGeneric}

// xdpPinName names the pinned XDP link for iface in a mode. The mode is part of
// the name because a link's mode is fixed at attach time and the kernel allows
// only one mode per interface: a mode change must be seen as a different link,
// not swapped into the old one.
func xdpPinName(mode config.XDPMode, ifname string) string {
	return "xdp-" + string(mode) + "-" + ifname
}

func xdpFlags(mode config.XDPMode) link.XDPAttachFlags {
	if mode == config.XDPNative {
		return link.XDPDriverMode
	}
	return link.XDPGenericMode
}

// dropXDPPins unpins every XDP link for ifname except keep ("" keeps none),
// which detaches them.
func dropXDPPins(ifname string, keep config.XDPMode) {
	for _, m := range xdpModes {
		if m == keep {
			continue
		}
		dropPinnedLink(filepath.Join(PinDir, linkPinSubdir, xdpPinName(m, ifname)))
	}
}

// xdpPlan is how AttachXDP should bring the link up: pinned links to try
// adopting, in preference order, then modes to attach fresh, in preference
// order, until one works.
type xdpPlan struct {
	adopt []config.XDPMode
	fresh []config.XDPMode
}

// planXDP decides the plan without touching the kernel. present is which modes
// currently have a pinned link for the interface. Auto tries native then
// generic, but if only a generic link is pinned it keeps it rather than
// retrying native on every restart: that would force a detach (a traffic gap)
// each time just to fail the same way. Only a link in a mode the request allows
// is ever adopted; a pinned link in another mode is not swapped into (it can't
// change mode), which is what makes a mode change a deliberate one-off gap.
func planXDP(persist bool, req config.XDPMode, present map[config.XDPMode]bool) xdpPlan {
	var order []config.XDPMode
	switch req.Effective() {
	case config.XDPNative:
		order = []config.XDPMode{config.XDPNative}
	case config.XDPAuto:
		order = []config.XDPMode{config.XDPNative, config.XDPGeneric}
	default:
		order = []config.XDPMode{config.XDPGeneric}
	}
	plan := xdpPlan{fresh: order}
	if persist {
		for _, m := range order {
			if present[m] {
				plan.adopt = append(plan.adopt, m)
			}
		}
	}
	return plan
}

// AttachTCXEgress attaches the reverse-NAT program on iface's egress path.
func (d *Datapath) AttachTCXEgress(iface *net.Interface) error {
	if d.natColl == nil {
		return fmt.Errorf("tc_nat object not loaded")
	}
	prog, ok := d.natColl.Programs["rivora_tc_nat_egress"]
	if !ok {
		return fmt.Errorf("program rivora_tc_nat_egress not found in object")
	}
	name := "tcx-egress-" + iface.Name
	return d.attachOrSwap(name, prog, func() (link.Link, error) {
		lnk, err := link.AttachTCX(link.TCXOptions{
			Interface: iface.Index,
			Program:   prog,
			Attach:    ebpf.AttachTCXEgress,
		})
		if err != nil {
			return nil, fmt.Errorf("attach tcx egress to %s: %w", iface.Name, err)
		}
		return lnk, nil
	})
}

// attachOrSwap installs prog under the link pin name. With Persist it first
// tries to adopt a link a previous run left pinned and swap prog into it in
// place (no detach, so no traffic gap); if there is none, or it is stale, it
// attaches fresh and pins the result. Without Persist it never leaves a pin
// behind, and clears any a persisted run left so the fresh attach isn't refused
// as "already attached".
func (d *Datapath) attachOrSwap(name string, prog *ebpf.Program, attach func() (link.Link, error)) error {
	path := filepath.Join(PinDir, linkPinSubdir, name)

	if d.Persist {
		if old, err := link.LoadPinnedLink(path, nil); err == nil {
			if uerr := old.Update(prog); uerr == nil {
				d.links = append(d.links, old)
				d.pinned = append(d.pinned, name)
				d.Swapped = append(d.Swapped, name)
				return nil
			}
			// The pinned link is dead (e.g. its interface was recreated, so
			// the kernel detached it) or can't take this program. Drop it and
			// fall through to a fresh attach.
			_ = old.Unpin()
			_ = old.Close()
		} else if !errors.Is(err, os.ErrNotExist) {
			_ = os.Remove(path)
		}
	} else {
		dropPinnedLink(path)
	}

	lnk, err := attach()
	if err != nil {
		return err
	}
	if d.Persist {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			_ = lnk.Close()
			return fmt.Errorf("mkdir link pin dir: %w", err)
		}
		if err := lnk.Pin(path); err != nil {
			_ = lnk.Close()
			return fmt.Errorf("pin link %s: %w", name, err)
		}
		d.pinned = append(d.pinned, name)
	}
	d.links = append(d.links, lnk)
	return nil
}

// dropPinnedLink removes a pinned link so the kernel detaches it. Unpinning is
// what matters: once nothing references the link it is destroyed.
func dropPinnedLink(path string) {
	if l, err := link.LoadPinnedLink(path, nil); err == nil {
		_ = l.Unpin()
		_ = l.Close()
		return
	}
	_ = os.Remove(path) // unreadable/stale entry: best-effort
}

// PersistedLinks lists the names of links currently pinned under PinDir, i.e.
// datapath a previous run left attached.
func PersistedLinks() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(PinDir, linkPinSubdir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// DetachPersisted unpins every persisted link, which detaches the datapath a
// previous run left running. Maps are left in place (a later start reuses
// them). It returns the names it removed; the first error stops the sweep.
func DetachPersisted() ([]string, error) {
	names, err := PersistedLinks()
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, n := range names {
		dropPinnedLink(filepath.Join(PinDir, linkPinSubdir, n))
		if _, statErr := os.Stat(filepath.Join(PinDir, linkPinSubdir, n)); statErr == nil {
			return removed, fmt.Errorf("could not remove pinned link %s", n)
		}
		removed = append(removed, n)
	}
	return removed, nil
}

// OrphanedLinks returns pinned links this run neither created nor adopted —
// typically an interface or NAT mode dropped from the config since the
// persisted datapath was attached. They keep forwarding until detached, so the
// caller should tell the operator.
func (d *Datapath) OrphanedLinks() ([]string, error) {
	present, err := PersistedLinks()
	if err != nil {
		return nil, err
	}
	return orphans(present, d.pinned), nil
}

func orphans(present, mine []string) []string {
	own := make(map[string]bool, len(mine))
	for _, n := range mine {
		own[n] = true
	}
	var out []string
	for _, n := range present {
		if !own[n] {
			out = append(out, n)
		}
	}
	return out
}

func (d *Datapath) Close() {
	for _, l := range d.links {
		_ = l.Close()
	}
	if d.natColl != nil {
		d.natColl.Close()
	}
	if d.ingressColl != nil {
		d.ingressColl.Close()
	}
	for _, m := range d.ownedMaps {
		_ = m.Close()
	}
}
