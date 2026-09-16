// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package loader loads and attaches Rivora's compiled BPF objects, pinning
// maps under /sys/fs/bpf/rivora-lb so a rivorad restart reuses live state
// instead of dropping connection affinity and stats. Modeled on the
// load/pin pattern in netra's internal/agent (LoadCollectionSpec +
// MapReplacements from pinned maps + link.AttachXDP/AttachTCX).
package loader

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
)

const PinDir = "/sys/fs/bpf/rivora-lb"

// Datapath holds the loaded collection and live attachments for the ingress
// XDP program and, when NAT mode is in use, the TCX egress un-NAT program.
type Datapath struct {
	ingressColl *ebpf.Collection
	natColl     *ebpf.Collection
	links       []link.Link

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
	lnk, err := link.AttachXDP(link.XDPOptions{
		Program:   prog,
		Interface: iface.Index,
		Flags:     link.XDPGenericMode,
	})
	if err != nil {
		return fmt.Errorf("attach xdp to %s: %w", iface.Name, err)
	}
	d.links = append(d.links, lnk)
	return nil
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
	lnk, err := link.AttachTCX(link.TCXOptions{
		Interface: iface.Index,
		Program:   prog,
		Attach:    ebpf.AttachTCXEgress,
	})
	if err != nil {
		return fmt.Errorf("attach tcx egress to %s: %w", iface.Name, err)
	}
	d.links = append(d.links, lnk)
	return nil
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
}
