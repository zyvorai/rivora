// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package ipamctrl

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/dynamicinformer"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	gwapi "github.com/zyvorai/rivora/api/gatewayapi"
	"github.com/zyvorai/rivora/api/v1alpha1"
)

// newGatewayDynamicClient builds an empty fake dynamic client with
// AddressPool + the Gateway API GVRs registered. Objects are added via
// createGatewayObj (not constructor args) — see internal/gatewayapi's
// identically-named/reasoned helper for why: passing namespaced
// unstructured objects through NewSimpleDynamicClientWithCustomListKinds's
// objs... silently fails to register them with a bare runtime.NewScheme().
func newGatewayDynamicClient() *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	register := func(gvr schema.GroupVersionResource, kind, listKind string) {
		scheme.AddKnownTypeWithName(gvr.GroupVersion().WithKind(kind), &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(gvr.GroupVersion().WithKind(listKind), &unstructured.UnstructuredList{})
	}
	register(v1alpha1.AddressPoolResource, "AddressPool", "AddressPoolList")
	register(gwapi.GatewayClassResource, "GatewayClass", "GatewayClassList")
	register(gwapi.GatewayResource, "Gateway", "GatewayList")

	gvrToListKind := map[schema.GroupVersionResource]string{
		v1alpha1.AddressPoolResource: "AddressPoolList",
		gwapi.GatewayClassResource:   "GatewayClassList",
		gwapi.GatewayResource:        "GatewayList",
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind)
}

func createGatewayObj(t *testing.T, dyn *dynamicfake.FakeDynamicClient, gvr schema.GroupVersionResource, obj *unstructured.Unstructured) {
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

func gatewayObj(namespace, name, className string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "Gateway",
		"metadata":   map[string]interface{}{"namespace": namespace, "name": name},
		"spec": map[string]interface{}{
			"gatewayClassName": className,
			"listeners": []interface{}{
				map[string]interface{}{"name": "tcp-1", "protocol": "TCP", "port": int64(80)},
			},
		},
	}}
}

func startSyncedWithGatewayAPI(t *testing.T, dyn *dynamicfake.FakeDynamicClient, objs ...runtime.Object) (*Reconciler, dynamicinformer.DynamicSharedInformerFactory, context.CancelFunc) {
	t.Helper()
	clientset := fake.NewSimpleClientset(objs...)
	r, factory, dynFactory := New(clientset, dyn, "", true, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	factory.Start(ctx.Done())
	dynFactory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(),
		factory.Core().V1().Services().Informer().HasSynced,
		dynFactory.ForResource(v1alpha1.AddressPoolResource).Informer().HasSynced,
		dynFactory.ForResource(gwapi.GatewayClassResource).Informer().HasSynced,
		dynFactory.ForResource(gwapi.GatewayResource).Informer().HasSynced,
	) {
		cancel()
		t.Fatal("caches never synced")
	}
	return r, dynFactory, cancel
}

func TestReconcileGatewayAllocatesAndPatchesStatus(t *testing.T) {
	dyn := newGatewayDynamicClient()
	createGatewayObj(t, dyn, v1alpha1.AddressPoolResource, addressPoolObj("default", []string{"10.0.0.0/30"}, true))
	createGatewayObj(t, dyn, gwapi.GatewayClassResource, gatewayClassObj("rivora", gwapi.ControllerName))
	createGatewayObj(t, dyn, gwapi.GatewayResource, gatewayObj("ns", "gw", "rivora"))

	r, dynFactory, cancel := startSyncedWithGatewayAPI(t, dyn)
	defer cancel()
	ctx := context.Background()

	if err := r.reconcilePools(ctx, dynFactory); err != nil {
		t.Fatal(err)
	}
	if avail, assigned := r.allocator.Counts("default"); avail == 0 && assigned == 0 {
		t.Fatalf("pool 'default' did not load: avail=%d assigned=%d", avail, assigned)
	}

	// First reconcile: adds the finalizer.
	if err := r.reconcileGateway(ctx, "ns/gw"); err != nil {
		t.Fatal(err)
	}
	// Second reconcile: allocates and patches status.
	if err := r.reconcileGateway(ctx, "ns/gw"); err != nil {
		t.Fatal(err)
	}

	obj, err := dyn.Resource(gwapi.GatewayResource).Namespace("ns").Get(ctx, "gw", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var gw gwapi.Gateway
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.UnstructuredContent(), &gw); err != nil {
		t.Fatal(err)
	}
	if !containsString(gw.Finalizers, Finalizer) {
		t.Fatalf("expected finalizer added, got %+v", gw.Finalizers)
	}
	if ip := gatewayAssignedIPv4(&gw); ip == "" {
		t.Error("expected an assigned address after the second reconcile")
	}
	if len(gw.Status.Conditions) == 0 {
		t.Error("expected at least one status condition written")
	}
}

func TestReconcileGatewayUnmanagedClassIsNoop(t *testing.T) {
	dyn := newGatewayDynamicClient()
	createGatewayObj(t, dyn, gwapi.GatewayClassResource, gatewayClassObj("other", "example.com/other-controller"))
	createGatewayObj(t, dyn, gwapi.GatewayResource, gatewayObj("ns", "gw", "other"))

	r, _, cancel := startSyncedWithGatewayAPI(t, dyn)
	defer cancel()
	ctx := context.Background()

	if err := r.reconcileGateway(ctx, "ns/gw"); err != nil {
		t.Fatal(err)
	}
	obj, err := dyn.Resource(gwapi.GatewayResource).Namespace("ns").Get(ctx, "gw", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var gw gwapi.Gateway
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.UnstructuredContent(), &gw); err != nil {
		t.Fatal(err)
	}
	if len(gw.Finalizers) != 0 {
		t.Errorf("expected no finalizer added for an unmanaged class, got %+v", gw.Finalizers)
	}
}

func TestReconcileGatewayReleasesOnDelete(t *testing.T) {
	dyn := newGatewayDynamicClient()
	createGatewayObj(t, dyn, v1alpha1.AddressPoolResource, addressPoolObj("default", []string{"10.0.0.0/30"}, true))
	createGatewayObj(t, dyn, gwapi.GatewayClassResource, gatewayClassObj("rivora", gwapi.ControllerName))
	createGatewayObj(t, dyn, gwapi.GatewayResource, gatewayObj("ns", "gw", "rivora"))

	r, dynFactory, cancel := startSyncedWithGatewayAPI(t, dyn)
	defer cancel()
	ctx := context.Background()

	if err := r.reconcilePools(ctx, dynFactory); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileGateway(ctx, "ns/gw"); err != nil { // add finalizer
		t.Fatal(err)
	}
	if err := r.reconcileGateway(ctx, "ns/gw"); err != nil { // allocate
		t.Fatal(err)
	}
	if avail, _ := r.allocator.Counts("default"); avail != 3 {
		t.Fatalf("expected 1 address assigned (3 available of 4), got avail=%d", avail)
	}

	if err := dyn.Resource(gwapi.GatewayResource).Namespace("ns").Delete(ctx, "gw", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileGateway(ctx, "ns/gw"); err != nil {
		t.Fatal(err)
	}
	if avail, _ := r.allocator.Counts("default"); avail != 4 {
		t.Errorf("expected the address released back to the pool, avail=%d, want 4", avail)
	}
}
