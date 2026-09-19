// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	"github.com/zyvorai/rivora/api/v1alpha1"
	"github.com/zyvorai/rivora/internal/config"
)

func policyObj(name, target string, created time.Time, spec map[string]any) *unstructured.Unstructured {
	spec["targetRef"] = map[string]any{"name": target}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rivora.zyvor.dev/v1alpha1",
		"kind":       "ServicePolicy",
		"metadata": map[string]any{
			"name": name, "namespace": "ns",
			"creationTimestamp": created.UTC().Format(time.RFC3339),
			"resourceVersion":   "1",
		},
		"spec": spec,
	}}
}

func TestCompilePolicyRejectsBadInput(t *testing.T) {
	mk := func(mut func(*v1alpha1.ServicePolicySpec)) *v1alpha1.ServicePolicy {
		p := &v1alpha1.ServicePolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p"}}
		p.Spec.TargetRef.Name = "svc"
		mut(&p.Spec)
		return p
	}
	cases := []struct {
		name string
		mut  func(*v1alpha1.ServicePolicySpec)
		want string // substring of the error; "" means valid
	}{
		{"empty is valid", func(*v1alpha1.ServicePolicySpec) {}, ""},
		{"wrong kind", func(s *v1alpha1.ServicePolicySpec) { s.TargetRef.Kind = "Gateway" }, "only Service"},
		{"unknown probe type", func(s *v1alpha1.ServicePolicySpec) { s.HealthCheck = &v1alpha1.HealthCheckPolicy{Type: "grpc"} }, "tcp or http"},
		{"http path without slash", func(s *v1alpha1.ServicePolicySpec) {
			s.HealthCheck = &v1alpha1.HealthCheckPolicy{Type: "http", Path: "healthz"}
		}, "must start with /"},
		{"tcp probe with http fields", func(s *v1alpha1.ServicePolicySpec) { s.HealthCheck = &v1alpha1.HealthCheckPolicy{Path: "/x"} }, "only apply to type http"},
		{"rate limit missing burst", func(s *v1alpha1.ServicePolicySpec) {
			s.RateLimit = &v1alpha1.RateLimitPolicy{PerSourcePacketsPerSecond: 10}
		}, "both be > 0"},
		{"rate limit all zero", func(s *v1alpha1.ServicePolicySpec) { s.RateLimit = &v1alpha1.RateLimitPolicy{} }, "both be > 0"},
		{"node weight zero", func(s *v1alpha1.ServicePolicySpec) {
			s.Weights = &v1alpha1.WeightPolicy{Nodes: map[string]uint32{"a": 0}}
		}, "1 or more"},
		{"weight too big", func(s *v1alpha1.ServicePolicySpec) { s.Weights = &v1alpha1.WeightPolicy{Default: 5000} }, "above the maximum"},
		{"everything valid", func(s *v1alpha1.ServicePolicySpec) {
			s.HealthCheck = &v1alpha1.HealthCheckPolicy{Type: "http", Path: "/healthz", ExpectStatus: "200"}
			s.RateLimit = &v1alpha1.RateLimitPolicy{PerSourcePacketsPerSecond: 100, Burst: 200}
			s.Weights = &v1alpha1.WeightPolicy{Default: 2, Nodes: map[string]uint32{"a": 5}}
		}, ""},
	}
	for _, c := range cases {
		_, err := compilePolicy(mk(c.mut))
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: error %v, want it to contain %q", c.name, err, c.want)
		}
	}
}

func TestPickPolicyOldestWinsThenName(t *testing.T) {
	at := func(name string, min int) *v1alpha1.ServicePolicy {
		p := &v1alpha1.ServicePolicy{}
		p.Name = name
		p.CreationTimestamp = metav1.NewTime(time.Date(2026, 1, 1, 0, min, 0, 0, time.UTC))
		return p
	}
	winner, ignored := pickPolicy([]*v1alpha1.ServicePolicy{at("c", 5), at("b", 1), at("a", 1), at("d", 9)})
	if winner.Name != "a" {
		t.Errorf("winner %q, want a (oldest, then lowest name)", winner.Name)
	}
	if len(ignored) != 3 {
		t.Errorf("ignored %d, want 3", len(ignored))
	}
	// Order of the input must not change the outcome.
	w2, _ := pickPolicy([]*v1alpha1.ServicePolicy{at("d", 9), at("a", 1), at("b", 1), at("c", 5)})
	if w2.Name != winner.Name {
		t.Errorf("winner depends on input order: %q vs %q", w2.Name, winner.Name)
	}
	if w, ig := pickPolicy(nil); w != nil || ig != nil {
		t.Errorf("no policies should pick nothing, got %v %v", w, ig)
	}
}

