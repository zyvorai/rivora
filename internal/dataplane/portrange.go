// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package dataplane

import (
	"errors"
	"fmt"
	"math/bits"
	"net"

	"github.com/cilium/ebpf"

	"github.com/zyvorai/rivora/internal/bpfmaps"
	"github.com/zyvorai/rivora/internal/config"
)

// A port range is not a map key, so range VIPs live in LPM tries (vip_range_map,
// vip_range_map6; see struct vip_range_key in bpf/rivora_common.h). The trie matches
// on prefixes, so a range is stored as the aligned power-of-two blocks that exactly
// tile it: 30000-30100 is five blocks, 1-65535 is at most thirty. A lookup with the
// packet's full port then returns the block that contains it.

// portBlock is an aligned block of 2^(16-Bits) ports starting at Port: the first Bits
// bits of the port are fixed, the rest are free.
type portBlock struct {
	Port uint16
	Bits int
}

func (b portBlock) size() uint32 { return 1 << (16 - b.Bits) }

// rangeBlocks tiles lo..hi (inclusive, lo <= hi) with the fewest aligned blocks.
func rangeBlocks(lo, hi uint16) []portBlock {
	var out []portBlock
	cur, end := uint32(lo), uint32(hi)
	for cur <= end {
		size := uint32(1)
		// Grow while the block stays aligned to its own size and inside the range.
		for size < 1<<15 && cur%(size*2) == 0 && cur+size*2-1 <= end {
			size *= 2
		}
		out = append(out, portBlock{Port: uint16(cur), Bits: 16 - (bits.Len32(size) - 1)})
		cur += size
	}
	return out
}

func (d *Dataplane) rangeMap(is4 bool) *ebpf.Map {
	if is4 {
		return d.dp.Maps[bpfmaps.MapVIPRange]
	}
	return d.dp.Maps[bpfmaps.MapVIPRange6]
}

// rangeKey4 and rangeKey6 build a block's trie key.
func rangeKey4(ip4 net.IP, proto uint8, b portBlock) bpfmaps.VipRangeKey {
	return bpfmaps.VipRangeKey{
		PrefixLen: uint32(bpfmaps.RangePrefixBase4 + b.Bits),
		Addr:      ip4ToBE32(ip4), Proto: proto, Port: htons(b.Port),
	}
}

func rangeKey6(ip net.IP, proto uint8, b portBlock) bpfmaps.VipRangeKey6 {
	return bpfmaps.VipRangeKey6{
		PrefixLen: uint32(bpfmaps.RangePrefixBase6 + b.Bits),
		Addr:      ip6To16(ip), Proto: proto, Port: htons(b.Port),
	}
}

// rangeMapWrite installs every block of vip's range, pointing at serviceID. A
// datapath object built before ranges existed has no trie: that fails loudly rather
// than leaving the VIP silently unserved.
func (d *Dataplane) rangeMapWrite(ip net.IP, vip config.VIP, proto uint8, serviceID uint32) error {
	ip4 := ip.To4()
	m := d.rangeMap(ip4 != nil)
	if m == nil {
		return errors.New("this datapath object has no port-range support; rebuild the BPF objects")
	}
	blocks := rangeBlocks(vip.Port, vip.PortEnd)
	for i, b := range blocks {
		var err error
		if ip4 != nil {
			k := rangeKey4(ip4, proto, b)
			err = m.Update(&k, serviceID, ebpf.UpdateAny)
		} else {
			k := rangeKey6(ip, proto, b)
			err = m.Update(&k, serviceID, ebpf.UpdateAny)
		}
		if err != nil {
			// Don't leave a half-installed range answering for part of its ports.
			d.rangeMapDeleteBlocks(ip, proto, blocks[:i])
			return fmt.Errorf("update vip_range_map: %w", err)
		}
	}
	return nil
}

func (d *Dataplane) rangeMapDelete(ip net.IP, vip config.VIP, proto uint8) error {
	d.rangeMapDeleteBlocks(ip, proto, rangeBlocks(vip.Port, vip.PortEnd))
	return nil
}

func (d *Dataplane) rangeMapDeleteBlocks(ip net.IP, proto uint8, blocks []portBlock) {
	ip4 := ip.To4()
	m := d.rangeMap(ip4 != nil)
	if m == nil {
		return
	}
	for _, b := range blocks {
		if ip4 != nil {
			k := rangeKey4(ip4, proto, b)
			_ = m.Delete(&k)
		} else {
			k := rangeKey6(ip, proto, b)
			_ = m.Delete(&k)
		}
	}
}
