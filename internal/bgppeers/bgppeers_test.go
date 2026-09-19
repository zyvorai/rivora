// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package bgppeers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"

	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/zyvorai/rivora/api/v1alpha1"
	"github.com/zyvorai/rivora/internal/bgp"
	"github.com/zyvorai/rivora/internal/config"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func peerObj(name, addr string, asn uint32, mutate ...func(*v1alpha1.BGPPeer)) v1alpha1.BGPPeer {
	o := v1alpha1.BGPPeer{
		ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(time.Unix(1000, 0))},
		Spec:       v1alpha1.BGPPeerSpec{Address: addr, ASN: asn},
	}
	for _, m := range mutate {
		m(&o)
	}
	return o
}

func noSecret(string, string) (string, error) { return "", errors.New("no such secret") }

func addrs(ps []config.BGPPeer) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Address)
	}
	sort.Strings(out)
	return out
}

func TestResolveNodeSelector(t *testing.T) {
	rack1 := func(o *v1alpha1.BGPPeer) {
		o.Spec.NodeSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"rack": "1"}}
	}
	objs := []v1alpha1.BGPPeer{
		peerObj("all", "10.0.0.1", 65000),
		peerObj("rack1", "10.0.1.1", 65001, rack1),
	}
	in := Inputs{LocalASN: 65100, Secret: noSecret, HaveNode: true}

	in.NodeLabels = map[string]string{"rack": "1"}
	got, problems := Resolve(objs, in)
	if !reflect.DeepEqual(addrs(got), []string{"10.0.0.1", "10.0.1.1"}) || len(problems) != 0 {
		t.Errorf("on a rack-1 node: peers %v, problems %v", addrs(got), problems)
	}

	in.NodeLabels = map[string]string{"rack": "2"}
	got, problems = Resolve(objs, in)
	if !reflect.DeepEqual(addrs(got), []string{"10.0.0.1"}) || len(problems) != 0 {
		t.Errorf("on a rack-2 node the rack-1 peer must be skipped without complaint: peers %v, problems %v", addrs(got), problems)
	}

	// No node name: a selector can never be judged, so it does not match.
	in.HaveNode, in.NodeLabels = false, nil
	got, _ = Resolve(objs, in)
	if !reflect.DeepEqual(addrs(got), []string{"10.0.0.1"}) {
		t.Errorf("without a node, only the unselected peer applies: %v", addrs(got))
	}
}

func TestResolveDuplicateAddressOldestWins(t *testing.T) {
	older := peerObj("older", "10.0.0.1", 65000)
	older.CreationTimestamp = metav1.NewTime(time.Unix(100, 0))
	newer := peerObj("newer", "10.0.0.1", 65099)
	newer.CreationTimestamp = metav1.NewTime(time.Unix(200, 0))
	// Handed over newest first: the order in the input must not matter.
	got, problems := Resolve([]v1alpha1.BGPPeer{newer, older}, Inputs{LocalASN: 65100, Secret: noSecret})
	if len(got) != 1 || got[0].ASN != 65000 {
		t.Fatalf("the older BGPPeer should win: %+v", got)
	}
	if len(problems) != 1 || problems[0].Name != "newer" {
		t.Errorf("the newer one should be reported: %+v", problems)
	}
}

func TestResolveDuplicateAddressSameAgeLowestNameWins(t *testing.T) {
	a := peerObj("a", "10.0.0.1", 65001)
	b := peerObj("b", "10.0.0.1", 65002) // same creation time as a
	got, problems := Resolve([]v1alpha1.BGPPeer{b, a}, Inputs{LocalASN: 65100, Secret: noSecret})
	if len(got) != 1 || got[0].ASN != 65001 || len(problems) != 1 || problems[0].Name != "b" {
		t.Errorf("with equal ages the lowest name should win deterministically: %+v %v", got, problems)
	}
}

func TestResolveStaticPeerWinsOverAResource(t *testing.T) {
	got, problems := Resolve([]v1alpha1.BGPPeer{peerObj("dup", "10.0.0.1", 65000)},
		Inputs{LocalASN: 65100, Secret: noSecret, Static: []config.BGPPeer{{Address: "10.0.0.1", ASN: 65000}}})
	if len(got) != 0 || len(problems) != 1 {
		t.Errorf("a resource for a startup peer's address must be ignored and reported: peers %v, problems %v", got, problems)
	}
}

func TestResolvePasswordFromSecret(t *testing.T) {
	withPW := func(o *v1alpha1.BGPPeer) {
		o.Spec.PasswordSecretRef = &v1alpha1.SecretKeyRef{Name: "tor"}
	}
	var asked [2]string
	in := Inputs{LocalASN: 65100, Secret: func(name, key string) (string, error) {
		asked = [2]string{name, key}
		return "s3cret", nil
	}}
	got, problems := Resolve([]v1alpha1.BGPPeer{peerObj("a", "10.0.0.1", 65000, withPW)}, in)
	if len(problems) != 0 || len(got) != 1 || got[0].Password != "s3cret" {
		t.Fatalf("password not resolved: %+v %v", got, problems)
	}
	if asked != [2]string{"tor", "password"} {
		t.Errorf("read Secret %v, want the default key \"password\" of Secret tor", asked)
	}

	// A missing Secret must not start the session without its password.
	got, problems = Resolve([]v1alpha1.BGPPeer{peerObj("a", "10.0.0.1", 65000, withPW)}, Inputs{LocalASN: 65100, Secret: noSecret})
	if len(got) != 0 || len(problems) != 1 {
		t.Errorf("a peer whose password cannot be read must not be applied: %v %v", got, problems)
	}
}