func TestBuildAppliesPolicyToEveryVIP(t *testing.T) {
	p := &v1alpha1.ServicePolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p"}}
	p.Spec.TargetRef.Name = "svc"
	p.Spec.HealthCheck = &v1alpha1.HealthCheckPolicy{Type: "http", Path: "/ready", Port: 9000}
	p.Spec.RateLimit = &v1alpha1.RateLimitPolicy{PerSourcePacketsPerSecond: 50, Burst: 100}
	p.Spec.Weights = &v1alpha1.WeightPolicy{Default: 2, Nodes: map[string]uint32{"node-a": 7}}
	sp, err := compilePolicy(p)
	if err != nil {
		t.Fatal(err)
	}

	svc := lbService(corev1.ServicePort{Name: "http", Port: 80}, corev1.ServicePort{Name: "alt", Port: 81})
	sl := slice("http", 8080,
		onNode("10.1.0.1", "node-a", true, true),
		onNode("10.1.0.2", "node-b", true, true),
		onNode("10.1.0.3", "", true, true),
	)
	sl.Ports = append(sl.Ports, discoveryv1.EndpointPort{Name: ptr("alt"), Port: ptr(int32(8081))})

	got, err := buildDesiredVIPs(svc, []*discoveryv1.EndpointSlice{sl}, buildOptions{Policy: sp})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d VIPs, want 2 (one per Service port)", len(got))
	}
	for _, d := range got {
		if d.VIP.HealthCheck.Type != config.ProbeHTTP || d.VIP.HealthCheck.Path != "/ready" || d.VIP.HealthCheck.Port != 9000 {
			t.Errorf("vip %d: health check not applied: %+v", d.VIP.Port, d.VIP.HealthCheck)
		}
		if d.VIP.RateLimit != (config.VIPRateLimit{PerSourcePacketsPerSecond: 50, Burst: 100}) {
			t.Errorf("vip %d: rate limit not applied: %+v", d.VIP.Port, d.VIP.RateLimit)
		}
		weights := map[string]uint32{}
		for _, b := range d.VIP.Backends {
			weights[b.Address] = b.Weight
		}
		want := map[string]uint32{"10.1.0.1": 7, "10.1.0.2": 2, "10.1.0.3": 2} // listed node, other node, no node
		for a, w := range want {
			if weights[a] != w {
				t.Errorf("vip %d backend %s: weight %d, want %d", d.VIP.Port, a, weights[a], w)
			}
		}
	}
}

func TestBuildWithoutPolicyIsUnchanged(t *testing.T) {
	svc := lbService(corev1.ServicePort{Name: "http", Port: 80})
	sl := slice("http", 8080, onNode("10.1.0.1", "node-a", true, true))
	got, err := buildDesiredVIPs(svc, []*discoveryv1.EndpointSlice{sl}, buildOptions{})
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v, %v", got, err)
	}
	v := got[0].VIP
	if v.HealthCheck != (config.ProbeSpec{}) || v.RateLimit.Set() || v.Backends[0].Weight != 0 {
		t.Errorf("no policy must leave defaults alone: %+v", v)
	}
}

// policyHarness runs a real Reconciler against fake typed and dynamic clients.
type policyHarness struct {
	r      *Reconciler
	plane  *fakeDataplane
	dyn    *dynfake.FakeDynamicClient
	cancel func()
}

func newPolicyHarness(t *testing.T, policies ...*unstructured.Unstructured) *policyHarness {
	t.Helper()
	svc := lbService(corev1.ServicePort{Name: "http", Port: 80})
	sl := slice("http", 8080, onNode("10.1.0.1", "node-a", true, true), onNode("10.1.0.2", "node-b", true, true))

	plane := newFakeDataplane()
	r, factory := New(fake.NewSimpleClientset(svc, sl), plane, "", testLogger())

	var objs []runtime.Object
	for _, p := range policies {
		objs = append(objs, p)
	}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{v1alpha1.ServicePolicyResource: "ServicePolicyList"}, objs...)
	r.EnableServicePolicies(dyn)

	ctx, cancel := context.WithCancel(context.Background())
	factory.Start(ctx.Done())
	r.policyFactory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(),
		factory.Core().V1().Services().Informer().HasSynced,
		factory.Discovery().V1().EndpointSlices().Informer().HasSynced,
		r.policyFactory.ForResource(v1alpha1.ServicePolicyResource).Informer().HasSynced,
	) {
		cancel()
		t.Fatal("caches never synced")
	}
	return &policyHarness{r: r, plane: plane, dyn: dyn, cancel: cancel}
}

func (h *policyHarness) lastVIP(t *testing.T) config.VIP {
	t.Helper()
	h.plane.mu.Lock()
	defer h.plane.mu.Unlock()
	if len(h.plane.upserts) == 0 {
		t.Fatal("no VIP was upserted")
	}
	return h.plane.upserts[len(h.plane.upserts)-1]
}

