// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/zyvorai/rivora/internal/config"
)

func ptr[T any](v T) *T { return &v }

func lbService(ports ...corev1.ServicePort) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "svc"},
		Spec: corev1.ServiceSpec{
			Type:  corev1.ServiceTypeLoadBalancer,
			Ports: ports,
		},
		Status: corev1.ServiceStatus{
			LoadBalancer: corev1.LoadBalancerStatus{
				Ingress: []corev1.LoadBalancerIngress{{IP: "10.0.0.5"}},
			},
		},
	}
}

func slice(portName string, port int32, endpoints ...discoveryv1.Endpoint) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns",
			Name:      "svc-abcde",
			Labels:    map[string]string{serviceNameLabel: "svc"},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Name: ptr(portName), Port: ptr(port)}},
		Endpoints:   endpoints,
	}
}

func TestBuildDesiredVIPsNoIngressYieldsNothing(t *testing.T) {
	svc := lbService(corev1.ServicePort{Name: "http", Port: 80})
	svc.Status.LoadBalancer.Ingress = nil
	got, err := buildDesiredVIPs(svc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %d VIPs with no assigned ingress IP, want 0", len(got))
	}
}

func TestBuildDesiredVIPsBasic(t *testing.T) {
	svc := lbService(corev1.ServicePort{Name: "http", Port: 80, Protocol: corev1.ProtocolTCP})
	sl := slice("http", 8080, discoveryv1.Endpoint{
		Addresses:  []string{"10.1.0.1"},
		Conditions: discoveryv1.EndpointConditions{Ready: ptr(true), Serving: ptr(true)},
	})

	got, err := buildDesiredVIPs(svc, []*discoveryv1.EndpointSlice{sl})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d VIPs, want 1", len(got))
	}
	vip := got[0].VIP
	if vip.Address != "10.0.0.5" || vip.Port != 80 || vip.Protocol != config.ProtoTCP || vip.Mode != config.ModeNAT {
		t.Errorf("unexpected VIP: %+v", vip)
	}
	if len(vip.Backends) != 1 || vip.Backends[0].Address != "10.1.0.1" || vip.Backends[0].Port != 8080 {
		t.Errorf("unexpected backends: %+v", vip.Backends)
	}
	if len(got[0].DrainingBackends) != 0 {
		t.Errorf("expected no draining backends, got %+v", got[0].DrainingBackends)
	}
}

func TestBuildDesiredVIPsSkipsUnsupportedProtocol(t *testing.T) {
	svc := lbService(corev1.ServicePort{Name: "sctp", Port: 80, Protocol: corev1.ProtocolSCTP})
	got, err := buildDesiredVIPs(svc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %d VIPs for an SCTP port, want 0", len(got))
	}
}

func TestBuildDesiredVIPsSkipsPortWithNoReadyBackends(t *testing.T) {
	svc := lbService(corev1.ServicePort{Name: "http", Port: 80})
	got, err := buildDesiredVIPs(svc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %d VIPs with zero backends, want 0", len(got))
	}
}

func TestBuildDesiredVIPsNotReadyIsIncludedAsDraining(t *testing.T) {
	svc := lbService(corev1.ServicePort{Name: "http", Port: 80})
	sl := slice("http", 8080, discoveryv1.Endpoint{
		Addresses:  []string{"10.1.0.1"},
		Conditions: discoveryv1.EndpointConditions{Ready: ptr(false), Serving: ptr(true), Terminating: ptr(true)},
	})

	got, err := buildDesiredVIPs(svc, []*discoveryv1.EndpointSlice{sl})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].VIP.Backends) != 1 {
		t.Fatalf("expected the terminating-but-serving backend to still be programmed: %+v", got)
	}
	if len(got[0].DrainingBackends) != 1 || got[0].DrainingBackends[0].Address != "10.1.0.1" {
		t.Errorf("expected it reported as draining, got %+v", got[0].DrainingBackends)
	}
}

func TestBuildDesiredVIPsNotServingIsExcludedEntirely(t *testing.T) {
	svc := lbService(corev1.ServicePort{Name: "http", Port: 80})
	sl := slice("http", 8080, discoveryv1.Endpoint{
		Addresses:  []string{"10.1.0.1"},
		Conditions: discoveryv1.EndpointConditions{Ready: ptr(false), Serving: ptr(false)},
	})

	got, err := buildDesiredVIPs(svc, []*discoveryv1.EndpointSlice{sl})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected a fully-not-serving endpoint to yield no VIP at all, got %+v", got)
	}
}

func TestBuildDesiredVIPsDedupesAcrossSlices(t *testing.T) {
	svc := lbService(corev1.ServicePort{Name: "http", Port: 80})
	ep := discoveryv1.Endpoint{
		Addresses:  []string{"10.1.0.1"},
		Conditions: discoveryv1.EndpointConditions{Ready: ptr(true), Serving: ptr(true)},
	}
	slA := slice("http", 8080, ep)
	slB := slice("http", 8080, ep)
	slB.Name = "svc-fghij"

	got, err := buildDesiredVIPs(svc, []*discoveryv1.EndpointSlice{slA, slB})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].VIP.Backends) != 1 {
		t.Errorf("expected the same endpoint across two slices to dedupe to one backend, got %+v", got)
	}
}

