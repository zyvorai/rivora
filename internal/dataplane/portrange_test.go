// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package dataplane

import (
	"math/rand"
	"testing"
)

// covers is the LPM rule: block b matches port p when b's leading bits equal p's.
func (b portBlock) covers(p uint16) bool {
	if b.Bits == 0 {
		return true
	}
	shift := 16 - b.Bits
	return uint32(p)>>shift == uint32(b.Port)>>shift
}

func TestRangeBlocksTileExactly(t *testing.T) {
	cases := [][2]uint16{
		{1, 2}, {80, 81}, {30000, 30100}, {1024, 2047}, {1, 65535}, {5, 65534}, {65534, 65535},
		{32768, 65535}, {1, 32767}, {100, 100}, {7, 8},
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 400; i++ {
		lo := uint16(rng.Intn(65535) + 1)
		hi := uint16(int(lo) + rng.Intn(65536-int(lo)))
		cases = append(cases, [2]uint16{lo, hi})
	}
	for _, c := range cases {
		lo, hi := c[0], c[1]
		blocks := rangeBlocks(lo, hi)
		if len(blocks) == 0 || len(blocks) > 30 {
			t.Fatalf("%d-%d: %d blocks", lo, hi, len(blocks))
		}
		// Contiguous, aligned to its own size, in order: the tiling is exact.
		next := uint32(lo)
		for _, b := range blocks {
			if uint32(b.Port) != next {
				t.Fatalf("%d-%d: block at %d, want %d (gap or overlap): %+v", lo, hi, b.Port, next, blocks)
			}
			if uint32(b.Port)%b.size() != 0 {
				t.Fatalf("%d-%d: block %+v is not aligned to its size", lo, hi, b)
			}
			next += b.size()
		}
		if next != uint32(hi)+1 {
			t.Fatalf("%d-%d: tiling ends at %d", lo, hi, next-1)
		}
	}
}

// TestRangeBlocksMatchLikeAnLPMTrie checks what actually matters: looking up every port
// with the trie's prefix rule finds exactly one block inside the range and none outside.
func TestRangeBlocksMatchLikeAnLPMTrie(t *testing.T) {
	for _, c := range [][2]uint16{{30000, 30100}, {1, 65535}, {1000, 1000 + 1023}, {443, 445}, {65000, 65535}} {
		lo, hi := c[0], c[1]
		blocks := rangeBlocks(lo, hi)
		for p := 0; p <= 65535; p++ {
			n := 0
			for _, b := range blocks {
				if b.covers(uint16(p)) {
					n++
				}
			}
			inside := p >= int(lo) && p <= int(hi)
			if inside && n != 1 {
				t.Fatalf("%d-%d: port %d matched %d blocks, want exactly 1", lo, hi, p, n)
			}
			if !inside && n != 0 {
				t.Fatalf("%d-%d: port %d is outside the range but matched %d blocks", lo, hi, p, n)
			}
		}
	}
}

func TestBlocksSpanRoundTripsAndRejectsDamage(t *testing.T) {
	blocks := rangeBlocks(30000, 30100)
	lo, hi, ok := blocksSpan(blocks)
	if !ok || lo != 30000 || hi != 30100 {
		t.Fatalf("blocksSpan = %d-%d ok=%v, want 30000-30100", lo, hi, ok)
	}
	// A block missing from the middle is a torn write: it must not be adopted as a range.
	if _, _, ok := blocksSpan(append(append([]portBlock(nil), blocks[:2]...), blocks[3:]...)); ok {
		t.Error("a range with a block missing was accepted")
	}
	if _, _, ok := blocksSpan(nil); ok {
		t.Error("no blocks was accepted")
	}
	if _, _, ok := blocksSpan([]portBlock{{Port: 80, Bits: 17}}); ok {
		t.Error("an impossible prefix length was accepted")
	}
	// A lone single-port block is an exact port, not a range.
	if _, _, ok := blocksSpan([]portBlock{{Port: 80, Bits: 16}}); ok {
		t.Error("a single-port block was accepted as a range")
	}
}
