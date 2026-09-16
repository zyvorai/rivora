// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package controller

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	"github.com/zyvorai/rivora/internal/config"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type fakeDataplane struct {
	mu       sync.Mutex
	upserts  []config.VIP
	removes  []string
	draining map[string]bool // "vipKey/backendKey" -> draining
}

func newFakeDataplane() *fakeDataplane {
	return &fakeDataplane{draining: map[string]bool{}}
}

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

func startSyncedReconciler(t *testing.T, plane dataplaner, objs ...runtime.Object) (*Reconciler, func()) {
	t.Helper()
	clientset := fake.NewSimpleClientset(objs...)
	r, factory := New(clientset, plane, "", testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(),
		factory.Core().V1().Services().Informer().HasSynced,
		factory.Discovery().V1().EndpointSlices().Informer().HasSynced,
	) {
		cancel()
		t.Fatal("caches never synced")
	}
	return r, cancel
}

func TestReconcilerCreatesVIPFromServiceAndEndpointSlice(t *testing.T) {
	svc := lbService(corev1.ServicePort{Name: "http", Port: 80})
	sl := slice("http", 8080, discoveryv1.Endpoint{
		Addresses:  []string{"10.1.0.1"},
		Conditions: discoveryv1.EndpointConditions{Ready: ptr(true), Serving: ptr(true)},
	})
	plane := newFakeDataplane()
	r, cancel := startSyncedReconciler(t, plane, svc, sl)
	defer cancel()

	if err := r.reconcile("ns/svc"); err != nil {
		t.Fatal(err)
	}

	plane.mu.Lock()
	defer plane.mu.Unlock()
	if len(plane.upserts) != 1 {
		t.Fatalf("got %d upserts, want 1: %+v", len(plane.upserts), plane.upserts)
	}
	if plane.upserts[0].Address != "10.0.0.5" || plane.upserts[0].Port != 80 {
		t.Errorf("unexpected VIP upserted: %+v", plane.upserts[0])
	}
	if got := r.installed["ns/svc"]; len(got) != 1 {
		t.Errorf("installed map not updated: %+v", r.installed)
	}
}

func TestReconcilerServiceGoneRemovesInstalled(t *testing.T) {
	plane := newFakeDataplane()
	r, cancel := startSyncedReconciler(t, plane) // no objects: Service lookup will 404
	defer cancel()
	r.installed["ns/svc"] = []string{"10.0.0.5:80:tcp"}

	if err := r.reconcile("ns/svc"); err != nil {
		t.Fatal(err)
	}

	plane.mu.Lock()
	defer plane.mu.Unlock()
	if len(plane.removes) != 1 || plane.removes[0] != "10.0.0.5:80:tcp" {
		t.Errorf("expected the stale VIP removed, got %+v", plane.removes)
	}
	if _, ok := r.installed["ns/svc"]; ok {
		t.Error("expected installed entry cleared after teardown")
	}
}

func TestReconcilerNonLoadBalancerServiceTearsDownPreviouslyInstalled(t *testing.T) {
	svc := lbService(corev1.ServicePort{Name: "http", Port: 80})
	svc.Spec.Type = corev1.ServiceTypeClusterIP // no longer a LoadBalancer Service
	plane := newFakeDataplane()
	r, cancel := startSyncedReconciler(t, plane, svc)
	defer cancel()
	r.installed["ns/svc"] = []string{"10.0.0.5:80:tcp"}

	if err := r.reconcile("ns/svc"); err != nil {
		t.Fatal(err)
	}
	plane.mu.Lock()
	defer plane.mu.Unlock()
	if len(plane.removes) != 1 {
		t.Errorf("expected removal when a Service stops being type=LoadBalancer, got %+v", plane.removes)
	}
}

func TestReconcilerDrainingBackendAppliedAfterUpsert(t *testing.T) {
	svc := lbService(corev1.ServicePort{Name: "http", Port: 80})
	sl := slice("http", 8080, discoveryv1.Endpoint{
		Addresses:  []string{"10.1.0.1"},
		Conditions: discoveryv1.EndpointConditions{Ready: ptr(false), Serving: ptr(true), Terminating: ptr(true)},
	})
	plane := newFakeDataplane()
	r, cancel := startSyncedReconciler(t, plane, svc, sl)
	defer cancel()

	if err := r.reconcile("ns/svc"); err != nil {
		t.Fatal(err)
	}

	plane.mu.Lock()
	defer plane.mu.Unlock()
	if !plane.draining["10.0.0.5:80:tcp/10.1.0.1:8080"] {
		t.Errorf("expected the terminating backend marked draining, got %+v", plane.draining)
	}
}

func TestReconcilerRemovesStaleVIPNoLongerDesired(t *testing.T) {
	// A Service that used to have port 80 programmed, but no longer has
	// any backends for it, should have that VIP removed.
	svc := lbService(corev1.ServicePort{Name: "http", Port: 80})
	plane := newFakeDataplane()
	r, cancel := startSyncedReconciler(t, plane, svc) // no EndpointSlice: zero backends
	defer cancel()
	r.installed["ns/svc"] = []string{"10.0.0.5:80:tcp"}

	if err := r.reconcile("ns/svc"); err != nil {
		t.Fatal(err)
	}
	plane.mu.Lock()
	defer plane.mu.Unlock()
	if len(plane.upserts) != 0 {
		t.Errorf("expected no upsert with zero backends, got %+v", plane.upserts)
	}
	if len(plane.removes) != 1 || plane.removes[0] != "10.0.0.5:80:tcp" {
		t.Errorf("expected the now-undesired VIP removed, got %+v", plane.removes)
	}
}

func TestReconcilerOnChangeFiresOnlyWhenSomethingChanged(t *testing.T) {
	plane := newFakeDataplane()
	r, cancel := startSyncedReconciler(t, plane) // Service not found -> no-op teardown, nothing installed
	defer cancel()

	var fired int
	r.OnChange = func() { fired++ }
	if err := r.reconcile("ns/svc"); err != nil {
		t.Fatal(err)
	}
	if fired != 0 {
		t.Errorf("OnChange fired %d times for a no-op reconcile, want 0", fired)
	}
}

func TestEnqueueSliceMapsToOwningService(t *testing.T) {
	plane := newFakeDataplane()
	r, cancel := startSyncedReconciler(t, plane)
	defer cancel()

	sl := slice("http", 8080)
	sl.Labels = map[string]string{serviceNameLabel: "svc"}
	r.enqueueSlice(sl)

	key, shutdown := r.queue.Get()
	if shutdown {
		t.Fatal("queue shut down unexpectedly")
	}
	defer r.queue.Done(key)
	if key != "ns/svc" {
		t.Errorf("enqueueSlice produced key %q, want \"ns/svc\"", key)
	}
}
