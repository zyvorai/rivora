// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BGPPeerResource / BGPPeerKind address BGPPeer objects.
var (
	BGPPeerResource = GroupVersion.WithResource("bgppeers")
	BGPPeerKind     = GroupVersion.WithKind("BGPPeer")
)

// BGPPeerSpec is one BGP neighbour for the rivorad speakers (started with -bgp or -bgp-config).
// A BGPPeer is cluster-scoped: it applies to every node that its nodeSelector matches, so one
// object can give each rack's nodes their own top-of-rack router. The peers a node is started
// with (flags or -bgp-config) stay; a BGPPeer is added to them, and one with the same address as a
// startup peer is ignored, so that peer's settings are not silently replaced.
type BGPPeerSpec struct {
	// Address is the neighbour's IPv4 or IPv6 address.
	Address string `json:"address"`
	// ASN is the neighbour's AS number.
	ASN uint32 `json:"asn"`
	// BFD enables Bidirectional Forwarding Detection on the session.
	BFD bool `json:"bfd,omitempty"`
	// Multihop is the TTL for an eBGP session to a router more than one hop away, 2-255.
	Multihop uint32 `json:"multihop,omitempty"`
	// GracefulRestart negotiates BGP graceful restart with this neighbour.
	GracefulRestart *BGPPeerGracefulRestart `json:"gracefulRestart,omitempty"`
	// PasswordSecretRef reads the TCP MD5 password from a Secret in the namespace rivorad runs in
	// (the peer must have the same one). Rotating the Secret re-establishes the session within a
	// minute.
	PasswordSecretRef *SecretKeyRef `json:"passwordSecretRef,omitempty"`
	// NodeSelector limits the peer to nodes whose labels match. Unset means every node.
	NodeSelector *metav1.LabelSelector `json:"nodeSelector,omitempty"`
}

// BGPPeerGracefulRestart mirrors config.BGPGracefulRestart.
type BGPPeerGracefulRestart struct {
	Enabled bool `json:"enabled"`
	// RestartTime is how long, in seconds, the neighbour holds this node's routes while it
	// restarts. Default 120, at most 4095.
	RestartTime uint32 `json:"restartTime,omitempty"`
}

// SecretKeyRef names one key of a Secret in rivorad's own namespace. Referencing other namespaces
// is deliberately not possible: rivorad is only granted read access to Secrets in its own.
type SecretKeyRef struct {
	Name string `json:"name"`
	// Key defaults to "password".
	Key string `json:"key,omitempty"`
}

type BGPPeer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec BGPPeerSpec `json:"spec,omitempty"`
}

type BGPPeerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []BGPPeer `json:"items"`
}
