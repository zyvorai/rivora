// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package gatewayapi

import (
	"context"
	"testing"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/client-go/kubernetes/fake"

	gwapi "github.com/zyvorai/rivora/api/gatewayapi"
	"github.com/zyvorai/rivora/internal/initsync"
)

// Every Gateway present at start-up, managed by this controller or not, counts toward the first
// pass; it is over once each has been reconciled and the managed one has been programmed.
func TestFirstPassCoversEveryGateway(t *testing.T) {
	dyn := newDynamicClient()
	createObj(t, dyn, gwapi.GatewayClassResource, gatewayClassObj("rivora", gwapi.ControllerName))
	createObj(t, dyn, gwapi.GatewayClassResource, gatewayClassObj("other", "example.com/other"))
	createObj(t, dyn, gwapi.GatewayResource, gatewayObj("ns", "gw", "rivora", []string{"10.0.0.100"}, listener("tcp-1", "TCP", 80)))
	createObj(t, dyn, gwapi.GatewayResource, gatewayObj("ns", "foreign", "other", []string{"10.0.0.101"}, listener("tcp-1", "TCP", 80)))
	createObj(t, dyn, gwapi.TCPRouteResource, tcpRouteObj("ns", "route", "gw", backendRef("svc", 8080)))
	sl := sliceObj("ns", "svc", 8080, discoveryv1.Endpoint{
		Addresses:  []string{"10.1.0.1"},
		Conditions: discoveryv1.EndpointConditions{Ready: ptr(true), Serving: ptr(true)},
	})

	plane := newFakeDataplane()
	r, factory, dynFactory := New(fake.NewSimpleClientset(svcObj("ns", "svc", 8080), sl), dyn, plane, testLogger())
	tr := initsync.New()
	r.SetInitTracker(tr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Run(ctx, factory, dynFactory, 2) }()

	select {
	case <-tr.Done():
	case <-time.After(5 * time.Second):
		t.Fatalf("first pass never completed; %d gateway(s) pending", tr.Pending())
	}
	plane.mu.Lock()
	defer plane.mu.Unlock()
	if len(plane.upserts) != 1 || plane.upserts[0].Address != "10.0.0.100" {
		t.Errorf("done before the managed Gateway was programmed: %+v", plane.upserts)
	}
}
