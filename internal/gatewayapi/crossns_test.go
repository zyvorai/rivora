// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package gatewayapi

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	gwapi "github.com/zyvorai/rivora/api/gatewayapi"
	"github.com/zyvorai/rivora/internal/config"
)

func readyEndpoint(addr string) discoveryv1.Endpoint {
	return discoveryv1.Endpoint{
		Addresses:  []string{addr},
		Conditions: discoveryv1.EndpointConditions{Ready: ptr(true), Serving: ptr(true)},
	}
}

func namespaceObj(name string, lbls map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: lbls}}
}

// routeIn builds a TCPRoute in namespace ns that attaches to Gateway infra/gw.
func routeIn(ns, name string, refs ...map[string]interface{}) *unstructured.Unstructured {
	rt := tcpRouteObj(ns, name, "gw", refs...)
	spec := rt.Object["spec"].(map[string]interface{})
	spec["parentRefs"] = []interface{}{map[string]interface{}{"name": "gw", "namespace": "infra"}}
	return rt
}

func withAllowedRoutes(l map[string]interface{}, ar map[string]interface{}) map[string]interface{} {
	l["allowedRoutes"] = ar
	return l
}

func grantObj(ns, name, fromNS, fromKind string, toName string) *unstructured.Unstructured {
	to := map[string]interface{}{"group": "", "kind": "Service"}
	if toName != "" {
		to["name"] = toName
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "gateway.networking.k8s.io/v1beta1",
		"kind":       "ReferenceGrant",
		"metadata":   map[string]interface{}{"namespace": ns, "name": name},
		"spec": map[string]interface{}{
			"from": []interface{}{map[string]interface{}{"group": gwapi.GroupName, "kind": fromKind, "namespace": fromNS}},
			"to":   []interface{}{to},
		},
	}}
}

func programmed(t *testing.T, plane *fakeDataplane) []config.VIP {
	t.Helper()
	plane.mu.Lock()
	defer plane.mu.Unlock()
	return append([]config.VIP(nil), plane.upserts...)
}

// A route in another namespace attaches only where the listener's allowedRoutes says so.
func TestRouteFromAnotherNamespaceNeedsAllowedRoutes(t *testing.T) {
	svc := svcObj("apps", "svc", 8080)
	sl := sliceObj("apps", "svc", 8080, readyEndpoint("10.1.0.1"))
	all := map[string]interface{}{"namespaces": map[string]interface{}{"from": "All"}}
	selector := func(k, v string) map[string]interface{} {
		return map[string]interface{}{"namespaces": map[string]interface{}{
			"from":     "Selector",
			"selector": map[string]interface{}{"matchLabels": map[string]interface{}{k: v}},
		}}
	}

	cases := []struct {
		name      string
		listener  map[string]interface{}
		wantVIPs  int
		namespace *corev1.Namespace
	}{
		{"default (Same) refuses another namespace", listener("tcp", "TCP", 80), 0, namespaceObj("apps", nil)},
		{"from: All admits it", withAllowedRoutes(listener("tcp", "TCP", 80), all), 1, namespaceObj("apps", nil)},
		{"a matching selector admits it", withAllowedRoutes(listener("tcp", "TCP", 80), selector("team", "app")), 1, namespaceObj("apps", map[string]string{"team": "app"})},
		{"a selector that does not match refuses it", withAllowedRoutes(listener("tcp", "TCP", 80), selector("team", "app")), 0, namespaceObj("apps", map[string]string{"team": "other"})},
	}
	for _, c := range cases {
		dyn := newDynamicClient()
		createObj(t, dyn, gwapi.GatewayClassResource, gatewayClassObj("rivora", gwapi.ControllerName))
		createObj(t, dyn, gwapi.GatewayResource, gatewayObj("infra", "gw", "rivora", []string{"10.0.0.100"}, c.listener))
		createObj(t, dyn, gwapi.TCPRouteResource, routeIn("apps", "route", backendRef("svc", 8080)))
		plane := newFakeDataplane()
		r, _, cancel := startSynced(t, dyn, plane, []runtime.Object{svc, sl, c.namespace}...)
		if err := r.reconcile("infra/gw"); err != nil {
			cancel()
			t.Fatal(err)
		}
		if got := programmed(t, plane); len(got) != c.wantVIPs {
			t.Errorf("%s: %d VIPs programmed, want %d: %+v", c.name, len(got), c.wantVIPs, got)
		} else if c.wantVIPs == 1 && (len(got[0].Backends) != 1 || got[0].Backends[0].Address != "10.1.0.1") {
			t.Errorf("%s: backends %+v, want the apps Service's endpoint", c.name, got[0].Backends)
		}
		cancel()
	}
}

