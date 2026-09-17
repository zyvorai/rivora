// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package dataplane

import (
	"net"
	"testing"

	"github.com/zyvorai/rivora/internal/config"
)

func TestIP6To16RoundTripsRawAddressBytes(t *testing.T) {
	ip := net.ParseIP("fd00:77::11")
	if ip == nil {
		t.Fatal("test address failed to parse")
	}
	got := ip6To16(ip)
	want := ip.To16()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ip6To16(%s)[%d] = %#x, want %#x", ip, i, got[i], want[i])
		}
	}
}

func TestIP6To16DiffersForDifferentAddresses(t *testing.T) {
	a := ip6To16(net.ParseIP("fd00:77::11"))
	b := ip6To16(net.ParseIP("fd00:77::12"))
	if a == b {
		t.Fatalf("expected distinct addresses to produce distinct byte arrays, both were %v", a)
	}
}

func TestProtoByte(t *testing.T) {
	if got := protoByte(config.ProtoTCP); got != 6 {
		t.Errorf("protoByte(tcp) = %d, want 6 (IPPROTO_TCP)", got)
	}
	if got := protoByte(config.ProtoUDP); got != 17 {
		t.Errorf("protoByte(udp) = %d, want 17 (IPPROTO_UDP)", got)
	}
}