func TestResolveRejectsInvalidPeers(t *testing.T) {
	bad := []v1alpha1.BGPPeer{
		peerObj("badaddr", "not-an-ip", 65000),
		peerObj("noasn", "10.0.0.2", 0),
		peerObj("multihop1", "10.0.0.3", 65000, func(o *v1alpha1.BGPPeer) { o.Spec.Multihop = 1 }),
		peerObj("ibgp-multihop", "10.0.0.4", 65100, func(o *v1alpha1.BGPPeer) { o.Spec.Multihop = 3 }), // same AS as the speaker
		peerObj("good", "10.0.0.5", 65000),
	}
	got, problems := Resolve(bad, Inputs{LocalASN: 65100, Secret: noSecret})
	if !reflect.DeepEqual(addrs(got), []string{"10.0.0.5"}) {
		t.Errorf("only the valid peer should apply: %v", addrs(got))
	}
	if len(problems) != 4 {
		t.Errorf("want 4 problems, got %d: %+v", len(problems), problems)
	}
}

func TestResolveCarriesEverySetting(t *testing.T) {
	o := peerObj("full", "10.0.0.1", 65000, func(o *v1alpha1.BGPPeer) {
		o.Spec.BFD = true
		o.Spec.Multihop = 4
		o.Spec.GracefulRestart = &v1alpha1.BGPPeerGracefulRestart{Enabled: true, RestartTime: 90}
	})
	got, problems := Resolve([]v1alpha1.BGPPeer{o}, Inputs{LocalASN: 65100, Secret: noSecret})
	want := config.BGPPeer{Address: "10.0.0.1", ASN: 65000, BFD: true, Multihop: 4, GracefulRestart: &config.BGPGracefulRestart{Enabled: true, RestartTime: 90}}
	if len(problems) != 0 || len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Errorf("got %+v (%v), want %+v", got, problems, want)
	}
}

type fakeApplier struct {
	mu   sync.Mutex
	last []config.BGPPeer
	n    int
}

func (f *fakeApplier) SetPeers(d []config.BGPPeer) (bgp.PeerChanges, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last, f.n = append([]config.BGPPeer(nil), d...), f.n+1
	return bgp.PeerChanges{}, nil
}

func (f *fakeApplier) addrs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return addrs(f.last)
}

func peerUnstructured(t *testing.T, o v1alpha1.BGPPeer) *unstructured.Unstructured {
	t.Helper()
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&o)
	if err != nil {
		t.Fatal(err)
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetAPIVersion(v1alpha1.GroupVersion.String())
	u.SetKind("BGPPeer")
	return u
}

func eventually(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", msg)
}

// End to end through the informer: objects created, selected by node, and deleted change the peers
// handed to the speaker, and the peers it was started with stay throughout.
func TestControllerFollowsTheResources(t *testing.T) {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(v1alpha1.BGPPeerKind, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(v1alpha1.GroupVersion.WithKind("BGPPeerList"), &unstructured.UnstructuredList{})
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{v1alpha1.BGPPeerResource: "BGPPeerList"})

	kube := fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{"rack": "1"}}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "rivora-system", Name: "tor"}, Data: map[string][]byte{"password": []byte("pw\n")}},
	)
	ap := &fakeApplier{}
	static := []config.BGPPeer{{Address: "192.0.2.1", ASN: 65000}}
	c := New(dyn, kube, ap, static, 65100, "n1", "rivora-system", testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	create := func(o v1alpha1.BGPPeer) {
		if _, err := dyn.Resource(v1alpha1.BGPPeerResource).Create(ctx, peerUnstructured(t, o), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "the startup peer is applied with no resources", func() bool { return reflect.DeepEqual(ap.addrs(), []string{"192.0.2.1"}) })

	create(peerObj("a", "10.0.0.1", 65000, func(o *v1alpha1.BGPPeer) { o.Spec.PasswordSecretRef = &v1alpha1.SecretKeyRef{Name: "tor"} }))
	create(peerObj("other-rack", "10.0.9.1", 65000, func(o *v1alpha1.BGPPeer) {
		o.Spec.NodeSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"rack": "9"}}
	}))
	create(peerObj("this-rack", "10.0.1.1", 65000, func(o *v1alpha1.BGPPeer) {
		o.Spec.NodeSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"rack": "1"}}
	}))
	eventually(t, "the startup peer plus the two that apply to this node", func() bool {
		return reflect.DeepEqual(ap.addrs(), []string{"10.0.0.1", "10.0.1.1", "192.0.2.1"})
	})
	ap.mu.Lock()
	for _, p := range ap.last {
		if p.Address == "10.0.0.1" && p.Password != "pw" {
			t.Errorf("the password was not read from the Secret (and trimmed): %q", p.Password)
		}
	}
	ap.mu.Unlock()

	if err := dyn.Resource(v1alpha1.BGPPeerResource).Delete(ctx, "a", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a deleted resource's peer is dropped, the startup peer kept", func() bool {
		return reflect.DeepEqual(ap.addrs(), []string{"10.0.1.1", "192.0.2.1"})
	})
}