func TestBuildDesiredVIPsMultiplePorts(t *testing.T) {
	svc := lbService(
		corev1.ServicePort{Name: "http", Port: 80},
		corev1.ServicePort{Name: "https", Port: 443},
	)
	slHTTP := slice("http", 8080, discoveryv1.Endpoint{
		Addresses:  []string{"10.1.0.1"},
		Conditions: discoveryv1.EndpointConditions{Ready: ptr(true), Serving: ptr(true)},
	})
	slHTTPS := slice("https", 8443, discoveryv1.Endpoint{
		Addresses:  []string{"10.1.0.1"},
		Conditions: discoveryv1.EndpointConditions{Ready: ptr(true), Serving: ptr(true)},
	})
	slHTTPS.Name = "svc-https"

	got, err := buildDesiredVIPs(svc, []*discoveryv1.EndpointSlice{slHTTP, slHTTPS})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d VIPs, want 2 (one per Service port)", len(got))
	}
	ports := map[uint16]uint16{} // vip port -> backend port
	for _, d := range got {
		ports[d.VIP.Port] = d.VIP.Backends[0].Port
	}
	if ports[80] != 8080 || ports[443] != 8443 {
		t.Errorf("unexpected port mapping: %+v", ports)
	}
}

func TestBuildDesiredVIPsAcceptsIPv6Endpoints(t *testing.T) {
	svc := lbService(corev1.ServicePort{Name: "http", Port: 80})
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "2001:db8::100"}}
	sl := slice("http", 8080, discoveryv1.Endpoint{
		Addresses:  []string{"2001:db8::1"},
		Conditions: discoveryv1.EndpointConditions{Ready: ptr(true), Serving: ptr(true)},
	})
	sl.AddressType = discoveryv1.AddressTypeIPv6
	got, err := buildDesiredVIPs(svc, []*discoveryv1.EndpointSlice{sl})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected an IPv6 VIP with IPv6 backend, got %+v", got)
	}
	if got[0].VIP.Address != "2001:db8::100" {
		t.Errorf("VIP address = %q, want 2001:db8::100", got[0].VIP.Address)
	}
	if len(got[0].VIP.Backends) != 1 || got[0].VIP.Backends[0].Address != "2001:db8::1" {
		t.Errorf("backends = %+v, want 2001:db8::1", got[0].VIP.Backends)
	}
}

func TestBuildDesiredVIPsFiltersCrossFamilyBackends(t *testing.T) {
	svc := lbService(corev1.ServicePort{Name: "http", Port: 80})
	v4 := slice("http", 8080, discoveryv1.Endpoint{
		Addresses:  []string{"10.1.0.1"},
		Conditions: discoveryv1.EndpointConditions{Ready: ptr(true), Serving: ptr(true)},
	})
	v6 := slice("http", 8080, discoveryv1.Endpoint{
		Addresses:  []string{"2001:db8::1"},
		Conditions: discoveryv1.EndpointConditions{Ready: ptr(true), Serving: ptr(true)},
	})
	v6.AddressType = discoveryv1.AddressTypeIPv6
	got, err := buildDesiredVIPs(svc, []*discoveryv1.EndpointSlice{v4, v6})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d VIPs, want 1", len(got))
	}
	if len(got[0].VIP.Backends) != 1 || got[0].VIP.Backends[0].Address != "10.1.0.1" {
		t.Errorf("expected only the IPv4 backend for an IPv4 VIP, got %+v", got[0].VIP.Backends)
	}
}

// TestBuildDesiredVIPsKubeVirtVM captures the exact EndpointSlice shape a
// live KubeVirt VirtualMachineInstance produces once its virt-launcher Pod
// is scheduled and ready (verified against a real VMI on the v0.3 test
// cluster) — EndpointsForPort never reads TargetRef, so it's
// indistinguishable from a plain Pod-backed Service; this pins that down
// as a regression test rather than relying only on that read of the code.
func TestBuildDesiredVIPsKubeVirtVM(t *testing.T) {
	svc := lbService(corev1.ServicePort{Port: 80}) // unnamed port, matching the real Service used
	sl := slice("", 80, discoveryv1.Endpoint{
		Addresses: []string{"10.42.0.28"},
		Conditions: discoveryv1.EndpointConditions{
			Ready: ptr(true), Serving: ptr(true), Terminating: ptr(false),
		},
		TargetRef: &corev1.ObjectReference{
			Kind:      "Pod",
			Name:      "virt-launcher-rivora-vmtest-pb5fw",
			Namespace: "rivora-vmtest",
		},
	})

	got, err := buildDesiredVIPs(svc, []*discoveryv1.EndpointSlice{sl})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].VIP.Backends) != 1 || got[0].VIP.Backends[0].Address != "10.42.0.28" {
		t.Fatalf("expected the VM's virt-launcher pod IP as a normal backend, got %+v", got)
	}
}

// TestBuildDesiredVIPsManualEndpointSliceExternalBackend captures a
// selector-less Service fronting a hand-authored EndpointSlice pointing at
// an address outside the cluster entirely (no TargetRef at all) — the
// standard, zero-new-code Kubernetes mechanism for a "physical/external
// backend" (verified live: Kubernetes' EndpointSlice controller leaves a
// manually-created slice alone as long as the owning Service has no
// selector). EndpointsForPort doesn't require TargetRef, so this works
// identically to any other backend.
func TestBuildDesiredVIPsManualEndpointSliceExternalBackend(t *testing.T) {
	svc := lbService(corev1.ServicePort{Port: 80}) // no Spec.Selector — matches a hand-authored Service+EndpointSlice pair
	sl := slice("", 8080, discoveryv1.Endpoint{
		Addresses: []string{"203.0.113.50"}, // TEST-NET-3, standing in for a real external IP
		Conditions: discoveryv1.EndpointConditions{
			Ready: ptr(true), Serving: ptr(true), Terminating: ptr(false),
		},
	})

	got, err := buildDesiredVIPs(svc, []*discoveryv1.EndpointSlice{sl})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].VIP.Backends) != 1 || got[0].VIP.Backends[0].Address != "203.0.113.50" {
		t.Fatalf("expected the external IP as a normal backend with no TargetRef needed, got %+v", got)
	}
}
