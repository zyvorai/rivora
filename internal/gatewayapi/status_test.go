// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package gatewayapi

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	gwapi "github.com/zyvorai/rivora/api/gatewayapi"
)

func startStatus(t *testing.T, dyn *dynamicfake.FakeDynamicClient, objs ...runtime.Object) (*StatusReconciler, context.CancelFunc) {
	t.Helper()
	r, factory, dynFactory := NewStatusReconciler(fake.NewSimpleClientset(objs...), dyn, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	factory.Start(ctx.Done())
	dynFactory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(),
		dynFactory.ForResource(gwapi.GatewayClassResource).Informer().HasSynced,
		dynFactory.ForResource(gwapi.GatewayResource).Informer().HasSynced,
		dynFactory.ForResource(gwapi.TCPRouteResource).Informer().HasSynced,
		dynFactory.ForResource(gwapi.UDPRouteResource).Informer().HasSynced,
		dynFactory.ForResource(gwapi.ReferenceGrantResource).Informer().HasSynced,
		factory.Core().V1().Namespaces().Informer().HasSynced,
		factory.Core().V1().Services().Informer().HasSynced,
	) {
		cancel()
		t.Fatal("caches never synced")
	}
	return r, cancel
}

func routeStatus(t *testing.T, dyn *dynamicfake.FakeDynamicClient, ns, name string) gwapi.RouteStatus {
	t.Helper()
	u, err := dyn.Resource(gwapi.TCPRouteResource).Namespace(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var rt gwapi.TCPRoute
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.UnstructuredContent(), &rt); err != nil {
		t.Fatal(err)
	}
	return rt.Status
}

func condOf(conds []metav1.Condition, typ string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == typ {
			return &conds[i]
		}
	}
	return nil
}

func ownParent(t *testing.T, st gwapi.RouteStatus) gwapi.RouteParentStatus {
	t.Helper()
	for _, p := range st.Parents {
		if p.ControllerName == gwapi.ControllerName {
			return p
		}
	}
	t.Fatalf("no parents entry from %s in %+v", gwapi.ControllerName, st)
	return gwapi.RouteParentStatus{}
}

func TestStatusReportsAcceptedAndResolvedRefs(t *testing.T) {
	dyn := newDynamicClient()
	createObj(t, dyn, gwapi.GatewayClassResource, gatewayClassObj("rivora", gwapi.ControllerName))
	open := withAllowedRoutes(listener("open", "TCP", 80), map[string]interface{}{"namespaces": map[string]interface{}{"from": "All"}})
	closed := listener("closed", "TCP", 81)
	createObj(t, dyn, gwapi.GatewayResource, gatewayObj("infra", "gw", "rivora", []string{"10.0.0.100"}, open, closed))

	// good: from apps to the open listener, its Service exists in apps.
	good := routeIn("apps", "good", backendRef("svc", 8080))
	good.Object["spec"].(map[string]interface{})["parentRefs"] = []interface{}{map[string]interface{}{"name": "gw", "namespace": "infra", "sectionName": "open"}}
	createObj(t, dyn, gwapi.TCPRouteResource, good)
	// refused: from apps to the closed listener (Same namespace only).
	refused := routeIn("apps", "refused", backendRef("svc", 8080))
	refused.Object["spec"].(map[string]interface{})["parentRefs"] = []interface{}{map[string]interface{}{"name": "gw", "namespace": "infra", "sectionName": "closed"}}
	createObj(t, dyn, gwapi.TCPRouteResource, refused)
	// forbidden ref: attached, but its backendRef into "data" has no ReferenceGrant.
	ref := backendRef("db", 5432)
	ref["namespace"] = "data"
	forbidden := routeIn("apps", "forbidden", ref)
	forbidden.Object["spec"].(map[string]interface{})["parentRefs"] = []interface{}{map[string]interface{}{"name": "gw", "namespace": "infra", "sectionName": "open"}}
	createObj(t, dyn, gwapi.TCPRouteResource, forbidden)
	// missing Service.
	missing := routeIn("apps", "missing", backendRef("ghost", 1))
	missing.Object["spec"].(map[string]interface{})["parentRefs"] = []interface{}{map[string]interface{}{"name": "gw", "namespace": "infra", "sectionName": "open"}}
	createObj(t, dyn, gwapi.TCPRouteResource, missing)

	r, cancel := startStatus(t, dyn, svcObj("apps", "svc", 8080), svcObj("data", "db", 5432))
	defer cancel()
	if err := r.reconcile(context.Background(), "infra/gw"); err != nil {
		t.Fatal(err)
	}

	p := ownParent(t, routeStatus(t, dyn, "apps", "good"))
	if c := condOf(p.Conditions, gwapi.ConditionAccepted); c == nil || c.Status != metav1.ConditionTrue || c.Reason != gwapi.ReasonAccepted {
		t.Errorf("good: Accepted = %+v, want True/Accepted", c)
	}
	if c := condOf(p.Conditions, gwapi.ConditionResolvedRefs); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("good: ResolvedRefs = %+v, want True", c)
	}
	if p.ParentRef.Name != "gw" || p.ControllerName != gwapi.ControllerName {
		t.Errorf("good: parent entry = %+v", p)
	}

	p = ownParent(t, routeStatus(t, dyn, "apps", "refused"))
	if c := condOf(p.Conditions, gwapi.ConditionAccepted); c == nil || c.Status != metav1.ConditionFalse || c.Reason != gwapi.ReasonNotAllowedByListeners {
		t.Errorf("refused: Accepted = %+v, want False/NotAllowedByListeners", c)
	}

	p = ownParent(t, routeStatus(t, dyn, "apps", "forbidden"))
	if c := condOf(p.Conditions, gwapi.ConditionAccepted); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("forbidden: the route is still Accepted (it attached), got %+v", c)
	}
	if c := condOf(p.Conditions, gwapi.ConditionResolvedRefs); c == nil || c.Status != metav1.ConditionFalse || c.Reason != gwapi.ReasonRefNotPermitted {
		t.Errorf("forbidden: ResolvedRefs = %+v, want False/RefNotPermitted", c)
	}

	p = ownParent(t, routeStatus(t, dyn, "apps", "missing"))
	if c := condOf(p.Conditions, gwapi.ConditionResolvedRefs); c == nil || c.Status != metav1.ConditionFalse || c.Reason != gwapi.ReasonBackendNotFound {
		t.Errorf("missing: ResolvedRefs = %+v, want False/BackendNotFound", c)
	}

	// The Gateway's listeners report the routes they admitted: three attached to "open", none to "closed".
	u, err := dyn.Resource(gwapi.GatewayResource).Namespace("infra").Get(context.Background(), "gw", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var gw gwapi.Gateway
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.UnstructuredContent(), &gw); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int32{}
	for _, l := range gw.Status.Listeners {
		counts[l.Name] = l.AttachedRoutes
		if len(l.SupportedKinds) != 1 || l.SupportedKinds[0].Kind != "TCPRoute" {
			t.Errorf("listener %s supportedKinds = %+v, want [TCPRoute]", l.Name, l.SupportedKinds)
		}
		if c := condOf(l.Conditions, gwapi.ConditionAccepted); c == nil || c.Status != metav1.ConditionTrue {
			t.Errorf("listener %s Accepted = %+v", l.Name, c)
		}
	}
	if counts["open"] != 3 || counts["closed"] != 0 {
		t.Errorf("attachedRoutes = %v, want open=3 closed=0", counts)
	}
	if len(gw.Status.Addresses) != 1 {
		t.Errorf("writing listener status must not disturb the Gateway's addresses: %+v", gw.Status.Addresses)
	}
}

