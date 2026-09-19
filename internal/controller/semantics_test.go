// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package controller

import (
	"bytes"
	"context"
	"log/slog"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	"github.com/zyvorai/rivora/internal/config"
)

func onNode(addr, node string, ready, serving bool) discoveryv1.Endpoint {
	ep := discoveryv1.Endpoint{
		Addresses:  []string{addr},
		Conditions: discoveryv1.EndpointConditions{Ready: ptr(ready), Serving: ptr(serving)},
	}
	if node != "" {
		ep.NodeName = ptr(node)
	}
	return ep
}

func backendAddrs(vips []desiredVIP) []string {
	var out []string
	for _, v := range vips {
		for _, b := range v.VIP.Backends {
			out = append(out, b.Address)
		}
	}
	sort.Strings(out)
	return out
}

func localService(policy corev1.ServiceExternalTrafficPolicy) *corev1.Service {
	svc := lbService(corev1.ServicePort{Name: "http", Port: 80})
	svc.Spec.ExternalTrafficPolicy = policy
	return svc
}

// ---- externalTrafficPolicy: Local ----

func TestLocalPolicyKeepsOnlyThisNodesEndpoints(t *testing.T) {
	sl := slice("http", 8080,
		onNode("10.1.0.1", "node-a", true, true),
		onNode("10.1.0.2", "node-a", true, true),
		onNode("10.1.0.3", "node-b", true, true),
		onNode("10.1.0.4", "node-c", true, true),
	)
	got, err := buildDesiredVIPs(localService(corev1.ServiceExternalTrafficPolicyLocal), []*discoveryv1.EndpointSlice{sl}, buildOptions{LocalNode: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"10.1.0.1", "10.1.0.2"}; strings.Join(backendAddrs(got), ",") != strings.Join(want, ",") {
		t.Errorf("backends = %v, want only node-a's %v", backendAddrs(got), want)
	}
}

func TestLocalPolicyDoesNotConfuseSimilarNodeNames(t *testing.T) {
	sl := slice("http", 8080,
		onNode("10.1.0.1", "node-1", true, true),
		onNode("10.1.0.10", "node-10", true, true),
		onNode("10.1.0.11", "Node-1", true, true), // node names are case-sensitive
	)
	got, _ := buildDesiredVIPs(localService(corev1.ServiceExternalTrafficPolicyLocal), []*discoveryv1.EndpointSlice{sl}, buildOptions{LocalNode: "node-1"})
	if strings.Join(backendAddrs(got), ",") != "10.1.0.1" {
		t.Errorf("backends = %v, want only the exact node-1", backendAddrs(got))
	}
}

func TestLocalPolicyDropsEndpointsWithNoNode(t *testing.T) {
	// An endpoint with no nodeName (an external address, a hand-authored slice) is on
	// no node, so it must not be assumed local.
	sl := slice("http", 8080, onNode("10.1.0.1", "node-a", true, true), onNode("192.0.2.9", "", true, true))
	got, _ := buildDesiredVIPs(localService(corev1.ServiceExternalTrafficPolicyLocal), []*discoveryv1.EndpointSlice{sl}, buildOptions{LocalNode: "node-a"})
	if strings.Join(backendAddrs(got), ",") != "10.1.0.1" {
		t.Errorf("backends = %v, want the nodeless endpoint dropped", backendAddrs(got))
	}
}

func TestLocalPolicyNodeWithNoLocalEndpointsGetsNoVIP(t *testing.T) {
	// The whole point under BGP: a node with no local pods programs no VIP, so it
	// has no healthy backend for it and withdraws the route instead of attracting
	// traffic it would have to forward elsewhere.
	sl := slice("http", 8080, onNode("10.1.0.3", "node-b", true, true))
	got, err := buildDesiredVIPs(localService(corev1.ServiceExternalTrafficPolicyLocal), []*discoveryv1.EndpointSlice{sl}, buildOptions{LocalNode: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %d VIPs on a node with no local endpoints, want none", len(got))
	}
}

func TestLocalPolicyKeepsLocalTerminatingEndpointsDraining(t *testing.T) {
	sl := slice("http", 8080,
		onNode("10.1.0.1", "node-a", true, true),
		onNode("10.1.0.2", "node-a", false, true), // terminating here: keep, draining
		onNode("10.1.0.3", "node-b", false, true), // terminating elsewhere: not ours
	)
	got, _ := buildDesiredVIPs(localService(corev1.ServiceExternalTrafficPolicyLocal), []*discoveryv1.EndpointSlice{sl}, buildOptions{LocalNode: "node-a"})
	if strings.Join(backendAddrs(got), ",") != "10.1.0.1,10.1.0.2" {
		t.Fatalf("backends = %v", backendAddrs(got))
	}
	if len(got[0].DrainingBackends) != 1 || got[0].DrainingBackends[0].Address != "10.1.0.2" {
		t.Errorf("draining = %+v, want just the local terminating endpoint", got[0].DrainingBackends)
	}
}

func TestClusterPolicyIgnoresTheNodeFilter(t *testing.T) {
	sl := slice("http", 8080, onNode("10.1.0.1", "node-a", true, true), onNode("10.1.0.3", "node-b", true, true))
	for _, policy := range []corev1.ServiceExternalTrafficPolicy{corev1.ServiceExternalTrafficPolicyCluster, ""} {
		got, _ := buildDesiredVIPs(localService(policy), []*discoveryv1.EndpointSlice{sl}, buildOptions{LocalNode: "node-a"})
		if len(backendAddrs(got)) != 2 {
			t.Errorf("policy %q: backends = %v, want every endpoint (only Local restricts)", policy, backendAddrs(got))
		}
	}
}

func TestLocalPolicyNotHonouredWhenNoNodeIsSet(t *testing.T) {
	// The safe default: with no LocalNode (L2 speaker, or no node name), a Local
	// Service keeps every endpoint, exactly as before this feature existed.
	sl := slice("http", 8080, onNode("10.1.0.1", "node-a", true, true), onNode("10.1.0.3", "node-b", true, true))
	got, _ := buildDesiredVIPs(localService(corev1.ServiceExternalTrafficPolicyLocal), []*discoveryv1.EndpointSlice{sl}, buildOptions{})
	if len(backendAddrs(got)) != 2 {
		t.Errorf("backends = %v, want all of them when Local isn't honoured", backendAddrs(got))
	}
}

func TestExportedEndpointsForPortIsUnfiltered(t *testing.T) {
	// The Gateway reconciler reuses this and must not silently start filtering by node.
	sl := slice("http", 8080, onNode("10.1.0.1", "node-a", true, true), onNode("10.1.0.3", "node-b", true, true))
	b, _ := EndpointsForPort([]*discoveryv1.EndpointSlice{sl}, "http")
	if len(b) != 2 {
		t.Errorf("EndpointsForPort returned %d backends, want 2", len(b))
	}
}

// ---- sessionAffinity ----

func TestSessionAffinityClientIPIsMapped(t *testing.T) {
	sl := slice("http", 8080, onNode("10.1.0.1", "node-a", true, true))
	for name, c := range map[string]struct {
		aff  corev1.ServiceAffinity
		want config.SessionAffinity
	}{
		"ClientIP": {corev1.ServiceAffinityClientIP, config.AffinityClientIP},
		"None":     {corev1.ServiceAffinityNone, ""},
		"unset":    {"", ""},
	} {
		svc := lbService(corev1.ServicePort{Name: "http", Port: 80})
		svc.Spec.SessionAffinity = c.aff
		got, _ := buildDesiredVIPs(svc, []*discoveryv1.EndpointSlice{sl}, buildOptions{})
		if len(got) != 1 || got[0].VIP.SessionAffinity != c.want {
			t.Errorf("%s: affinity = %q, want %q", name, got[0].VIP.SessionAffinity, c.want)
		}
	}
}

func TestSessionAffinityAppliesToEveryPort(t *testing.T) {
	svc := lbService(corev1.ServicePort{Name: "http", Port: 80}, corev1.ServicePort{Name: "https", Port: 443})
	svc.Spec.SessionAffinity = corev1.ServiceAffinityClientIP
	sl := slice("http", 8080, onNode("10.1.0.1", "node-a", true, true))
	sl2 := slice("https", 8443, onNode("10.1.0.1", "node-a", true, true))
	sl2.Name = "svc-fghij"
	got, _ := buildDesiredVIPs(svc, []*discoveryv1.EndpointSlice{sl, sl2}, buildOptions{})
	if len(got) != 2 {
		t.Fatalf("got %d VIPs, want 2", len(got))
	}
	for _, v := range got {
		if v.VIP.SessionAffinity != config.AffinityClientIP {
			t.Errorf("port %d lost the affinity: %q", v.VIP.Port, v.VIP.SessionAffinity)
		}
	}
}

func TestSessionAffinityAndLocalPolicyCombine(t *testing.T) {
	svc := localService(corev1.ServiceExternalTrafficPolicyLocal)
	svc.Spec.SessionAffinity = corev1.ServiceAffinityClientIP
	sl := slice("http", 8080, onNode("10.1.0.1", "node-a", true, true), onNode("10.1.0.3", "node-b", true, true))
	got, _ := buildDesiredVIPs(svc, []*discoveryv1.EndpointSlice{sl}, buildOptions{LocalNode: "node-a"})
	if len(got) != 1 || len(got[0].VIP.Backends) != 1 || got[0].VIP.SessionAffinity != config.AffinityClientIP {
		t.Errorf("got %+v, want one local backend and clientIP affinity", got)
	}
}

// ---- the warning, through the real reconciler ----

func reconcilerWith(t *testing.T, policy LocalPolicy, objs ...runtime.Object) (*Reconciler, *fakeDataplane, *bytes.Buffer, func()) {
	t.Helper()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	plane := newFakeDataplane()
	r, factory := New(fake.NewSimpleClientset(objs...), plane, "", logger)
	r.SetLocalPolicy(policy)
	ctx, cancel := context.WithCancel(context.Background())
	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), factory.Core().V1().Services().Informer().HasSynced, factory.Discovery().V1().EndpointSlices().Informer().HasSynced) {
		cancel()
		t.Fatal("caches never synced")
	}
	return r, plane, &logs, cancel
}

