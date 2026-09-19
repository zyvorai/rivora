// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package gatewayapi

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gwapi "github.com/zyvorai/rivora/api/gatewayapi"
)

type fakeEnv struct {
	nsLabels map[string]map[string]string
	grants   map[string][]gwapi.ReferenceGrant
	services map[string]bool // "ns/name"
}

func (f fakeEnv) NamespaceLabels(ns string) (map[string]string, bool) {
	l, ok := f.nsLabels[ns]
	return l, ok
}
func (f fakeEnv) Grants(ns string) []gwapi.ReferenceGrant { return f.grants[ns] }
func (f fakeEnv) ServiceExists(ns, name string) bool      { return f.services[ns+"/"+name] }

func sp(s string) *string { return &s }

func gateway(listeners ...gwapi.GatewayListener) *gwapi.Gateway {
	gw := &gwapi.Gateway{Spec: gwapi.GatewaySpec{Listeners: listeners}}
	gw.Namespace, gw.Name = "infra", "gw"
	return gw
}

func route(kind, ns string, parents ...gwapi.ParentRef) RouteRef {
	return RouteRef{Kind: kind, Namespace: ns, Name: "r", Spec: gwapi.RouteSpec{ParentRefs: parents}}
}

func TestEvaluateParentAttachmentRules(t *testing.T) {
	tcp := gwapi.GatewayListener{Name: "tcp", Protocol: "TCP", Port: 80}
	udp := gwapi.GatewayListener{Name: "udp", Protocol: "UDP", Port: 53}
	env := fakeEnv{nsLabels: map[string]map[string]string{
		"infra": {"team": "platform"}, "apps": {"team": "app", "env": "prod"}, "other": {"team": "x"},
	}}
	ref := gwapi.ParentRef{Name: "gw", Namespace: sp("infra")}
	sameNS := gwapi.ParentRef{Name: "gw"} // no namespace: the route's own

	from := func(f string, sel *metav1.LabelSelector) *gwapi.AllowedRoutes {
		return &gwapi.AllowedRoutes{Namespaces: &gwapi.RouteNamespaces{From: &f, Selector: sel}}
	}
	withAR := func(l gwapi.GatewayListener, ar *gwapi.AllowedRoutes) gwapi.GatewayListener {
		l.AllowedRoutes = ar
		return l
	}

	cases := []struct {
		name      string
		gw        *gwapi.Gateway
		rr        RouteRef
		pr        gwapi.ParentRef
		accepted  bool
		reason    string
		listeners []string
	}{
		{"same namespace, default allowedRoutes", gateway(tcp), route(gwapi.KindTCPRoute, "infra", sameNS), sameNS, true, gwapi.ReasonAccepted, []string{"tcp"}},
		{"another namespace is refused by default (Same)", gateway(tcp), route(gwapi.KindTCPRoute, "apps", ref), ref, false, gwapi.ReasonNotAllowedByListeners, nil},
		{"from: All admits any namespace", gateway(withAR(tcp, from("All", nil))), route(gwapi.KindTCPRoute, "apps", ref), ref, true, gwapi.ReasonAccepted, []string{"tcp"}},
		{"selector matches the route's namespace",
			gateway(withAR(tcp, from("Selector", &metav1.LabelSelector{MatchLabels: map[string]string{"team": "app"}}))),
			route(gwapi.KindTCPRoute, "apps", ref), ref, true, gwapi.ReasonAccepted, []string{"tcp"}},
		{"selector does not match",
			gateway(withAR(tcp, from("Selector", &metav1.LabelSelector{MatchLabels: map[string]string{"team": "app"}}))),
			route(gwapi.KindTCPRoute, "other", ref), ref, false, gwapi.ReasonNotAllowedByListeners, nil},
		{"selector with matchExpressions",
			gateway(withAR(tcp, from("Selector", &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "env", Operator: metav1.LabelSelectorOpIn, Values: []string{"prod"}}}}))),
			route(gwapi.KindTCPRoute, "apps", ref), ref, true, gwapi.ReasonAccepted, []string{"tcp"}},
		{"selector but no selector given", gateway(withAR(tcp, from("Selector", nil))), route(gwapi.KindTCPRoute, "apps", ref), ref, false, gwapi.ReasonNotAllowedByListeners, nil},
		{"unknown namespace never matches a selector",
			gateway(withAR(tcp, from("Selector", &metav1.LabelSelector{MatchLabels: map[string]string{"team": "app"}}))),
			route(gwapi.KindTCPRoute, "ghost", ref), ref, false, gwapi.ReasonNotAllowedByListeners, nil},
		{"unknown from value", gateway(withAR(tcp, from("Everyone", nil))), route(gwapi.KindTCPRoute, "apps", ref), ref, false, gwapi.ReasonNotAllowedByListeners, nil},
		{"a UDPRoute does not attach to a TCP listener", gateway(tcp), route(gwapi.KindUDPRoute, "infra", sameNS), sameNS, false, gwapi.ReasonNotAllowedByListeners, nil},
		{"a UDPRoute attaches to the UDP listener beside a TCP one", gateway(tcp, udp), route(gwapi.KindUDPRoute, "infra", sameNS), sameNS, true, gwapi.ReasonAccepted, []string{"udp"}},
		{"sectionName picks one listener", gateway(tcp, gwapi.GatewayListener{Name: "tcp2", Protocol: "TCP", Port: 81}),
			route(gwapi.KindTCPRoute, "infra", gwapi.ParentRef{Name: "gw", SectionName: sp("tcp2")}), gwapi.ParentRef{Name: "gw", SectionName: sp("tcp2")}, true, gwapi.ReasonAccepted, []string{"tcp2"}},
		{"no sectionName attaches to every matching listener", gateway(tcp, gwapi.GatewayListener{Name: "tcp2", Protocol: "TCP", Port: 81}),
			route(gwapi.KindTCPRoute, "infra", sameNS), sameNS, true, gwapi.ReasonAccepted, []string{"tcp", "tcp2"}},
		{"a sectionName matching nothing is NoMatchingParent", gateway(tcp),
			route(gwapi.KindTCPRoute, "infra", gwapi.ParentRef{Name: "gw", SectionName: sp("nope")}), gwapi.ParentRef{Name: "gw", SectionName: sp("nope")}, false, gwapi.ReasonNoMatchingParent, nil},
		{"another Gateway's name is NoMatchingParent", gateway(tcp), route(gwapi.KindTCPRoute, "infra", gwapi.ParentRef{Name: "other"}), gwapi.ParentRef{Name: "other"}, false, gwapi.ReasonNoMatchingParent, nil},
		{"the Gateway in another namespace is not this one", gateway(tcp),
			route(gwapi.KindTCPRoute, "infra", gwapi.ParentRef{Name: "gw", Namespace: sp("elsewhere")}), gwapi.ParentRef{Name: "gw", Namespace: sp("elsewhere")}, false, gwapi.ReasonNoMatchingParent, nil},
		{"a parentRef of another kind is not a Gateway", gateway(tcp),
			route(gwapi.KindTCPRoute, "infra", gwapi.ParentRef{Name: "gw", Kind: sp("Service")}), gwapi.ParentRef{Name: "gw", Kind: sp("Service")}, false, gwapi.ReasonNoMatchingParent, nil},
		{"allowedRoutes.kinds may exclude the route's kind",
			gateway(withAR(tcp, &gwapi.AllowedRoutes{Kinds: []gwapi.RouteGroupKind{{Kind: "HTTPRoute"}}})), route(gwapi.KindTCPRoute, "infra", sameNS), sameNS, false, gwapi.ReasonNotAllowedByListeners, nil},
		{"allowedRoutes.kinds may list it",
			gateway(withAR(tcp, &gwapi.AllowedRoutes{Kinds: []gwapi.RouteGroupKind{{Kind: "TCPRoute"}}})), route(gwapi.KindTCPRoute, "infra", sameNS), sameNS, true, gwapi.ReasonAccepted, []string{"tcp"}},
		{"a listener protocol Rivora does not serve", gateway(gwapi.GatewayListener{Name: "h", Protocol: "HTTP", Port: 80}), route(gwapi.KindTCPRoute, "infra", sameNS), sameNS, false, gwapi.ReasonNotAllowedByListeners, nil},
	}
	for _, c := range cases {
		got := EvaluateParent(c.gw, c.rr, c.pr, env)
		if got.Accepted != c.accepted || got.Reason != c.reason {
			t.Errorf("%s: accepted=%v reason=%q (%s), want accepted=%v reason=%q", c.name, got.Accepted, got.Reason, got.Message, c.accepted, c.reason)
			continue
		}
		if len(got.Listeners) != len(c.listeners) {
			t.Errorf("%s: listeners %v, want %v", c.name, got.Listeners, c.listeners)
			continue
		}
		for i := range c.listeners {
			if got.Listeners[i] != c.listeners[i] {
				t.Errorf("%s: listeners %v, want %v", c.name, got.Listeners, c.listeners)
			}
		}
	}
}

