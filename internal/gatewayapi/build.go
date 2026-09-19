// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package gatewayapi

import (
	"fmt"
	"net"

	"k8s.io/apimachinery/pkg/labels"

	gwapi "github.com/zyvorai/rivora/api/gatewayapi"
	"github.com/zyvorai/rivora/internal/config"
	"github.com/zyvorai/rivora/internal/controller"
)

// desiredVIP mirrors internal/controller's desiredVIP — one Gateway
// listener's desired dataplane state.
type desiredVIP struct {
	VIP              config.VIP
	DrainingBackends []config.Backend
}

// buildDesiredVIPs computes gw's desired dataplane state from its
// status-assigned address, its listeners, and whichever routes attach to
// each. A Gateway with no assigned address yet (rivora-controller hasn't
// run IPAM for it) yields no VIPs at all — the caller then removes
// whatever was previously installed, mirroring
// internal/controller.buildDesiredVIPs' handling of an unassigned
// Service.
func (r *Reconciler) buildDesiredVIPs(gw *gwapi.Gateway, routes []attachedRoute) ([]desiredVIP, error) {
	addrs := gatewayIPs(gw)
	if len(addrs) == 0 {
		return nil, nil
	}

	var out []desiredVIP
	for _, addr := range addrs {
		for _, l := range gw.Spec.Listeners {
			proto, ok := listenerProtocol(l.Protocol)
			if !ok {
				continue // e.g. a TLS/HTTP listener on a Gateway that also has TCP/UDP listeners: skip that one, not the whole Gateway
			}

			var backends, draining []config.Backend
			seen := map[string]bool{}
			for _, rt := range routes {
				if !r.attaches(gw, l, rt.ref) {
					continue
				}
				for _, rule := range rt.ref.Spec.Rules {
					for _, br := range rule.BackendRefs {
						if out := CheckBackendRef(rt.ref, br, r); !out.Resolved {
							r.logger.Warn("backendRef not used", "route", rt.ref.Namespace+"/"+rt.ref.Name, "backendRef", br.Name, "reason", out.Reason, "why", out.Message)
							continue
						}
						bs, bsDraining, err := r.resolveBackendRef(rt.ref.Namespace, br)
						if err != nil {
							r.logger.Error("resolve backendRef", "backendRef", br.Name, "err", err)
							continue // one bad backendRef shouldn't drop the whole listener
						}
						for _, b := range bs {
							key := fmt.Sprintf("%s:%d", b.Address, b.Port)
							if seen[key] {
								continue
							}
							seen[key] = true
							backends = append(backends, b)
						}
						draining = append(draining, bsDraining...)
					}
				}
			}
			backends = controller.SameFamily(backends, addr)
			draining = controller.SameFamily(draining, addr)
			if len(backends) == 0 {
				continue
			}

			out = append(out, desiredVIP{
				VIP: config.VIP{
					Address:  addr,
					Port:     uint16(l.Port),
					Protocol: proto,
					Mode:     config.ModeNAT, // K8s-managed VIPs are NAT-only, same as internal/controller
					Backends: backends,
				},
				DrainingBackends: draining,
			})
		}
	}
	return out, nil
}

// gatewayIPs returns every IPAddress-typed status address (IPv4 and IPv6).
func gatewayIPs(gw *gwapi.Gateway) []string {
	var out []string
	seen := map[string]bool{}
	for _, a := range gw.Status.Addresses {
		ip := net.ParseIP(a.Value)
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

func gatewayIPv4(gw *gwapi.Gateway) string {
	for _, ip := range gatewayIPs(gw) {
		if parsed := net.ParseIP(ip); parsed != nil && parsed.To4() != nil {
			return ip
		}
	}
	return ""
}

func listenerProtocol(p string) (config.Protocol, bool) {
	switch p {
	case "TCP":
		return config.ProtoTCP, true
	case "UDP":
		return config.ProtoUDP, true
	default:
		return "", false
	}
}

// attaches reports whether route rr attaches to listener l of gw through any of its parentRefs.
func (r *Reconciler) attaches(gw *gwapi.Gateway, l gwapi.GatewayListener, rr RouteRef) bool {
	for _, pr := range rr.Spec.ParentRefs {
		if attachToListener(gw, l, rr, pr, r).Attached {
			return true
		}
	}
	return false
}

// resolveBackendRef resolves one backendRef to its Service's backends
// (ready and draining, same split internal/controller.EndpointsForPort
// already returns) via that Service's own port-name mapping — a
// backendRef's Port is the Service's numeric port, unlike Service
// reconciliation which already has the port *name* in hand from
// iterating Service.Spec.Ports directly.
//
// Weight (Gateway API's native canary/traffic-split field on backendRef,
// default 1) is applied by dividing it evenly across this Service's
// current ready-endpoint count — an approximation: Gateway API's weight
// is a per-backendRef (i.e. per-Service) share, while Rivora's Maglev
// weights individual backend IPs, so the intended aggregate split is only
// reached exactly when compared backendRefs have similar endpoint counts.
func (r *Reconciler) resolveBackendRef(routeNamespace string, br gwapi.BackendRef) (backends, draining []config.Backend, err error) {
	// CheckBackendRef has already established that a cross-namespace reference is permitted.
	svcNamespace := routeNamespace
	if br.Namespace != nil && *br.Namespace != "" {
		svcNamespace = *br.Namespace
	}
	if br.Kind != nil && *br.Kind != "Service" {
		return nil, nil, fmt.Errorf("unsupported backendRef kind %q", *br.Kind)
	}
	if br.Port == nil {
		return nil, nil, fmt.Errorf("backendRef %s: port is required", br.Name)
	}

	svc, err := r.serviceLister.Services(svcNamespace).Get(br.Name)
	if err != nil {
		return nil, nil, err
	}
	portName, found := "", false
	for _, p := range svc.Spec.Ports {
		if p.Port == *br.Port {
			portName, found = p.Name, true
			break
		}
	}
	if !found {
		return nil, nil, fmt.Errorf("service %s has no port %d", br.Name, *br.Port)
	}

	slices, err := r.sliceLister.EndpointSlices(svcNamespace).List(labels.SelectorFromSet(labels.Set{
		serviceNameLabel: br.Name,
	}))
	if err != nil {
		return nil, nil, err
	}
	ready, draining := controller.EndpointsForPort(slices, portName)
	if len(ready) == 0 {
		return nil, draining, nil
	}

	weight := int32(1)
	if br.Weight != nil {
		weight = *br.Weight
	}
	perBackend := weight / int32(len(ready))
	if perBackend < 1 {
		perBackend = 1
	}
	backends = make([]config.Backend, len(ready))
	for i, b := range ready {
		b.Weight = uint32(perBackend)
		backends[i] = b
	}
	return backends, draining, nil
}
