// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package installer

import "testing"

func TestLoadChart(t *testing.T) {
	chrt, err := LoadChart()
	if err != nil {
		t.Fatalf("LoadChart: %v", err)
	}
	if chrt.Metadata == nil || chrt.Metadata.Name != "rivora" {
		t.Errorf("chart name = %+v, want %q", chrt.Metadata, "rivora")
	}
	if len(chrt.Templates) == 0 {
		t.Error("chart has no templates — embed likely picked up the wrong directory")
	}
	if chrt.Values == nil {
		t.Error("chart has no default values.yaml")
	}
	// The AddressPool CRD is a chart-root crds/ file, not a template — make
	// sure the embed captured it too, since install() relies on Helm's own
	// CRD-install-once handling of chrt.CRDObjects().
	if len(chrt.CRDObjects()) == 0 {
		t.Error("chart has no CRDs — crds/addresspool-crd.yaml likely missing from the embed")
	}
}
