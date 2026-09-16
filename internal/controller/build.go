// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package controller

import (
	"fmt"
	"net"

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
// NAT-only (see plan: DSR would need binding the VIP into backend pods, a
// CNI-specific story not designed for v0.2) and IPv4-only, matching
// internal/ipam's scope. A Service with no assigned LoadBalancer IP yet
// (rivora-controller hasn't run IPAM for it), or that has no ready
// backends, or that uses an unsupported protocol, yields no VIP for that
// port — the caller then just removes whatever was previously installed.
func buildDesiredVIPs(svc *corev1.Service, slices []*discoveryv1.EndpointSlice) ([]desiredVIP, error) {
	addr := ingressIPv4(svc)
	if addr == "" {
		return nil, nil
	}

	var out []desiredVIP
	for _, p := range svc.Spec.Ports {
		proto, ok := protocolFor(p.Protocol)
		if !ok {
			continue // SCTP or other unsupported protocol: skip this port, not the whole Service
		}

		backends, draining := endpointsForPort(slices, p.Name)
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
			},
			DrainingBackends: draining,
		})
	}
	return out, nil
}

func ingressIPv4(svc *corev1.Service) string {
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		return ""
	}
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		if ing.IP == "" {
			continue
		}
		if ip := net.ParseIP(ing.IP).To4(); ip != nil {
			return ip.String()
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

// endpointsForPort collects every serving backend, across every
// EndpointSlice sharding this Service's endpoints, whose slice-local port
// entry matches portName (Kubernetes matches Service ports to EndpointPort
// entries by name; an unnamed single-port Service has portName == "" and
// each slice has exactly one port whose Name is also ""). A "not serving"
// endpoint (fully torn down, not just terminating) is skipped entirely; a
// serving-but-not-ready one is included so its established connections
// keep flowing, and reported back in the draining slice so the caller
// excludes it from new-flow selection.
func endpointsForPort(slices []*discoveryv1.EndpointSlice, portName string) (backends, draining []config.Backend) {
	seen := map[string]bool{} // "addr:port" — dedupe across slices, defensive against overlap
	for _, slice := range slices {
		targetPort, ok := portForName(slice.Ports, portName)
		if !ok {
			continue
		}
		for _, ep := range slice.Endpoints {
			if !endpointServing(ep) {
				continue
			}
			ready := ep.Conditions.Ready != nil && *ep.Conditions.Ready
			for _, addr := range ep.Addresses {
				ip := net.ParseIP(addr).To4()
				if ip == nil {
					continue // IPv6 endpoint: v0.2 is IPv4-only
				}
				b := config.Backend{Address: ip.String(), Port: targetPort}
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

func portForName(ports []discoveryv1.EndpointPort, name string) (uint16, bool) {
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

// endpointServing reports whether ep should be programmed into the
// dataplane at all (as opposed to draining: still programmed, just
// excluded from new-flow selection — see endpointsForPort). Conditions
// default to true when unset per the EndpointSlice API's documented
// zero-value semantics.
func endpointServing(ep discoveryv1.Endpoint) bool {
	return ep.Conditions.Serving == nil || *ep.Conditions.Serving
}
