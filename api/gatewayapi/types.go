// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package gatewayapi holds just enough of the upstream Kubernetes Gateway
// API (github.com/kubernetes-sigs/gateway-api, group
// gateway.networking.k8s.io — NOT a Rivora-owned API) to read the four
// resource kinds internal/gatewayapi and internal/ipamctrl need:
// GatewayClass, Gateway, TCPRoute, UDPRoute. Deliberately not the full
// upstream API surface, and no dependency on sigs.k8s.io/gateway-api's
// generated clientset — accessed via k8s.io/client-go/dynamic (same
// pattern api/v1alpha1 already established for AddressPool), converting
// to/from these types with runtime's unstructured converter. Field names
// must match the upstream CRDs' JSON exactly; Rivora never installs these
// CRDs itself (see deploy/helm/rivora/README.md) — they're a shared,
// cross-vendor standard the cluster operator installs separately.
package gatewayapi

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const GroupName = "gateway.networking.k8s.io"

// ControllerName is the value Rivora expects in
// GatewayClass.spec.controllerName to consider itself responsible for a
// class (and, transitively, every Gateway referencing it). Not
// configurable — a single well-known name, matching how api/v1alpha1's
// AddressPool group name is also hardcoded rather than a config option.
const ControllerName = "rivora.zyvor.dev/gateway-controller"

var (
	// GatewayClass and Gateway are part of the Gateway API "standard"
	// channel, promoted to v1.
	gatewayV1 = schema.GroupVersion{Group: GroupName, Version: "v1"}
	// TCPRoute/UDPRoute are part of the "experimental" channel — v1alpha2
	// is, as of writing, the only version they've shipped at.
	gatewayV1alpha2 = schema.GroupVersion{Group: GroupName, Version: "v1alpha2"}

	GatewayClassResource = gatewayV1.WithResource("gatewayclasses")
	GatewayResource      = gatewayV1.WithResource("gateways")
	TCPRouteResource     = gatewayV1alpha2.WithResource("tcproutes")
	UDPRouteResource     = gatewayV1alpha2.WithResource("udproutes")
)

// GatewayClass — spec.controllerName only; status is written, not read.
type GatewayClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              GatewayClassSpec `json:"spec"`
}

type GatewayClassSpec struct {
	ControllerName string `json:"controllerName"`
}

// IsManagedClass reports whether gc names Rivora as its controller —
// shared by internal/gatewayapi (the per-node dataplane reconciler) and
// internal/ipamctrl (the leader-elected IPAM reconciler), which both need
// the identical test.
func IsManagedClass(gc *GatewayClass) bool {
	return gc.Spec.ControllerName == ControllerName
}

// Gateway — only the fields Rivora reads/writes: which class it belongs
// to, its listeners (name/protocol/port — no TLS section, Rivora is L4
// passthrough only), an optional user-requested address, and the
// addresses/conditions rivora-controller's IPAM writes to status.
type Gateway struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              GatewaySpec   `json:"spec"`
	Status            GatewayStatus `json:"status,omitempty"`
}

type GatewaySpec struct {
	GatewayClassName string            `json:"gatewayClassName"`
	Listeners        []GatewayListener `json:"listeners"`
	Addresses        []GatewayAddress  `json:"addresses,omitempty"`
}

type GatewayListener struct {
	Name     string `json:"name"`
	Protocol string `json:"protocol"` // "TCP" or "UDP" — anything else is skipped, not an error
	Port     int32  `json:"port"`
}

type GatewayAddress struct {
	Type  *string `json:"type,omitempty"` // "IPAddress" expected; nil/unset treated as IPAddress
	Value string  `json:"value"`
}

type GatewayStatus struct {
	Addresses  []GatewayAddress   `json:"addresses,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// TCPRoute / UDPRoute share an identical shape upstream (RouteSpec +
// RouteStatus, generically); Rivora keeps them as two distinct Go types
// only because they're two distinct Kinds/GVRs, not because the shape
// differs.
type TCPRoute struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              RouteSpec `json:"spec"`
}

type UDPRoute struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              RouteSpec `json:"spec"`
}

type RouteSpec struct {
	ParentRefs []ParentRef `json:"parentRefs,omitempty"`
	Rules      []RouteRule `json:"rules"`
}

// ParentRef identifies the Gateway (and, optionally, one specific
// listener by name) this route attaches to. Namespace is deliberately
// not read here — v1 only supports a route attaching to a Gateway in its
// own namespace (see reconciler doc comments); a cross-namespace
// ParentRef is simply never matched, not specially rejected.
type ParentRef struct {
	Name        string  `json:"name"`
	SectionName *string `json:"sectionName,omitempty"`
}

type RouteRule struct {
	BackendRefs []BackendRef `json:"backendRefs,omitempty"`
}

// BackendRef targets a Service (Group/Kind are read implicitly as the
// core-API Service default — any other Kind is skipped rather than
// erroring, since Rivora has nothing else to route to) in the Route's own
// namespace only (Namespace set = cross-namespace = requires a
// ReferenceGrant Rivora doesn't check yet — skipped, not rejected
// outright, matching the "unsupported input yields no VIP for that piece,
// not a hard failure" pattern internal/controller's buildDesiredVIPs
// already uses for e.g. SCTP ports).
type BackendRef struct {
	Group     *string `json:"group,omitempty"`
	Kind      *string `json:"kind,omitempty"`
	Name      string  `json:"name"`
	Namespace *string `json:"namespace,omitempty"`
	Port      *int32  `json:"port,omitempty"`
	Weight    *int32  `json:"weight,omitempty"`
}
