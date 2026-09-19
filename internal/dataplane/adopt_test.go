// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package dataplane

import (
	"net"
	"reflect"
	"testing"

	"github.com/zyvorai/rivora/internal/bpfmaps"
	"github.com/zyvorai/rivora/internal/config"
)

// The decoders must invert exactly what vipMapWrite builds; these construct keys
// with the same expressions rather than hand-written byte values, so they fail
// if either side's endianness handling drifts.
func TestVipKeyRoundTrip4(t *testing.T) {
	for _, c := range []struct {
		addr  string
		port  uint16
		proto config.Protocol
	}{
		{"10.0.0.100", 80, config.ProtoTCP},
		{"192.168.1.1", 65535, config.ProtoUDP},
		{"1.2.3.4", 1, config.ProtoTCP},
		{"255.0.0.1", 8443, config.ProtoTCP},
	} {
		key := bpfmaps.VipKey{Addr: ip4ToBE32(net.ParseIP(c.addr).To4()), Port: htons(c.port), Proto: protoByte(c.proto)}
		addr, port, proto, ok := vipFromKey4(key)
		if !ok || addr != c.addr || port != c.port || proto != c.proto {
			t.Errorf("%v: decoded %s:%d/%s ok=%v", c, addr, port, proto, ok)
		}
	}
}

func TestVipKeyRoundTrip6(t *testing.T) {
	for _, c := range []struct {
		addr  string
		port  uint16
		proto config.Protocol
	}{
		{"fd00::100", 443, config.ProtoTCP},
		{"2001:db8::1", 53, config.ProtoUDP},
	} {
		key := bpfmaps.VipKey6{Addr: ip6To16(net.ParseIP(c.addr)), Port: htons(c.port), Proto: protoByte(c.proto)}
		addr, port, proto, ok := vipFromKey6(key)
		if !ok || addr != c.addr || port != c.port || proto != c.proto {
			t.Errorf("%v: decoded %s:%d/%s ok=%v", c, addr, port, proto, ok)
		}
	}
}

func TestVipKeyUnknownProtocolIsNotOurs(t *testing.T) {
	if _, _, _, ok := vipFromKey4(bpfmaps.VipKey{Addr: 1, Port: 1, Proto: 1}); ok {
		t.Error("ICMP-protocol key was treated as a rivorad VIP")
	}
	if _, _, _, ok := vipFromKey6(bpfmaps.VipKey6{Proto: 132}); ok {
		t.Error("SCTP-protocol key was treated as a rivorad VIP")
	}
}

func TestDistinctIDs(t *testing.T) {
	got := distinctIDs([]uint32{5, 2, 5, 9, 2, 2, 9, 0})
	if want := []uint32{0, 2, 5, 9}; !reflect.DeepEqual(got, want) {
		t.Errorf("distinctIDs = %v, want %v", got, want)
	}
	if got := distinctIDs(nil); len(got) != 0 {
		t.Errorf("distinctIDs(nil) = %v", got)
	}
}

func TestIDReserveThenAllocNeverCollides(t *testing.T) {
	a := newIDAllocator(16)
	for k, id := range map[string]uint32{"b": 3, "a": 0, "d": 7} {
		if err := a.reserve(k, id); err != nil {
			t.Fatalf("reserve %s=%d: %v", k, id, err)
		}
	}
	held := map[uint32]string{3: "b", 0: "a", 7: "d"}
	// New allocations must fill the gaps and then grow, never reusing a held ID.
	for i := 0; i < 10; i++ {
		key := string(rune('k' + i))
		id, err := a.Alloc(key)
		if err != nil {
			t.Fatal(err)
		}
		if owner, taken := held[id]; taken {
			t.Fatalf("Alloc(%s) returned id %d, already held by %s", key, id, owner)
		}
		held[id] = key
	}
	if len(held) != 13 {
		t.Errorf("expected 13 distinct ids, got %d", len(held))
	}
}

