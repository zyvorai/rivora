// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/zyvorai/rivora/internal/config"
	"github.com/zyvorai/rivora/internal/initsync"
)

type failingPlane struct{ *fakeDataplane }

func (failingPlane) UpsertVIP(config.VIP) error { return errors.New("map full") }

func runWithTracker(t *testing.T, plane dataplaner) *initsync.Tracker {
	t.Helper()
	ready := discoveryv1.Endpoint{
		Addresses:  []string{"10.1.0.1"},
		Conditions: discoveryv1.EndpointConditions{Ready: ptr(true), Serving: ptr(true)},
	}
	plain := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "plain"}} // not a LoadBalancer
	r, factory := New(fake.NewSimpleClientset(lbService(corev1.ServicePort{Name: "http", Port: 80}), slice("http", 8080, ready), plain), plane, "", testLogger())
	tr := initsync.New()
	r.SetInitTracker(tr)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = r.Run(ctx, factory, 2) }()
	return tr
}

// The first pass is over when every Service that existed at start-up, wanted or not, has been
// reconciled: that is what lets rivorad remove the VIPs nothing claimed.
func TestFirstPassCompletesOnceEveryServiceIsReconciled(t *testing.T) {
	plane := newFakeDataplane()
	tr := runWithTracker(t, plane)
	select {
	case <-tr.Done():
	case <-time.After(5 * time.Second):
		t.Fatalf("first pass never completed; %d service(s) pending", tr.Pending())
	}
	plane.mu.Lock()
	defer plane.mu.Unlock()
	if len(plane.upserts) != 1 {
		t.Errorf("done before the LoadBalancer Service was programmed: %d upserts", len(plane.upserts))
	}
}

// A Service that cannot be programmed keeps the first pass open: pruning while it is unresolved
// could remove the VIP it still wants.
func TestFirstPassStaysOpenWhileAServiceFails(t *testing.T) {
	tr := runWithTracker(t, failingPlane{newFakeDataplane()})
	select {
	case <-tr.Done():
		t.Fatal("first pass completed although a Service failed to reconcile")
	case <-time.After(700 * time.Millisecond):
	}
	if tr.Pending() != 1 {
		t.Errorf("%d pending, want exactly the failing Service", tr.Pending())
	}
}