func TestCheckBackendRef(t *testing.T) {
	grant := func(ns string, from []gwapi.ReferenceGrantFrom, to []gwapi.ReferenceGrantTo) map[string][]gwapi.ReferenceGrant {
		return map[string][]gwapi.ReferenceGrant{ns: {{Spec: gwapi.ReferenceGrantSpec{From: from, To: to}}}}
	}
	fromApps := []gwapi.ReferenceGrantFrom{{Group: gwapi.GroupName, Kind: "TCPRoute", Namespace: "apps"}}
	toSvc := []gwapi.ReferenceGrantTo{{Group: "", Kind: "Service"}}
	services := map[string]bool{"apps/local": true, "data/db": true, "data/cache": true}

	rr := route(gwapi.KindTCPRoute, "apps")
	cross := func(name string) gwapi.BackendRef { return gwapi.BackendRef{Name: name, Namespace: sp("data")} }

	cases := []struct {
		name   string
		env    fakeEnv
		rr     RouteRef
		br     gwapi.BackendRef
		ok     bool
		reason string
	}{
		{"same namespace needs no grant", fakeEnv{services: services}, rr, gwapi.BackendRef{Name: "local"}, true, gwapi.ReasonResolvedRefs},
		{"explicit own namespace needs no grant", fakeEnv{services: services}, rr, gwapi.BackendRef{Name: "local", Namespace: sp("apps")}, true, gwapi.ReasonResolvedRefs},
		{"missing Service", fakeEnv{services: services}, rr, gwapi.BackendRef{Name: "nope"}, false, gwapi.ReasonBackendNotFound},
		{"another namespace with no grant is refused", fakeEnv{services: services}, rr, cross("db"), false, gwapi.ReasonRefNotPermitted},
		{"a grant allows it", fakeEnv{services: services, grants: grant("data", fromApps, toSvc)}, rr, cross("db"), true, gwapi.ReasonResolvedRefs},
		{"a grant scoped to one Service allows only that one",
			fakeEnv{services: services, grants: grant("data", fromApps, []gwapi.ReferenceGrantTo{{Kind: "Service", Name: sp("db")}})}, rr, cross("cache"), false, gwapi.ReasonRefNotPermitted},
		{"...and allows that one",
			fakeEnv{services: services, grants: grant("data", fromApps, []gwapi.ReferenceGrantTo{{Kind: "Service", Name: sp("db")}})}, rr, cross("db"), true, gwapi.ReasonResolvedRefs},
		{"a grant for another route namespace does not help",
			fakeEnv{services: services, grants: grant("data", []gwapi.ReferenceGrantFrom{{Group: gwapi.GroupName, Kind: "TCPRoute", Namespace: "other"}}, toSvc)}, rr, cross("db"), false, gwapi.ReasonRefNotPermitted},
		{"a grant for another route kind does not help",
			fakeEnv{services: services, grants: grant("data", []gwapi.ReferenceGrantFrom{{Group: gwapi.GroupName, Kind: "UDPRoute", Namespace: "apps"}}, toSvc)}, rr, cross("db"), false, gwapi.ReasonRefNotPermitted},
		{"a grant in the route's own namespace does not authorise reaching into another",
			fakeEnv{services: services, grants: grant("apps", fromApps, toSvc)}, rr, cross("db"), false, gwapi.ReasonRefNotPermitted},
		{"a granted Service that does not exist", fakeEnv{services: services, grants: grant("data", fromApps, toSvc)}, rr, cross("ghost"), false, gwapi.ReasonBackendNotFound},
		{"a non-Service kind", fakeEnv{services: services}, rr, gwapi.BackendRef{Name: "x", Kind: sp("ConfigMap")}, false, gwapi.ReasonInvalidKind},
		{"a non-core group", fakeEnv{services: services}, rr, gwapi.BackendRef{Name: "local", Group: sp("example.com")}, false, gwapi.ReasonInvalidKind},
	}
	for _, c := range cases {
		got := CheckBackendRef(c.rr, c.br, c.env)
		if got.Resolved != c.ok || got.Reason != c.reason {
			t.Errorf("%s: resolved=%v reason=%q (%s), want resolved=%v reason=%q", c.name, got.Resolved, got.Reason, got.Message, c.ok, c.reason)
		}
	}
}