func TestReconcilerAppliesServicePolicy(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	h := newPolicyHarness(t, policyObj("probe", "svc", old, map[string]any{
		"healthCheck": map[string]any{"type": "http", "path": "/ready"},
		"rateLimit":   map[string]any{"perSourcePacketsPerSecond": int64(20), "burst": int64(40)},
		"weights":     map[string]any{"nodes": map[string]any{"node-a": int64(9)}},
	}))
	defer h.cancel()

	if err := h.r.reconcile("ns/svc"); err != nil {
		t.Fatal(err)
	}
	v := h.lastVIP(t)
	if v.HealthCheck.Type != config.ProbeHTTP || v.HealthCheck.Path != "/ready" {
		t.Errorf("health check not applied: %+v", v.HealthCheck)
	}
	if v.RateLimit.PerSourcePacketsPerSecond != 20 || v.RateLimit.Burst != 40 {
		t.Errorf("rate limit not applied: %+v", v.RateLimit)
	}
	w := map[string]uint32{}
	for _, b := range v.Backends {
		w[b.Address] = b.Weight
	}
	if w["10.1.0.1"] != 9 || w["10.1.0.2"] != 0 {
		t.Errorf("weights = %v, want node-a's endpoint at 9 and node-b's unset", w)
	}
}

func TestReconcilerOldestPolicyWinsAndInvalidFallsBack(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	newer := time.Now().Add(-time.Minute)
	h := newPolicyHarness(t,
		policyObj("newer", "svc", newer, map[string]any{"rateLimit": map[string]any{"perSourcePacketsPerSecond": int64(1), "burst": int64(1)}}),
		policyObj("older", "svc", old, map[string]any{"rateLimit": map[string]any{"perSourcePacketsPerSecond": int64(77), "burst": int64(88)}}),
		policyObj("other-svc", "elsewhere", old, map[string]any{"rateLimit": map[string]any{"perSourcePacketsPerSecond": int64(5), "burst": int64(5)}}),
	)
	defer h.cancel()
	if err := h.r.reconcile("ns/svc"); err != nil {
		t.Fatal(err)
	}
	if got := h.lastVIP(t).RateLimit; got.PerSourcePacketsPerSecond != 77 {
		t.Errorf("rate limit %+v, want the oldest policy's (77)", got)
	}

	// An invalid winner is not half-applied: the Service keeps its defaults.
	bad := newPolicyHarness(t, policyObj("bad", "svc", old, map[string]any{
		"healthCheck": map[string]any{"type": "http", "path": "no-slash"},
		"rateLimit":   map[string]any{"perSourcePacketsPerSecond": int64(9), "burst": int64(9)},
	}))
	defer bad.cancel()
	if err := bad.r.reconcile("ns/svc"); err != nil {
		t.Fatal(err)
	}
	v := bad.lastVIP(t)
	if v.HealthCheck != (config.ProbeSpec{}) || v.RateLimit.Set() {
		t.Errorf("an invalid policy must apply nothing, got probe %+v limit %+v", v.HealthCheck, v.RateLimit)
	}
}

func TestPolicyEventRequeuesTargetService(t *testing.T) {
	h := newPolicyHarness(t)
	defer h.cancel()
	p := policyObj("late", "svc", time.Now(), map[string]any{"rateLimit": map[string]any{"perSourcePacketsPerSecond": int64(3), "burst": int64(3)}})
	if _, err := h.dyn.Resource(v1alpha1.ServicePolicyResource).Namespace("ns").Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for h.r.queue.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	key, shutdown := h.r.queue.Get()
	if shutdown || key != "ns/svc" {
		t.Fatalf("policy creation queued %q (shutdown=%v), want ns/svc", key, shutdown)
	}
}

func TestPolicyBGPCommunitiesReachTheVIP(t *testing.T) {
	p := &v1alpha1.ServicePolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p"}}
	p.Spec.TargetRef.Name = "svc"
	p.Spec.BGP = &v1alpha1.BGPPolicy{Communities: []string{"65000:42", "no-export"}}
	sp, err := compilePolicy(p)
	if err != nil {
		t.Fatal(err)
	}
	svc := lbService(corev1.ServicePort{Name: "http", Port: 80})
	sl := slice("http", 8080, onNode("10.1.0.1", "node-a", true, true))
	got, err := buildDesiredVIPs(svc, []*discoveryv1.EndpointSlice{sl}, buildOptions{Policy: sp})
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v, %v", got, err)
	}
	if c := got[0].VIP.BGPCommunities; len(c) != 2 || c[0] != "65000:42" || c[1] != "no-export" {
		t.Errorf("VIP communities = %v, want [65000:42 no-export]", c)
	}
	p.Spec.BGP.Communities = []string{"65000:notanumber"}
	if _, err := compilePolicy(p); err == nil || !strings.Contains(err.Error(), "bgp.communities") {
		t.Errorf("a bad community must reject the whole policy, got %v", err)
	}
}
