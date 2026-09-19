// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package gatewayapi

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/dynamicinformer"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	gwapi "github.com/zyvorai/rivora/api/gatewayapi"
	"github.com/zyvorai/rivora/internal/config"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func ptr[T any](v T) *T { return &v }

type fakeDataplane struct {
	mu       sync.Mutex
	upserts  []config.VIP
	removes  []string
	draining map[string]bool
}

func newFakeDataplane() *fakeDataplane { return &fakeDataplane{draining: map[string]bool{}} }

func (f *fakeDataplane) UpsertVIP(vip config.VIP) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upserts = append(f.upserts, vip)
	return nil
}

func (f *fakeDataplane) RemoveVIP(key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removes = append(f.removes, key)
	return nil
}

func (f *fakeDataplane) SetBackendDrainingByKey(vipKey, backendKey string, draining bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.draining[vipKey+"/"+backendKey] = draining
	return nil
}

// newDynamicClient builds an empty fake dynamic client with the 4 Gateway
// API GVRs registered. Objects are added afterward via createObj, not
// passed as constructor args — NewSimpleDynamicClientWithCustomListKinds's
// variadic objs... silently fails to register *namespaced* unstructured
// objects with a bare runtime.NewScheme() (confirmed empirically: a
// GatewayClass — cluster-scoped, no precedent for the failure — comes
// through fine via the constructor; a namespaced Gateway does not, but
// the exact same object added via .Create() after construction works).
func newDynamicClient() *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	register := func(gvr schema.GroupVersionResource, kind, listKind string) {
		scheme.AddKnownTypeWithName(gvr.GroupVersion().WithKind(kind), &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(gvr.GroupVersion().WithKind(listKind), &unstructured.UnstructuredList{})
	}
	register(gwapi.GatewayClassResource, "GatewayClass", "GatewayClassList")
	register(gwapi.GatewayResource, "Gateway", "GatewayList")
	register(gwapi.TCPRouteResource, "TCPRoute", "TCPRouteList")
	register(gwapi.UDPRouteResource, "UDPRoute", "UDPRouteList")
	register(gwapi.ReferenceGrantResource, "ReferenceGrant", "ReferenceGrantList")

	gvrToListKind := map[schema.GroupVersionResource]string{
		gwapi.GatewayClassResource:   "GatewayClassList",
		gwapi.GatewayResource:        "GatewayList",
		gwapi.TCPRouteResource:       "TCPRouteList",
		gwapi.UDPRouteResource:       "UDPRouteList",
		gwapi.ReferenceGrantResource: "ReferenceGrantList",
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind)
}

// createObj adds obj (cluster- or namespace-scoped, inferred from
// obj.GetNamespace()) to dyn under gvr.
func createObj(t *testing.T, dyn *dynamicfake.FakeDynamicClient, gvr schema.GroupVersionResource, obj *unstructured.Unstructured) {
	t.Helper()
	ctx := context.Background()
	var err error
	if ns := obj.GetNamespace(); ns != "" {
		_, err = dyn.Resource(gvr).Namespace(ns).Create(ctx, obj, metav1.CreateOptions{})
	} else {
		_, err = dyn.Resource(gvr).Create(ctx, obj, metav1.CreateOptions{})
	}
	if err != nil {
		t.Fatalf("create %s %s/%s: %v", gvr.Resource, obj.GetNamespace(), obj.GetName(), err)
	}
}

func gatewayClassObj(name, controllerName string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "GatewayClass",
		"metadata":   map[string]interface{}{"name": name},
		"spec":       map[string]interface{}{"controllerName": controllerName},
	}}
}

func gatewayObj(namespace, name, className string, addresses []string, listeners ...map[string]interface{}) *unstructured.Unstructured {
	addrs := make([]interface{}, len(addresses))
	for i, a := range addresses {
		addrs[i] = map[string]interface{}{"type": "IPAddress", "value": a}
	}
	ls := make([]interface{}, len(listeners))
	for i, l := range listeners {
		ls[i] = l
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "Gateway",
		"metadata":   map[string]interface{}{"namespace": namespace, "name": name},
		"spec": map[string]interface{}{
			"gatewayClassName": className,
			"listeners":        ls,
		},
		"status": map[string]interface{}{"addresses": addrs},
	}}
}

func listener(name, protocol string, port int64) map[string]interface{} {
	return map[string]interface{}{"name": name, "protocol": protocol, "port": port}
}

func tcpRouteObj(namespace, name, parentName string, backendRefs ...map[string]interface{}) *unstructured.Unstructured {
	refs := make([]interface{}, len(backendRefs))
	for i, r := range backendRefs {
		refs[i] = r
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "gateway.networking.k8s.io/v1alpha2",
		"kind":       "TCPRoute",
		"metadata":   map[string]interface{}{"namespace": namespace, "name": name},
		"spec": map[string]interface{}{
			"parentRefs": []interface{}{
				map[string]interface{}{"name": parentName},
			},
			"rules": []interface{}{
				map[string]interface{}{"backendRefs": refs},
			},
		},
	}}
}

