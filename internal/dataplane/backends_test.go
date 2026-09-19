// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package dataplane

import "testing"

func TestBackendsOfLabelsAndOrdersEveryVIP(t *testing.T) {
	statuses := []Status{
		{VIPAddress: "10.0.0.2", VIPPort: 443, Protocol: "tcp", Backends: []BackendStatus{{ID: 5, Weight: 3}, {ID: 1}}},
		{VIPAddress: "10.0.0.1", VIPPort: 80, Protocol: "tcp", Backends: []BackendStatus{{ID: 1, Weight: 2}}},
		{VIPAddress: "10.0.0.3", VIPPort: 30000, VIPPortEnd: 30100, Protocol: "udp", Backends: []BackendStatus{{ID: 9}}},
	}
	got := backendsOf(statuses)
	want := []struct {
		vip string
		id  uint32
		w   uint32
	}{
		{"10.0.0.1:80:tcp", 1, 2},
		{"10.0.0.2:443:tcp", 1, 0}, // backend 1 serves two VIPs: one row each, with that VIP's weight
		{"10.0.0.2:443:tcp", 5, 3},
		{"10.0.0.3:30000-30100:udp", 9, 0},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].VIP != w.vip || got[i].ID != w.id || got[i].Weight != w.w {
			t.Errorf("row %d = {%s %d %d}, want {%s %d %d}", i, got[i].VIP, got[i].ID, got[i].Weight, w.vip, w.id, w.w)
		}
	}
	// The key is the form `rivoractl weight --vip` takes.
	if k := statuses[0].VIPKey(); k != "10.0.0.2:443:tcp" {
		t.Errorf("VIPKey = %q", k)
	}
}

func TestBackendsOfNothingIsAnEmptyListNotNull(t *testing.T) {
	if got := backendsOf(nil); got == nil || len(got) != 0 {
		t.Errorf("want an empty non-nil slice (it is JSON-encoded as []), got %#v", got)
	}
}
