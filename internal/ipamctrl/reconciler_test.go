// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package ipamctrl

import (
	"context"
	"io"
	"log/slog"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/dynamicinformer"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	"github.com/zyvorai/rivora/api/v1alpha1"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func addressPoolObj(name string, addresses []string, autoAssign bool) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rivora.zyvor.dev/v1alpha1",
		"kind":       "AddressPool",
		"metadata":   map[string]interface{}{"name": name},
		"spec": map[string]interface{}{
			"addresses":  toInterfaceSlice(addresses),
			"autoAssign": autoAssign,
		},
	}}
}

func toInterfaceSlice(ss []string) []interface{} {
	out := make([]interface{}, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func newDynamicClient(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	gvrToListKind := map[schema.GroupVersionResource]string{
		v1alpha1.AddressPoolResource: "AddressPoolList",
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind, objs...)
}

func lbService(name string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name},
		Spec: corev1.ServiceSpec{
			Type:  corev1.ServiceTypeLoadBalancer,
			Ports: []corev1.ServicePort{{Name: "http", Port: 80}},
		},
	}
}

// startSynced builds a Reconciler against fake clientsets, starts both
// informer factories, and waits for their caches to sync — the same setup
// Run() does, minus actually calling Run (tests drive reconcile methods
// directly for determinism instead of racing the async workqueue).
func startSynced(t *testing.T, dyn *dynamicfake.FakeDynamicClient, objs ...runtime.Object) (*Reconciler, informers.SharedInformerFactory, dynamicinformer.DynamicSharedInformerFactory, context.CancelFunc) {
	t.Helper()
	clientset := fake.NewSimpleClientset(objs...)
	r, factory, dynFactory := New(clientset, dyn, "", testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	factory.Start(ctx.Done())
	dynFactory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(),
		factory.Core().V1().Services().Informer().HasSynced,
		dynFactory.ForResource(v1alpha1.AddressPoolResource).Informer().HasSynced,
	) {
		cancel()
		t.Fatal("caches never synced")
	}
	return r, factory, dynFactory, cancel
}

func TestReconcilePoolsPopulatesAllocator(t *testing.T) {
	dyn := newDynamicClient(addressPoolObj("default", []string{"10.0.0.0/30"}, true))
	r, _, dynFactory, cancel := startSynced(t, dyn)
	defer cancel()

	if err := r.reconcilePools(context.Background(), dynFactory); err != nil {
		t.Fatal(err)
	}

	avail, assigned := r.allocator.Counts("default")
	if avail != 4 || assigned != 0 {
		t.Errorf("Counts = (%d, %d), want (4, 0)", avail, assigned)
	}
}

func TestReconcileServiceAddsFinalizerThenAllocates(t *testing.T) {
	dyn := newDynamicClient(addressPoolObj("default", []string{"10.0.0.0/30"}, true))
	svc := lbService("svc")
	r, _, dynFactory, cancel := startSynced(t, dyn, svc)
	defer cancel()
	ctx := context.Background()

	if err := r.reconcilePools(ctx, dynFactory); err != nil {
		t.Fatal(err)
	}

	// First reconcile: adds the finalizer, doesn't allocate yet.
	if err := r.reconcileService(ctx, "ns/svc"); err != nil {
		t.Fatal(err)
	}
	got, err := r.clientset.CoreV1().Services("ns").Get(ctx, "svc", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(got.Finalizers, Finalizer) {
		t.Fatalf("expected finalizer added, got %+v", got.Finalizers)
	}

	// Second reconcile: now allocates and patches status.
	if err := r.reconcileService(ctx, "ns/svc"); err != nil {
		t.Fatal(err)
	}
	got, err = r.clientset.CoreV1().Services("ns").Get(ctx, "svc", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ip := ingressIPv4(got); ip == "" {
		t.Error("expected an assigned ingress IP after the second reconcile")
	}
}

func TestReconcileServiceDeletionReleasesAndRemovesFinalizer(t *testing.T) {
	dyn := newDynamicClient(addressPoolObj("default", []string{"10.0.0.0/30"}, true))
	svc := lbService("svc")
	svc.Finalizers = []string{Finalizer}
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "10.0.0.1"}}
	now := metav1.Now()
	svc.DeletionTimestamp = &now

	r, _, dynFactory, cancel := startSynced(t, dyn, svc)
	defer cancel()
	ctx := context.Background()
	if err := r.reconcilePools(ctx, dynFactory); err != nil {
		t.Fatal(err)
	}
	if err := r.allocator.Reserve("10.0.0.1", "ns/svc"); err != nil {
		t.Fatal(err)
	}

	if err := r.reconcileService(ctx, "ns/svc"); err != nil {
		t.Fatal(err)
	}
	got, err := r.clientset.CoreV1().Services("ns").Get(ctx, "svc", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if containsString(got.Finalizers, Finalizer) {
		t.Errorf("expected finalizer removed on deletion, got %+v", got.Finalizers)
	}
	if avail, _ := r.allocator.Counts("default"); avail != 4 {
		t.Errorf("expected the address released back to the pool, Counts avail=%d, want 4", avail)
	}
}

func TestReconcileServiceNonManagedIsNoop(t *testing.T) {
	dyn := newDynamicClient()
	svc := lbService("svc")
	svc.Spec.Type = corev1.ServiceTypeClusterIP
	r, _, _, cancel := startSynced(t, dyn, svc)
	defer cancel()

	if err := r.reconcileService(context.Background(), "ns/svc"); err != nil {
		t.Fatal(err)
	}
	got, err := r.clientset.CoreV1().Services("ns").Get(context.Background(), "svc", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Finalizers) != 0 {
		t.Errorf("expected no finalizer added to a non-managed Service, got %+v", got.Finalizers)
	}
}
