// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package speaker

import (
	"testing"

	"github.com/zyvorai/rivora/internal/dataplane"
)

func TestNatVIPAddressesFiltersDSR(t *testing.T) {
	got := natVIPAddresses([]dataplane.Status{
		{VIPAddress: "10.0.0.1", Mode: "nat"},
		{VIPAddress: "10.0.0.2", Mode: "dsr"},
	})
	if len(got) != 1 || got[0].String() != "10.0.0.1" {
		t.Errorf("got %v, want only the NAT-mode VIP", got)
	}
}

func TestNatVIPAddressesDedupes(t *testing.T) {
	got := natVIPAddresses([]dataplane.Status{
		{VIPAddress: "10.0.0.1", Mode: "nat", VIPPort: 80},
		{VIPAddress: "10.0.0.1", Mode: "nat", VIPPort: 443},
	})
	if len(got) != 1 {
		t.Errorf("got %d addresses, want 1 (same VIP address, two ports)", len(got))
	}
}

func TestNatVIPAddressesSkipsUnparseable(t *testing.T) {
	got := natVIPAddresses([]dataplane.Status{
		{VIPAddress: "not-an-ip", Mode: "nat"},
	})
	if len(got) != 0 {
		t.Errorf("got %v, want none for an unparseable address", got)
	}
}

func TestNatVIPAddressesEmptyInput(t *testing.T) {
	if got := natVIPAddresses(nil); len(got) != 0 {
		t.Errorf("got %v, want none", got)
	}
}
