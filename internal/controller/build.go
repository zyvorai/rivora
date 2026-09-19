// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package controller

import (
	"fmt"
	"net"
	"net/netip"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"

	"github.com/zyvorai/rivora/internal/config"
)

// desiredVIP is one Service port's desired dataplane state: the VIP to
// UpsertVIP (backends include both ready and draining — UpsertVIP wants
// the full set, or a draining backend's affinity-tracked flows would be
// torn down) plus which of those backends are draining (applied as a
// second pass via SetBackendDrainingByKey, since UpsertVIP itself always
// marks a newly-acquired backend healthy).
type desiredVIP struct {
	VIP              config.VIP
	DrainingBackends []config.Backend
}

// buildDesiredVIPs computes what svc's dataplane state should be from its
// spec/status and the EndpointSlices that back it. K8s-managed VIPs are
// NAT-only (DSR would need binding the VIP into backend pods, a
// CNI-specific story not designed yet). IPv4 and IPv6 LoadBalancer
// ingress addresses are both programmed; backends are filtered to the
// same address family as each VIP (mixed-family behind one VIP is
// rejected by config.Validate). A Service with no assigned LoadBalancer
// IP yet, or that has no ready same-family backends, or that uses an
// unsupported protocol, yields no VIP for that port — the caller then
// just removes whatever was previously installed.
func buildDesiredVIPs(svc *corev1.Service, slices []*discoveryv1.EndpointSlice, opts buildOptions) ([]desiredVIP, error) {
	addrs := ingressIPs(svc)
	if len(addrs) == 0 {
		return nil, nil
	}

	// externalTrafficPolicy: Local restricts a VIP to this node's own endpoints, but
	// only when the caller says it is safe to (see buildOptions.LocalNode).
	localOnly := ""
	if opts.LocalNode != "" && svc.Spec.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyLocal {
		localOnly = opts.LocalNode
	}
	affinity := affinityFor(svc)

	var out []desiredVIP
	for _, addr := range addrs {
		for _, p := range svc.Spec.Ports {
			proto, ok := protocolFor(p.Protocol)
			if !ok {
				continue // SCTP or other unsupported protocol: skip this port, not the whole Service
			}

			backends, draining := endpointsForPort(slices, p.Name, localOnly, opts.Policy.weightFor)
			backends = SameFamily(backends, addr)
			draining = SameFamily(draining, addr)
			if len(backends) == 0 {
				continue
			}

			out = append(out, desiredVIP{
				VIP: config.VIP{
					Address:  addr,
					Port:     uint16(p.Port),
					Protocol: proto,
					Mode:     config.ModeNAT,
					Backends: backends,

					SessionAffinity: affinity,
					HealthCheck:     opts.Policy.probeSpec(),
					RateLimit:       opts.Policy.vipRateLimit(),
					BGPCommunities:  opts.Policy.bgpCommunities(),
				},
				DrainingBackends: draining,
			})
		}
	}
	return out, nil
}

// buildOptions carries this node's behaviour into buildDesiredVIPs.
type buildOptions struct {
	// LocalNode, when non-empty, makes a Service with externalTrafficPolicy: Local
	// use only the endpoints running on this node. It must be empty unless every
	// node that can attract this Service's traffic is one that has its endpoints.
	// That holds with BGP (a node advertises a VIP only while it has a healthy local
	// backend, so a node with none withdraws it), but NOT with the L2 speaker, where
	// one elected node answers ARP for every VIP whether or not it has the pods: a
	// Local Service would then blackhole whenever the leader has none.
	LocalNode string

	// Policy is the ServicePolicy in force for the Service, if any: its health
	// probe, per-source rate limit and endpoint weights are applied to every VIP.
	Policy *servicePolicy
}

// affinityFor maps Service.spec.sessionAffinity. ClientIP becomes source-address
// hashing. The Service's timeoutSeconds is not honoured: affinity here holds while
// the backend set is stable (and moves only a small share of clients when it
// changes), rather than expiring on a timer.
func affinityFor(svc *corev1.Service) config.SessionAffinity {
	if svc.Spec.SessionAffinity == corev1.ServiceAffinityClientIP {
		return config.AffinityClientIP
	}
	return ""
}

