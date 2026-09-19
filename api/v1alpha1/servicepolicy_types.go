// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ServicePolicyResource / ServicePolicyKind address ServicePolicy objects.
var (
	ServicePolicyResource = GroupVersion.WithResource("servicepolicies")
	ServicePolicyKind     = GroupVersion.WithKind("ServicePolicy")
)

// ServicePolicySpec tunes how Rivora load-balances one Service, for the settings
// Kubernetes' own Service API has no field for: how its backends are probed, how
// fast one client may open connections, and how traffic is weighted across
// endpoints. Settings that are left out keep their defaults, so a policy that
// sets only healthCheck changes only that.
type ServicePolicySpec struct {
	// TargetRef names the Service this policy applies to, in the policy's own
	// namespace. Only Services are supported.
	TargetRef PolicyTargetRef `json:"targetRef"`

	// HealthCheck replaces the default probe (a TCP connect to the endpoint's
	// port) for this Service's endpoints.
	HealthCheck *HealthCheckPolicy `json:"healthCheck,omitempty"`

	// RateLimit caps how quickly one source address may open new TCP
	// connections (SYN packets) to this Service's VIPs. It replaces the node-wide
	// limit for these VIPs; established connections and UDP are not affected.
	RateLimit *RateLimitPolicy `json:"rateLimit,omitempty"`

	// Weights sets each endpoint's share of new connections.
	Weights *WeightPolicy `json:"weights,omitempty"`

	// BGP tunes how this Service's VIP is advertised when rivorad runs the BGP speaker.
	BGP *BGPPolicy `json:"bgp,omitempty"`
}

// BGPPolicy is the per-Service part of BGP advertisement.
type BGPPolicy struct {
	// Communities are added to the route advertised for this Service's VIPs, on top of the
	// speaker's own: "65000:100" (both halves 0-65535) or no-export, no-advertise,
	// no-export-subconfed.
	Communities []string `json:"communities,omitempty"`
	// Peers limits the route advertised for this Service's VIPs to the BGP peers with these
	// addresses. Unset means every peer.
	Peers []string `json:"peers,omitempty"`
}

// PolicyTargetRef points a policy at a Service.
type PolicyTargetRef struct {
	// Kind defaults to Service, the only supported kind.
	Kind string `json:"kind,omitempty"`
	// Name of a Service in the policy's namespace.
	Name string `json:"name"`
}

// HealthCheckPolicy mirrors config.ProbeSpec.
type HealthCheckPolicy struct {
	// Type is tcp (default) or http.
	Type string `json:"type,omitempty"`
	// Port probes this port instead of the endpoint's service port. 0 means the
	// service port.
	Port uint16 `json:"port,omitempty"`
	// Path, Host and ExpectStatus apply to type http only.
	Path         string `json:"path,omitempty"`
	Host         string `json:"host,omitempty"`
	ExpectStatus string `json:"expectStatus,omitempty"`
}

// RateLimitPolicy is a per-source token bucket on new connections.
type RateLimitPolicy struct {
	PerSourcePacketsPerSecond uint64 `json:"perSourcePacketsPerSecond"`
	Burst                     uint64 `json:"burst"`
}

// WeightPolicy weights endpoints. An endpoint's weight is the entry in Nodes for
// the node it runs on, else Default, else 1. Weights are relative: 2 gets twice
// the connections of 1.
type WeightPolicy struct {
	// Default applies to endpoints on nodes not listed in Nodes. 1..1000.
	Default uint32 `json:"default,omitempty"`
	// Nodes maps a node name to the weight of the endpoints running on it. 1..1000.
	Nodes map[string]uint32 `json:"nodes,omitempty"`
}

// ServicePolicy is namespaced: it lives beside the Service it tunes.
type ServicePolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ServicePolicySpec `json:"spec,omitempty"`
}

type ServicePolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []ServicePolicy `json:"items"`
}