func backendRef(name string, port int64) map[string]interface{} {
	return map[string]interface{}{"name": name, "port": port}
}

func weightedBackendRef(name string, port, weight int64) map[string]interface{} {
	return map[string]interface{}{"name": name, "port": port, "weight": weight}
}

func svcObj(namespace, name string, port int32) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: port}}},
	}
}

func sliceObj(namespace, svcName string, port int32, endpoints ...discoveryv1.Endpoint) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      svcName + "-abcde",
			Labels:    map[string]string{serviceNameLabel: svcName},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Port: ptr(port)}},
		Endpoints:   endpoints,
	}
}

func startSynced(t *testing.T, dyn *dynamicfake.FakeDynamicClient, plane dataplaner, objs ...runtime.Object) (*Reconciler, dynamicinformer.DynamicSharedInformerFactory, context.CancelFunc) {
	t.Helper()
	clientset := fake.NewSimpleClientset(objs...)
	r, factory, dynFactory := New(clientset, dyn, plane, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	factory.Start(ctx.Done())
	dynFactory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(),
		factory.Core().V1().Services().Informer().HasSynced,
		factory.Discovery().V1().EndpointSlices().Informer().HasSynced,
		dynFactory.ForResource(gwapi.GatewayClassResource).Informer().HasSynced,
		dynFactory.ForResource(gwapi.GatewayResource).Informer().HasSynced,
		dynFactory.ForResource(gwapi.TCPRouteResource).Informer().HasSynced,
		dynFactory.ForResource(gwapi.UDPRouteResource).Informer().HasSynced,
		factory.Core().V1().Namespaces().Informer().HasSynced,
		dynFactory.ForResource(gwapi.ReferenceGrantResource).Informer().HasSynced,
	) {
		cancel()
		t.Fatal("caches never synced")
	}
	return r, dynFactory, cancel
}

func TestReconcilerCreatesVIPFromGatewayAndTCPRoute(t *testing.T) {
	dyn := newDynamicClient()
	createObj(t, dyn, gwapi.GatewayClassResource, gatewayClassObj("rivora", gwapi.ControllerName))
	createObj(t, dyn, gwapi.GatewayResource, gatewayObj("ns", "gw", "rivora", []string{"10.0.0.100"}, listener("tcp-1", "TCP", 80)))
	createObj(t, dyn, gwapi.TCPRouteResource, tcpRouteObj("ns", "route", "gw", backendRef("svc", 8080)))

	svc := svcObj("ns", "svc", 8080)
	sl := sliceObj("ns", "svc", 8080, discoveryv1.Endpoint{
		Addresses:  []string{"10.1.0.1"},
		Conditions: discoveryv1.EndpointConditions{Ready: ptr(true), Serving: ptr(true)},
	})

	plane := newFakeDataplane()
	r, _, cancel := startSynced(t, dyn, plane, svc, sl)
	defer cancel()

	if err := r.reconcile("ns/gw"); err != nil {
		t.Fatal(err)
	}

	plane.mu.Lock()
	defer plane.mu.Unlock()
	if len(plane.upserts) != 1 {
		t.Fatalf("got %d upserts, want 1: %+v", len(plane.upserts), plane.upserts)
	}
	vip := plane.upserts[0]
	if vip.Address != "10.0.0.100" || vip.Port != 80 || vip.Protocol != config.ProtoTCP || vip.Mode != config.ModeNAT {
		t.Errorf("unexpected VIP: %+v", vip)
	}
	if len(vip.Backends) != 1 || vip.Backends[0].Address != "10.1.0.1" || vip.Backends[0].Port != 8080 {
		t.Errorf("unexpected backends: %+v", vip.Backends)
	}
}

func TestReconcilerIgnoresGatewayWithUnmanagedClass(t *testing.T) {
	dyn := newDynamicClient()
	createObj(t, dyn, gwapi.GatewayClassResource, gatewayClassObj("other", "example.com/other-controller"))
	createObj(t, dyn, gwapi.GatewayResource, gatewayObj("ns", "gw", "other", []string{"10.0.0.100"}, listener("tcp-1", "TCP", 80)))

	plane := newFakeDataplane()
	r, _, cancel := startSynced(t, dyn, plane)
	defer cancel()

	if err := r.reconcile("ns/gw"); err != nil {
		t.Fatal(err)
	}
	plane.mu.Lock()
	defer plane.mu.Unlock()
	if len(plane.upserts) != 0 {
		t.Errorf("expected no VIP for a Gateway under an unmanaged class, got %+v", plane.upserts)
	}
}