func TestUnhonouredLocalPolicyWarnsOncePerService(t *testing.T) {
	svc := localService(corev1.ServiceExternalTrafficPolicyLocal)
	sl := slice("http", 8080, onNode("10.1.0.1", "node-a", true, true))
	r, plane, logs, cancel := reconcilerWith(t, LocalPolicy{NotHonouredReason: "the L2 speaker answers from a single node"}, svc, sl)
	defer cancel()

	for i := 0; i < 3; i++ {
		if err := r.reconcile("ns/svc"); err != nil {
			t.Fatal(err)
		}
	}
	out := logs.String()
	if n := strings.Count(out, "externalTrafficPolicy: Local is being treated as Cluster"); n != 1 {
		t.Errorf("warned %d times over 3 reconciles, want exactly once:\n%s", n, out)
	}
	if !strings.Contains(out, "ns/svc") || !strings.Contains(out, "L2 speaker") {
		t.Errorf("warning should name the Service and the reason: %s", out)
	}
	// It still worked: the endpoint was programmed as Cluster would.
	if len(plane.upserts) == 0 {
		t.Error("the Service was not programmed at all")
	}
}

func TestHonouredLocalPolicyDoesNotWarnAndFilters(t *testing.T) {
	svc := localService(corev1.ServiceExternalTrafficPolicyLocal)
	sl := slice("http", 8080, onNode("10.1.0.1", "node-a", true, true), onNode("10.1.0.3", "node-b", true, true))
	r, plane, logs, cancel := reconcilerWith(t, LocalPolicy{Node: "node-a"}, svc, sl)
	defer cancel()

	if err := r.reconcile("ns/svc"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "treated as Cluster") {
		t.Errorf("warned although Local is honoured: %s", logs.String())
	}
	last := plane.upserts[len(plane.upserts)-1]
	if len(last.Backends) != 1 || last.Backends[0].Address != "10.1.0.1" {
		t.Errorf("programmed backends = %+v, want only node-a's", last.Backends)
	}
}

