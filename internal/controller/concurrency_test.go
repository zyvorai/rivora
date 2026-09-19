// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package controller

import (
	"fmt"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// The reconciler runs several workers. The workqueue guarantees one Service key is
// never processed twice at once, but DIFFERENT Services run concurrently and share
// the reconciler's bookkeeping maps. Run under -race: an unguarded shared map is
// reported here, and in production is a concurrent-map-write crash.
func TestReconcileIsSafeAcrossConcurrentWorkers(t *testing.T) {
	const services = 16
	var objs []runtime.Object
	var keys []string
	for i := 0; i < services; i++ {
		name := fmt.Sprintf("svc-%d", i)
		svc := lbService(corev1.ServicePort{Name: "http", Port: int32(80 + i)})
		svc.Name = name
		svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: fmt.Sprintf("10.0.1.%d", i+1)}}
		sl := slice("http", 8080, discoveryv1.Endpoint{
			Addresses:  []string{fmt.Sprintf("10.1.0.%d", i+1)},
			Conditions: discoveryv1.EndpointConditions{Ready: ptr(true), Serving: ptr(true)},
		})
		sl.Name = name + "-abcde"
		sl.Labels = map[string]string{serviceNameLabel: name}
		objs = append(objs, svc, sl)
		keys = append(keys, "ns/"+name)
	}

	plane := newFakeDataplane()
	r, cancel := startSyncedReconciler(t, plane, objs...)
	defer cancel()

	var wg sync.WaitGroup
	for round := 0; round < 4; round++ {
		for _, k := range keys {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := r.reconcile(k); err != nil {
					t.Errorf("reconcile %s: %v", k, err)
				}
			}()
		}
	}
	wg.Wait()

	// Every Service ended up with its VIP installed exactly once in the bookkeeping.
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.installed) != services {
		t.Errorf("installed tracks %d services, want %d", len(r.installed), services)
	}
}