// A backendRef into another namespace is used only when a ReferenceGrant there permits it.
func TestCrossNamespaceBackendRefNeedsAReferenceGrant(t *testing.T) {
	svc := svcObj("data", "db", 5432)
	sl := sliceObj("data", "db", 5432, readyEndpoint("10.2.0.1"))
	ref := backendRef("db", 5432)
	ref["namespace"] = "data"

	cases := []struct {
		name  string
		grant *unstructured.Unstructured
		want  int
	}{
		{"no grant", nil, 0},
		{"a grant for this route kind and namespace", grantObj("data", "g", "infra", "TCPRoute", ""), 1},
		{"a grant naming this Service", grantObj("data", "g", "infra", "TCPRoute", "db"), 1},
		{"a grant naming another Service", grantObj("data", "g", "infra", "TCPRoute", "cache"), 0},
		{"a grant for another route namespace", grantObj("data", "g", "elsewhere", "TCPRoute", ""), 0},
		{"a grant for another route kind", grantObj("data", "g", "infra", "UDPRoute", ""), 0},
	}
	for _, c := range cases {
		dyn := newDynamicClient()
		createObj(t, dyn, gwapi.GatewayClassResource, gatewayClassObj("rivora", gwapi.ControllerName))
		createObj(t, dyn, gwapi.GatewayResource, gatewayObj("infra", "gw", "rivora", []string{"10.0.0.100"}, listener("tcp", "TCP", 5432)))
		createObj(t, dyn, gwapi.TCPRouteResource, tcpRouteObj("infra", "route", "gw", ref))
		if c.grant != nil {
			createObj(t, dyn, gwapi.ReferenceGrantResource, c.grant)
		}
		plane := newFakeDataplane()
		r, _, cancel := startSynced(t, dyn, plane, svc, sl)
		if err := r.reconcile("infra/gw"); err != nil {
			cancel()
			t.Fatal(err)
		}
		got := programmed(t, plane)
		if len(got) != c.want {
			t.Errorf("%s: %d VIPs programmed, want %d: %+v", c.name, len(got), c.want, got)
		} else if c.want == 1 && (len(got[0].Backends) != 1 || got[0].Backends[0].Address != "10.2.0.1") {
			t.Errorf("%s: backends %+v, want the data namespace's endpoint", c.name, got[0].Backends)
		}
		cancel()
	}
}

func TestRouteKindMustMatchTheListenerProtocol(t *testing.T) {
	dyn := newDynamicClient()
	createObj(t, dyn, gwapi.GatewayClassResource, gatewayClassObj("rivora", gwapi.ControllerName))
	// One UDP listener; a TCPRoute names it: it must not become a UDP VIP.
	createObj(t, dyn, gwapi.GatewayResource, gatewayObj("infra", "gw", "rivora", []string{"10.0.0.100"}, listener("dns", "UDP", 53)))
	createObj(t, dyn, gwapi.TCPRouteResource, tcpRouteObj("infra", "route", "gw", backendRef("svc", 8080)))
	plane := newFakeDataplane()
	r, _, cancel := startSynced(t, dyn, plane, svcObj("infra", "svc", 8080), sliceObj("infra", "svc", 8080, readyEndpoint("10.1.0.1")))
	defer cancel()
	if err := r.reconcile("infra/gw"); err != nil {
		t.Fatal(err)
	}
	if got := programmed(t, plane); len(got) != 0 {
		t.Errorf("a TCPRoute attached to a UDP listener: %+v", got)
	}
}

// Routes in two namespaces, one Gateway: each listener serves only the routes it admits.
func TestListenersServeOnlyTheRoutesTheyAdmit(t *testing.T) {
	dyn := newDynamicClient()
	createObj(t, dyn, gwapi.GatewayClassResource, gatewayClassObj("rivora", gwapi.ControllerName))
	open := withAllowedRoutes(listener("open", "TCP", 80), map[string]interface{}{"namespaces": map[string]interface{}{"from": "All"}})
	closed := listener("closed", "TCP", 81) // Same namespace only
	createObj(t, dyn, gwapi.GatewayResource, gatewayObj("infra", "gw", "rivora", []string{"10.0.0.100"}, open, closed))
	createObj(t, dyn, gwapi.TCPRouteResource, routeIn("apps", "route", backendRef("svc", 8080)))
	plane := newFakeDataplane()
	r, _, cancel := startSynced(t, dyn, plane, svcObj("apps", "svc", 8080), sliceObj("apps", "svc", 8080, readyEndpoint("10.1.0.1")))
	defer cancel()
	if err := r.reconcile("infra/gw"); err != nil {
		t.Fatal(err)
	}
	got := programmed(t, plane)
	if len(got) != 1 || got[0].Port != 80 {
		t.Errorf("only the open listener (80) should serve the apps route, got %+v", got)
	}
}