func TestIDReserveIsIdempotentAndRejectsConflicts(t *testing.T) {
	a := newIDAllocator(8)
	if err := a.reserve("x", 2); err != nil {
		t.Fatal(err)
	}
	if err := a.reserve("x", 2); err != nil {
		t.Errorf("re-reserving the same key/id should be a no-op, got %v", err)
	}
	if err := a.reserve("x", 3); err == nil {
		t.Error("one key under two ids was accepted")
	}
	if err := a.reserve("y", 2); err == nil {
		t.Error("two keys sharing one id were accepted (that is exactly the aliasing adoption must refuse)")
	}
	if err := a.reserve("z", 8); err == nil {
		t.Error("an out-of-range id was accepted")
	}
	// A rejected reserve must not have changed anything.
	if id, ok := a.Get("x"); !ok || id != 2 {
		t.Errorf("x = %d,%v after rejected reserves, want 2,true", id, ok)
	}
	if _, ok := a.Get("y"); ok {
		t.Error("rejected key y was recorded")
	}
}

func TestIDReserveClaimsAFreedSlot(t *testing.T) {
	a := newIDAllocator(8)
	for _, k := range []string{"a", "b", "c"} { // ids 0,1,2
		if _, err := a.Alloc(k); err != nil {
			t.Fatal(err)
		}
	}
	a.Release("b") // id 1 goes on the free list
	if err := a.reserve("late", 1); err != nil {
		t.Fatal(err)
	}
	id, _ := a.Alloc("new")
	if id == 1 {
		t.Error("Alloc handed out id 1 after it was reserved")
	}
}

func TestExtentReserveSplitsAndRestores(t *testing.T) {
	a := newExtentAllocator(1000)
	if err := a.reserve(extent{offset: 300, size: 100}); err != nil {
		t.Fatal(err)
	}
	want := []extent{{0, 300}, {400, 600}}
	if !reflect.DeepEqual(a.free, want) {
		t.Fatalf("free = %v, want %v", a.free, want)
	}
	a.Free(extent{offset: 300, size: 100})
	if !reflect.DeepEqual(a.free, []extent{{0, 1000}}) {
		t.Errorf("after Free the pool did not coalesce back: %v", a.free)
	}
}

func TestExtentReserveEdgesAndErrors(t *testing.T) {
	a := newExtentAllocator(100)
	if err := a.reserve(extent{offset: 0, size: 10}); err != nil { // touches the start
		t.Fatal(err)
	}
	if err := a.reserve(extent{offset: 90, size: 10}); err != nil { // touches the end
		t.Fatal(err)
	}
	if err := a.reserve(extent{offset: 5, size: 10}); err == nil {
		t.Error("an extent overlapping an already-reserved one was accepted")
	}
	if err := a.reserve(extent{offset: 95, size: 10}); err == nil {
		t.Error("an extent running past the table was accepted")
	}
	if err := a.reserve(extent{offset: 10, size: 0}); err == nil {
		t.Error("an empty extent was accepted")
	}
	if err := a.reserve(extent{offset: ^uint32(0) - 1, size: 10}); err == nil {
		t.Error("an extent whose end overflows was accepted")
	}
	if !reflect.DeepEqual(a.free, []extent{{10, 80}}) {
		t.Errorf("rejected reserves changed the pool: %v", a.free)
	}
}

func TestExtentReserveThenAllocDoesNotOverlap(t *testing.T) {
	a := newExtentAllocator(65537)
	if err := a.reserve(extent{offset: 1031, size: 4099}); err != nil {
		t.Fatal(err)
	}
	reserved := extent{offset: 1031, size: 4099}
	for i := 0; i < 5; i++ {
		e, err := a.Alloc(1031)
		if err != nil {
			t.Fatal(err)
		}
		if e.offset < reserved.offset+reserved.size && reserved.offset < e.offset+e.size {
			t.Fatalf("Alloc returned %v overlapping the reserved %v", e, reserved)
		}
	}
}
