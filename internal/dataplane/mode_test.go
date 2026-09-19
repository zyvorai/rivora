// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package dataplane

import (
	"testing"

	"github.com/zyvorai/rivora/internal/bpfmaps"
	"github.com/zyvorai/rivora/internal/config"
)

// A restart adopts VIPs from the maps, so the mode byte must round-trip every mode: one that
// came back as plain DSR would forward a tunnel VIP by rewriting a MAC nobody set.
func TestModeFromByteCoversEveryMode(t *testing.T) {
	for b, want := range map[uint8]config.Mode{
		bpfmaps.ModeDSR:        config.ModeDSR,
		bpfmaps.ModeNAT:        config.ModeNAT,
		bpfmaps.ModeTunnelIPIP: config.ModeDSRIPIP,
		bpfmaps.ModeTunnelGRE:  config.ModeDSRGRE,
	} {
		if got := modeFromByte(b); got != want {
			t.Errorf("modeFromByte(%d) = %q, want %q", b, got, want)
		}
	}
	// The four values must be distinct or two modes would be indistinguishable in the map.
	seen := map[uint8]bool{}
	for _, b := range []uint8{bpfmaps.ModeDSR, bpfmaps.ModeNAT, bpfmaps.ModeTunnelIPIP, bpfmaps.ModeTunnelGRE} {
		if seen[b] {
			t.Errorf("mode byte %d is used twice", b)
		}
		seen[b] = true
	}
}