func TestClusterServiceNeverWarns(t *testing.T) {
	svc := localService(corev1.ServiceExternalTrafficPolicyCluster)
	sl := slice("http", 8080, onNode("10.1.0.1", "node-a", true, true))
	r, _, logs, cancel := reconcilerWith(t, LocalPolicy{}, svc, sl)
	defer cancel()
	_ = r.reconcile("ns/svc")
	if strings.Contains(logs.String(), "Local") {
		t.Errorf("a Cluster Service produced a Local warning: %s", logs.String())
	}
}

func TestLocalWarningIsForgottenWhenTheServiceIsDeleted(t *testing.T) {
	svc := localService(corev1.ServiceExternalTrafficPolicyLocal)
	sl := slice("http", 8080, onNode("10.1.0.1", "node-a", true, true))
	r, _, logs, cancel := reconcilerWith(t, LocalPolicy{}, svc, sl)
	defer cancel()

	_ = r.reconcile("ns/svc")
	r.forgetLocalWarning("ns/svc") // what reconcile does when the Service is gone
	_ = r.reconcile("ns/svc")
	if n := strings.Count(logs.String(), "treated as Cluster"); n != 2 {
		t.Errorf("a re-created Service was warned %d times in total, want 2 (once per lifetime)", n)
	}
}
