// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package ipam

import (
	"reflect"
	"testing"
)

func TestExpandPoolCIDR(t *testing.T) {
	got, err := ExpandPool([]string{"10.0.0.0/30"}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.0.0.0", "10.0.0.1", "10.0.0.2", "10.0.0.3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExpandPoolCIDRAvoidBuggyIPs(t *testing.T) {
	got, err := ExpandPool([]string{"10.0.0.0/29"}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, ip := range got {
		if ip == "10.0.0.0" {
			t.Error("avoidBuggyIPs should have skipped the network address 10.0.0.0")
		}
	}
	// /29 = 8 addresses, minus .0 = 7.
	if len(got) != 7 {
		t.Errorf("len(got) = %d, want 7", len(got))
	}
}

func TestExpandPoolRange(t *testing.T) {
	got, err := ExpandPool([]string{"10.0.0.10-10.0.0.12"}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.0.0.10", "10.0.0.11", "10.0.0.12"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExpandPoolSingleAddress(t *testing.T) {
	got, err := ExpandPool([]string{"10.0.0.5"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"10.0.0.5"}) {
		t.Errorf("got %v", got)
	}
}

func TestExpandPoolDedupsAcrossOverlappingSpecs(t *testing.T) {
	got, err := ExpandPool([]string{"10.0.0.0/30", "10.0.0.2-10.0.0.5"}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.0.0.0", "10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExpandPoolRejectsInvalidRange(t *testing.T) {
	if _, err := ExpandPool([]string{"10.0.0.20-10.0.0.10"}, false); err == nil {
		t.Error("expected error for start > end")
	}
	if _, err := ExpandPool([]string{"not-an-ip"}, false); err == nil {
		t.Error("expected error for garbage input")
	}
	if _, err := ExpandPool([]string{"2001:db8::/32"}, false); err == nil {
		t.Error("expected error for IPv6 CIDR (v0.2 is IPv4-only)")
	}
}