// ingressIPs returns every LoadBalancer ingress IP (IPv4 and IPv6), in
// status order. Hostname-only ingress entries are skipped.
func ingressIPs(svc *corev1.Service) []string {
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		if ing.IP == "" {
			continue
		}
		ip := net.ParseIP(ing.IP)
		if ip == nil {
			continue
		}
		s := ip.String()
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// ingressIPv4 is kept for callers that still want the first IPv4 ingress
// only (e.g. older tests). Prefer ingressIPs for new code.
func ingressIPv4(svc *corev1.Service) string {
	for _, ip := range ingressIPs(svc) {
		if addr, err := netip.ParseAddr(ip); err == nil && addr.Is4() {
			return ip
		}
	}
	return ""
}

func protocolFor(p corev1.Protocol) (config.Protocol, bool) {
	switch p {
	case corev1.ProtocolTCP, "":
		return config.ProtoTCP, true
	case corev1.ProtocolUDP:
		return config.ProtoUDP, true
	default:
		return "", false
	}
}

// EndpointsForPort collects every serving backend, across every
// EndpointSlice sharding this Service's endpoints, whose slice-local port
// entry matches portName. IPv4 and IPv6 addresses are both accepted;
// callers that need a single address family (because the VIP is
// family-scoped) should filter with sameFamily. Exported: reused by
// internal/gatewayapi for TCPRoute/UDPRoute backendRefs.
func EndpointsForPort(slices []*discoveryv1.EndpointSlice, portName string) (backends, draining []config.Backend) {
	return endpointsForPort(slices, portName, "", nil)
}

// endpointsForPort is EndpointsForPort with an optional node filter: a non-empty
// node keeps only endpoints that run on it. An endpoint with no node at all
// (external addresses, hand-authored slices) is not on any node, so it is dropped
// by the filter rather than assumed local. weigh, if set, gives each backend its
// weight from the endpoint it came from.
func endpointsForPort(slices []*discoveryv1.EndpointSlice, portName, node string, weigh func(discoveryv1.Endpoint) uint32) (backends, draining []config.Backend) {
	seen := map[string]bool{} // "addr:port" — dedupe across slices
	for _, slice := range slices {
		targetPort, ok := PortForName(slice.Ports, portName)
		if !ok {
			continue
		}
		for _, ep := range slice.Endpoints {
			if !endpointServing(ep) {
				continue
			}
			if node != "" && (ep.NodeName == nil || *ep.NodeName != node) {
				continue
			}
			ready := ep.Conditions.Ready != nil && *ep.Conditions.Ready
			for _, addr := range ep.Addresses {
				ip := net.ParseIP(addr)
				if ip == nil {
					continue
				}
				b := config.Backend{Address: ip.String(), Port: targetPort}
				if weigh != nil {
					b.Weight = weigh(ep)
				}
				key := fmt.Sprintf("%s:%d", b.Address, b.Port)
				if seen[key] {
					continue
				}
				seen[key] = true
				backends = append(backends, b)
				if !ready {
					draining = append(draining, b)
				}
			}
		}
	}
	return backends, draining
}

// SameFamily keeps backends whose address family matches vip.
func SameFamily(backends []config.Backend, vip string) []config.Backend {
	vipAddr, err := netip.ParseAddr(vip)
	if err != nil {
		return nil
	}
	var out []config.Backend
	for _, b := range backends {
		ba, err := netip.ParseAddr(b.Address)
		if err != nil {
			continue
		}
		if ba.Is4() == vipAddr.Is4() {
			out = append(out, b)
		}
	}
	return out
}

// PortForName is exported alongside EndpointsForPort for the same reason.
func PortForName(ports []discoveryv1.EndpointPort, name string) (uint16, bool) {
	for _, p := range ports {
		if p.Port == nil {
			continue
		}
		portName := ""
		if p.Name != nil {
			portName = *p.Name
		}
		if portName == name {
			return uint16(*p.Port), true
		}
	}
	return 0, false
}

func endpointServing(ep discoveryv1.Endpoint) bool {
	return ep.Conditions.Serving == nil || *ep.Conditions.Serving
}
