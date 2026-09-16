// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AddressPoolSpec mirrors MetalLB's IPAddressPool shape — the de facto
// convention for this problem, so operators already familiar with it get
// no surprises.
type AddressPoolSpec struct {
	// Addresses is a list of CIDRs ("10.0.0.0/24") and/or explicit ranges
	// ("10.0.0.10-10.0.0.40") this pool allocates from.
	Addresses []string `json:"addresses"`

	// Protocol is reserved for future forwarding-advertisement modes;
	// v0.2 only implements "layer2" (the L2/ARP speaker).
	Protocol string `json:"protocol,omitempty"`

	// AutoAssign controls whether this pool is used for Services that
	// don't request a specific pool. Defaults to true.
	AutoAssign *bool `json:"autoAssign,omitempty"`

	// AvoidBuggyIPs skips a CIDR's .0 and .255 addresses (some older
	// client stacks mishandle them), matching MetalLB's own option name.
	AvoidBuggyIPs bool `json:"avoidBuggyIPs,omitempty"`
}

// AddressPoolStatus carries only coarse counts, not per-allocation
// state — allocation/release happens via the *Service* objects
// themselves (see internal/ipam), so this never becomes a
// status-subresource contention point under Service churn. It's updated
// periodically for observability, not read by the allocator itself.
type AddressPoolStatus struct {
	AvailableIPs int64              `json:"availableIPs,omitempty"`
	AssignedIPs  int64              `json:"assignedIPs,omitempty"`
	Conditions   []metav1.Condition `json:"conditions,omitempty"`
}

// AddressPool is cluster-scoped: one node's view of "here's a range of
// VIPs rivora-controller may hand out."
type AddressPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AddressPoolSpec   `json:"spec,omitempty"`
	Status AddressPoolStatus `json:"status,omitempty"`
}

type AddressPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []AddressPool `json:"items"`
}
