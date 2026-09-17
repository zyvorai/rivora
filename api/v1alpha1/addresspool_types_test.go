// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package v1alpha1

import (
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestAddressPoolJSONRoundTrip catches a typo'd json struct tag — the wire
// format the Kubernetes API server actually stores and serves — which a
// Go compiler has no way to flag on its own.
func TestAddressPoolJSONRoundTrip(t *testing.T) {
	autoAssign := true
	original := AddressPool{
		TypeMeta:   metav1.TypeMeta{Kind: "AddressPool", APIVersion: "rivora.zyvor.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: AddressPoolSpec{
			Addresses:     []string{"10.0.0.0/24", "2001:db8::/64"},
			Protocol:      "layer2",
			AutoAssign:    &autoAssign,
			AvoidBuggyIPs: true,
		},
		Status: AddressPoolStatus{
			AvailableIPs: 250,
			AssignedIPs:  4,
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal into map: %v", err)
	}
	spec, ok := raw["spec"].(map[string]any)
	if !ok {
		t.Fatalf("spec field missing or wrong shape in %s", data)
	}
	for _, field := range []string{"addresses", "protocol", "autoAssign", "avoidBuggyIPs"} {
		if _, ok := spec[field]; !ok {
			t.Errorf("spec.%s missing from marshaled JSON: %s", field, data)
		}
	}

	var decoded AddressPool
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal into AddressPool: %v", err)
	}
	if len(decoded.Spec.Addresses) != 2 || decoded.Spec.Addresses[0] != "10.0.0.0/24" {
		t.Errorf("Spec.Addresses = %v after round-trip", decoded.Spec.Addresses)
	}
	if decoded.Spec.AutoAssign == nil || *decoded.Spec.AutoAssign != true {
		t.Errorf("Spec.AutoAssign = %v after round-trip, want true", decoded.Spec.AutoAssign)
	}
	if decoded.Status.AvailableIPs != 250 || decoded.Status.AssignedIPs != 4 {
		t.Errorf("Status = %+v after round-trip", decoded.Status)
	}
}

// TestAddressPoolListJSONRoundTrip covers the List type separately since it
// embeds ListMeta (not ObjectMeta) and an Items slice, a different shape
// from AddressPool itself.
func TestAddressPoolListJSONRoundTrip(t *testing.T) {
	original := AddressPoolList{
		TypeMeta: metav1.TypeMeta{Kind: "AddressPoolList", APIVersion: "rivora.zyvor.dev/v1alpha1"},
		Items: []AddressPool{
			{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "v6"}},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded AddressPoolList
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded.Items) != 2 || decoded.Items[0].Name != "default" || decoded.Items[1].Name != "v6" {
		t.Errorf("Items = %+v after round-trip", decoded.Items)
	}
}