func TestStatusLeavesOtherControllersEntriesAlone(t *testing.T) {
	dyn := newDynamicClient()
	createObj(t, dyn, gwapi.GatewayClassResource, gatewayClassObj("rivora", gwapi.ControllerName))
	createObj(t, dyn, gwapi.GatewayResource, gatewayObj("ns", "gw", "rivora", []string{"10.0.0.100"}, listener("tcp", "TCP", 80)))
	rt := tcpRouteObj("ns", "route", "gw", backendRef("svc", 8080))
	// Another controller already reported on this route.
	rt.Object["status"] = map[string]interface{}{"parents": []interface{}{map[string]interface{}{
		"parentRef":      map[string]interface{}{"name": "other-gw"},
		"controllerName": "example.com/other-controller",
		"conditions": []interface{}{map[string]interface{}{
			"type": "Accepted", "status": "True", "reason": "Accepted", "message": "fine",
			"lastTransitionTime": "2026-01-01T00:00:00Z",
		}},
	}}}
	createObj(t, dyn, gwapi.TCPRouteResource, rt)

	r, cancel := startStatus(t, dyn, svcObj("ns", "svc", 8080))
	defer cancel()
	if err := r.reconcile(context.Background(), "ns/gw"); err != nil {
		t.Fatal(err)
	}
	st := routeStatus(t, dyn, "ns", "route")
	if len(st.Parents) != 2 {
		t.Fatalf("parents = %+v, want the other controller's entry kept plus ours", st.Parents)
	}
	var theirs *gwapi.RouteParentStatus
	for i := range st.Parents {
		if st.Parents[i].ControllerName == "example.com/other-controller" {
			theirs = &st.Parents[i]
		}
	}
	if theirs == nil || theirs.ParentRef.Name != "other-gw" || len(theirs.Conditions) != 1 || theirs.Conditions[0].Message != "fine" {
		t.Errorf("another controller's entry was altered: %+v", theirs)
	}
	ownParent(t, st)
}