func TestReconcilerNoAddressYieldsNoVIP(t *testing.T) {
	dyn := newDynamicClient()
	createObj(t, dyn, gwapi.GatewayClassResource, gatewayClassObj("rivora", gwapi.ControllerName))
	createObj(t, dyn, gwapi.GatewayResource, gatewayObj("ns", "gw", "rivora", nil, listener("tcp-1", "TCP", 80))) // no status.addresses yet

	plane := newFakeDataplane()
	r, _, cancel := startSynced(t, dyn, plane)
	defer cancel()

	if err := r.reconcile("ns/gw"); err != nil {
		t.Fatal(err)
	}
	plane.mu.Lock()
	defer plane.mu.Unlock()
	if len(plane.upserts) != 0 {
		t.Errorf("expected no VIP before IPAM assigns an address, got %+v", plane.upserts)
	}
}

func TestReconcilerGatewayDeletedRemovesInstalledVIPs(t *testing.T) {
	dyn := newDynamicClient()
	plane := newFakeDataplane()
	r, _, cancel := startSynced(t, dyn, plane)
	defer cancel()
	r.installed["ns/gw"] = []string{"10.0.0.100:80:tcp"}

	if err := r.reconcile("ns/gw"); err != nil {
		t.Fatal(err)
	}
	plane.mu.Lock()
	defer plane.mu.Unlock()
	if len(plane.removes) != 1 || plane.removes[0] != "10.0.0.100:80:tcp" {
		t.Errorf("expected the stale VIP removed, got %+v", plane.removes)
	}
}

func TestReconcilerWeightedBackendRefsSplitAcrossServices(t *testing.T) {
	dyn := newDynamicClient()
	createObj(t, dyn, gwapi.GatewayClassResource, gatewayClassObj("rivora", gwapi.ControllerName))
	createObj(t, dyn, gwapi.GatewayResource, gatewayObj("ns", "gw", "rivora", []string{"10.0.0.100"}, listener("tcp-1", "TCP", 80)))
	createObj(t, dyn, gwapi.TCPRouteResource, tcpRouteObj("ns", "route", "gw",
		weightedBackendRef("stable", 8080, 9),
		weightedBackendRef("canary", 8080, 1),
	))

	stableSvc := svcObj("ns", "stable", 8080)
	canarySvc := svcObj("ns", "canary", 8080)
	stableSlice := sliceObj("ns", "stable", 8080, discoveryv1.Endpoint{
		Addresses:  []string{"10.1.0.1"},
		Conditions: discoveryv1.EndpointConditions{Ready: ptr(true), Serving: ptr(true)},
	})
	canarySlice := sliceObj("ns", "canary", 8080, discoveryv1.Endpoint{
		Addresses:  []string{"10.1.0.2"},
		Conditions: discoveryv1.EndpointConditions{Ready: ptr(true), Serving: ptr(true)},
	})

	plane := newFakeDataplane()
	r, _, cancel := startSynced(t, dyn, plane, stableSvc, canarySvc, stableSlice, canarySlice)
	defer cancel()

	if err := r.reconcile("ns/gw"); err != nil {
		t.Fatal(err)
	}

	plane.mu.Lock()
	defer plane.mu.Unlock()
	if len(plane.upserts) != 1 || len(plane.upserts[0].Backends) != 2 {
		t.Fatalf("expected one VIP with both backends, got %+v", plane.upserts)
	}
	weights := map[string]uint32{}
	for _, b := range plane.upserts[0].Backends {
		weights[b.Address] = b.Weight
	}
	if weights["10.1.0.1"] != 9 || weights["10.1.0.2"] != 1 {
		t.Errorf("expected weights 9/1 for stable/canary, got %+v", weights)
	}
}

func TestReconcilerCrossNamespaceBackendRefSkipped(t *testing.T) {
	dyn := newDynamicClient()
	createObj(t, dyn, gwapi.GatewayClassResource, gatewayClassObj("rivora", gwapi.ControllerName))
	createObj(t, dyn, gwapi.GatewayResource, gatewayObj("ns", "gw", "rivora", []string{"10.0.0.100"}, listener("tcp-1", "TCP", 80)))
	ref := backendRef("svc", 8080)
	ref["namespace"] = "other-ns"
	createObj(t, dyn, gwapi.TCPRouteResource, tcpRouteObj("ns", "route", "gw", ref))

	plane := newFakeDataplane()
	r, _, cancel := startSynced(t, dyn, plane)
	defer cancel()

	if err := r.reconcile("ns/gw"); err != nil {
		t.Fatal(err)
	}
	plane.mu.Lock()
	defer plane.mu.Unlock()
	if len(plane.upserts) != 0 {
		t.Errorf("expected a cross-namespace backendRef to be skipped (v1 scope), got %+v", plane.upserts)
	}
}
