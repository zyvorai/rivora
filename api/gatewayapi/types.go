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
	gatewayV1beta1  = schema.GroupVersion{Group: GroupName, Version: "v1beta1"}

	GatewayClassResource = gatewayV1.WithResource("gatewayclasses")
	GatewayResource      = gatewayV1.WithResource("gateways")
	TCPRouteResource     = gatewayV1alpha2.WithResource("tcproutes")
	UDPRouteResource     = gatewayV1alpha2.WithResource("udproutes")

	// ReferenceGrant is part of the standard channel, at v1beta1.
	ReferenceGrantResource = gatewayV1beta1.WithResource("referencegrants")
)

// Route kinds Rivora serves.
const (
	KindTCPRoute = "TCPRoute"
	KindUDPRoute = "UDPRoute"
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
	// AllowedRoutes says which routes may attach to this listener: by namespace (the Gateway's
	// own by default) and by kind (the kind matching the listener's protocol by default).
	AllowedRoutes *AllowedRoutes `json:"allowedRoutes,omitempty"`
}

// AllowedRoutes mirrors the upstream field of the same name.
type AllowedRoutes struct {
	Namespaces *RouteNamespaces `json:"namespaces,omitempty"`
	Kinds      []RouteGroupKind `json:"kinds,omitempty"`
}

// RouteNamespaces selects the namespaces routes may attach from: From is Same (the default: the
// Gateway's own namespace), All, or Selector (namespaces whose labels match Selector).
type RouteNamespaces struct {
	From     *string               `json:"from,omitempty"`
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
}

// RouteGroupKind names a route kind; Group defaults to gateway.networking.k8s.io.
type RouteGroupKind struct {
	Group *string `json:"group,omitempty"`
	Kind  string  `json:"kind"`
}

type GatewayAddress struct {
	Type  *string `json:"type,omitempty"` // "IPAddress" expected; nil/unset treated as IPAddress
	Value string  `json:"value"`
}

type GatewayStatus struct {
	Addresses  []GatewayAddress   `json:"addresses,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Listeners reports, per listener, the kinds it serves and how many routes are attached.
	Listeners []ListenerStatus `json:"listeners,omitempty"`
}

// ListenerStatus is one listener's entry in Gateway.status.listeners.
type ListenerStatus struct {
	Name           string             `json:"name"`
	SupportedKinds []RouteGroupKind   `json:"supportedKinds"`
	AttachedRoutes int32              `json:"attachedRoutes"`
	Conditions     []metav1.Condition `json:"conditions"`
}

// TCPRoute / UDPRoute share an identical shape upstream (RouteSpec +
// RouteStatus, generically); Rivora keeps them as two distinct Go types
// only because they're two distinct Kinds/GVRs, not because the shape
// differs.
type TCPRoute struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              RouteSpec   `json:"spec"`
	Status            RouteStatus `json:"status,omitempty"`
}

type UDPRoute struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              RouteSpec   `json:"spec"`
	Status            RouteStatus `json:"status,omitempty"`
}

// RouteStatus is the per-parent status of a route. Several controllers may share one route, each
// owning the entries carrying its own ControllerName; a writer must leave the others' alone.
type RouteStatus struct {
	Parents []RouteParentStatus `json:"parents"`
}

// RouteParentStatus says whether one parentRef was accepted and whether the route's references
// resolved.
type RouteParentStatus struct {
	ParentRef      ParentRef          `json:"parentRef"`
	ControllerName string             `json:"controllerName"`
	Conditions     []metav1.Condition `json:"conditions"`
}

// Route condition types and reasons Rivora sets.
const (
	ConditionAccepted     = "Accepted"
	ConditionResolvedRefs = "ResolvedRefs"

	ReasonAccepted              = "Accepted"
	ReasonNoMatchingParent      = "NoMatchingParent"
	ReasonNotAllowedByListeners = "NotAllowedByListeners"
	ReasonResolvedRefs          = "ResolvedRefs"
	ReasonRefNotPermitted       = "RefNotPermitted"
	ReasonInvalidKind           = "InvalidKind"
	ReasonBackendNotFound       = "BackendNotFound"
	ReasonUnsupportedProtocol   = "UnsupportedProtocol"
	ReasonListenerAccepted      = "Accepted"
	ReasonListenerResolvedRefs  = "ResolvedRefs"
)

type RouteSpec struct {
	ParentRefs []ParentRef `json:"parentRefs,omitempty"`
	Rules      []RouteRule `json:"rules"`
}

// ParentRef identifies the Gateway (and, optionally, one specific listener by name) this route
// attaches to. Namespace is the Gateway's namespace, defaulting to the route's own; a route in
// another namespace attaches only if the listener's allowedRoutes permits it.
type ParentRef struct {
	Group       *string `json:"group,omitempty"`
	Kind        *string `json:"kind,omitempty"`
	Namespace   *string `json:"namespace,omitempty"`
	Name        string  `json:"name"`
	SectionName *string `json:"sectionName,omitempty"`
}

type RouteRule struct {
	BackendRefs []BackendRef `json:"backendRefs,omitempty"`
}

// BackendRef targets a Service (any other Kind is rejected: Rivora has nothing else to route to).
// Namespace defaults to the route's own; a Service in another namespace is used only if a
// ReferenceGrant in that namespace allows this route's kind and namespace to reference it.
type BackendRef struct {
	Group     *string `json:"group,omitempty"`
	Kind      *string `json:"kind,omitempty"`
	Name      string  `json:"name"`
	Namespace *string `json:"namespace,omitempty"`
	Port      *int32  `json:"port,omitempty"`
	Weight    *int32  `json:"weight,omitempty"`
}

// ReferenceGrant lets objects of the kinds/namespaces in From reference objects of the kinds in To,
// in the ReferenceGrant's own namespace.
type ReferenceGrant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ReferenceGrantSpec `json:"spec"`
}

type ReferenceGrantSpec struct {
	From []ReferenceGrantFrom `json:"from"`
	To   []ReferenceGrantTo   `json:"to"`
}

type ReferenceGrantFrom struct {
	Group     string `json:"group"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
}

// ReferenceGrantTo names a kind (and, optionally, one object) that may be referenced. An empty
// Group is the core API.
type ReferenceGrantTo struct {
	Group string  `json:"group"`
	Kind  string  `json:"kind"`
	Name  *string `json:"name,omitempty"`
}