func TestStatusIsQuietWhenNothingChanged(t *testing.T) {
	dyn := newDynamicClient()
	createObj(t, dyn, gwapi.GatewayClassResource, gatewayClassObj("rivora", gwapi.ControllerName))
	createObj(t, dyn, gwapi.GatewayResource, gatewayObj("ns", "gw", "rivora", []string{"10.0.0.100"}, listener("tcp", "TCP", 80)))
	createObj(t, dyn, gwapi.TCPRouteResource, tcpRouteObj("ns", "route", "gw", backendRef("svc", 8080)))
	r, cancel := startStatus(t, dyn, svcObj("ns", "svc", 8080))
	defer cancel()

	if err := r.reconcile(context.Background(), "ns/gw"); err != nil {
		t.Fatal(err)
	}
	updates := func() int {
		n := 0
		for _, a := range dyn.Actions() {
			if a.GetVerb() == "update" && a.GetSubresource() == "status" {
				n++
			}
		}
		return n
	}
	first := updates()
	if first == 0 {
		t.Fatal("the first reconcile wrote no status")
	}
	// The informer cache must see our write before the second pass, or it would diff against the old object.
	waitUntil(t, func() bool {
		u, err := r.tcpLister.ByNamespace("ns").Get("route")
		if err != nil {
			return false
		}
		if _, found, _ := unstructured.NestedSlice(u.(*unstructured.Unstructured).Object, "status", "parents"); !found {
			return false
		}
		g, err := r.gatewayLister.ByNamespace("ns").Get("gw")
		if err != nil {
			return false
		}
		_, found, _ := unstructured.NestedSlice(g.(*unstructured.Unstructured).Object, "status", "listeners")
		return found
	})
	if err := r.reconcile(context.Background(), "ns/gw"); err != nil {
		t.Fatal(err)
	}
	if got := updates(); got != first {
		t.Errorf("a second reconcile with nothing changed wrote %d more status update(s)", got-first)
	}
}

func TestStatusIgnoresUnmanagedGateways(t *testing.T) {
	dyn := newDynamicClient()
	createObj(t, dyn, gwapi.GatewayClassResource, gatewayClassObj("theirs", "example.com/other-controller"))
	createObj(t, dyn, gwapi.GatewayResource, gatewayObj("ns", "gw", "theirs", []string{"10.0.0.100"}, listener("tcp", "TCP", 80)))
	createObj(t, dyn, gwapi.TCPRouteResource, tcpRouteObj("ns", "route", "gw", backendRef("svc", 8080)))
	r, cancel := startStatus(t, dyn, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "svc"}})
	defer cancel()
	if err := r.reconcile(context.Background(), "ns/gw"); err != nil {
		t.Fatal(err)
	}
	for _, a := range dyn.Actions() {
		if a.GetVerb() == "update" {
			t.Errorf("a Gateway of another controller's class was written to: %v", a)
		}
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		sleepMS(25)
	}
	t.Fatal("timed out waiting for the informer cache")
}

func sleepMS(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }

// With several failures, ResolvedRefs reports the most serious: a forbidden reference over a wrong
// kind over a missing Service.
func TestResolvedRefsReportsTheMostSeriousFailure(t *testing.T) {
	forbidden := backendRef("db", 5432)
	forbidden["namespace"] = "data"
	wrongKind := backendRef("cfg", 1)
	wrongKind["kind"] = "ConfigMap"
	missing := backendRef("ghost", 1)

	cases := []struct {
		name string
		refs []map[string]interface{}
		want string
	}{
		{"forbidden beats missing", []map[string]interface{}{missing, forbidden}, gwapi.ReasonRefNotPermitted},
		{"forbidden beats wrong kind", []map[string]interface{}{wrongKind, forbidden}, gwapi.ReasonRefNotPermitted},
		{"wrong kind beats missing", []map[string]interface{}{missing, wrongKind}, gwapi.ReasonInvalidKind},
		{"one good ref does not hide a bad one", []map[string]interface{}{backendRef("svc", 8080), missing}, gwapi.ReasonBackendNotFound},
	}
	for _, c := range cases {
		dyn := newDynamicClient()
		createObj(t, dyn, gwapi.GatewayClassResource, gatewayClassObj("rivora", gwapi.ControllerName))
		createObj(t, dyn, gwapi.GatewayResource, gatewayObj("ns", "gw", "rivora", []string{"10.0.0.100"}, listener("tcp", "TCP", 80)))
		createObj(t, dyn, gwapi.TCPRouteResource, tcpRouteObj("ns", "route", "gw", c.refs...))
		r, cancel := startStatus(t, dyn, svcObj("ns", "svc", 8080), svcObj("data", "db", 5432))
		if err := r.reconcile(context.Background(), "ns/gw"); err != nil {
			cancel()
			t.Fatal(err)
		}
		p := ownParent(t, routeStatus(t, dyn, "ns", "route"))
		if cond := condOf(p.Conditions, gwapi.ConditionResolvedRefs); cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != c.want {
			t.Errorf("%s: ResolvedRefs = %+v, want False/%s", c.name, cond, c.want)
		}
		cancel()
	}
}
