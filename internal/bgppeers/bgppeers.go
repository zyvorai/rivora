// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package bgppeers turns BGPPeer resources into the BGP sessions of the local speaker. The speaker
// is started with the peers its flags or -bgp-config name; this adds the ones the cluster declares,
// filtered to this node, and keeps the running set equal to the union as the resources change.
package bgppeers

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/zyvorai/rivora/api/v1alpha1"
	"github.com/zyvorai/rivora/internal/bgp"
	"github.com/zyvorai/rivora/internal/config"
)

// resync is how often every BGPPeer is looked at again even if nothing changed, which is what
// picks up a rotated password Secret or a relabelled node.
const resync = time.Minute

// Applier is the part of *bgp.Speaker this needs.
type Applier interface {
	SetPeers(desired []config.BGPPeer) (bgp.PeerChanges, error)
}

// Problem is a BGPPeer that is not applied, and why.
type Problem struct {
	Name string
	Err  error
}

// Inputs are what deciding which BGPPeers apply here depends on.
type Inputs struct {
	// LocalASN is the speaker's AS, for validating the peers (multihop is eBGP-only).
	LocalASN uint32
	// NodeLabels are this node's labels, and HaveNode says they are known: with no node name a
	// nodeSelector can never match.
	NodeLabels map[string]string
	HaveNode   bool
	// Static are the peers the speaker started with. A resource for one of their addresses is
	// ignored so it cannot quietly replace that peer's settings.
	Static []config.BGPPeer
	// Secret returns the value of key in the named Secret.
	Secret func(name, key string) (string, error)
}

// Resolve turns the BGPPeer objects into the peers that apply to this node, plus the ones that
// were skipped and why. The result is deterministic: objects are taken oldest first, so when two
// declare the same address the older one wins and the other is reported.
func Resolve(objs []v1alpha1.BGPPeer, in Inputs) (peers []config.BGPPeer, problems []Problem) {
	sorted := append([]v1alpha1.BGPPeer(nil), objs...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i].CreationTimestamp, sorted[j].CreationTimestamp
		if !a.Equal(&b) {
			return a.Before(&b)
		}
		return sorted[i].Name < sorted[j].Name
	})

	taken := map[string]string{} // address -> who has it
	for _, p := range in.Static {
		taken[p.Address] = "the peers this node was started with"
	}
	for _, o := range sorted {
		fail := func(err error) { problems = append(problems, Problem{Name: o.Name, Err: err}) }
		s := o.Spec

		if s.NodeSelector != nil {
			sel, err := metav1.LabelSelectorAsSelector(s.NodeSelector)
			if err != nil {
				fail(fmt.Errorf("nodeSelector: %w", err))
				continue
			}
			if !in.HaveNode || !sel.Matches(labels.Set(in.NodeLabels)) {
				continue // for other nodes: not a problem
			}
		}

		peer := config.BGPPeer{
			Address:  s.Address,
			ASN:      s.ASN,
			BFD:      s.BFD,
			Multihop: s.Multihop,
		}
		if g := s.GracefulRestart; g != nil {
			peer.GracefulRestart = &config.BGPGracefulRestart{Enabled: g.Enabled, RestartTime: g.RestartTime}
		}
		if ref := s.PasswordSecretRef; ref != nil {
			key := ref.Key
			if key == "" {
				key = "password"
			}
			pw, err := in.Secret(ref.Name, key)
			if err != nil {
				// Never start the session without the MD5 password it is meant to have: the peer
				// would refuse it, and a session that does come up unauthenticated is worse.
				fail(fmt.Errorf("password from Secret %s key %s: %w", ref.Name, key, err))
				continue
			}
			peer.Password = pw
		}
		if err := peer.Validate(in.LocalASN); err != nil {
			fail(err)
			continue
		}
		if who, dup := taken[peer.Address]; dup {
			fail(fmt.Errorf("address %s is already used by %s", peer.Address, who))
			continue
		}
		taken[peer.Address] = "BGPPeer " + o.Name
		peers = append(peers, peer)
	}
	return peers, problems
}

// Controller keeps a speaker's peers equal to Static plus the BGPPeer resources that apply.
type Controller struct {
	dyn       dynamic.Interface
	kube      kubernetes.Interface
	speaker   Applier
	logger    *slog.Logger
	nodeName  string
	namespace string // where password Secrets are read
	localASN  uint32
	static    []config.BGPPeer

	trigger chan struct{}

	mu       sync.Mutex
	reported map[string]string // BGPPeer name -> the last problem logged for it
}

// New builds a Controller. static are the peers the speaker was started with, namespace is where
// password Secrets are read from, and nodeName (may be empty) picks the node whose labels
// nodeSelectors are matched against.
func New(dyn dynamic.Interface, kube kubernetes.Interface, speaker Applier, static []config.BGPPeer, localASN uint32, nodeName, namespace string, logger *slog.Logger) *Controller {
	return &Controller{
		dyn: dyn, kube: kube, speaker: speaker, logger: logger,
		nodeName: nodeName, namespace: namespace, localASN: localASN, static: static,
		trigger:  make(chan struct{}, 1),
		reported: map[string]string{},
	}
}

func (c *Controller) poke() {
	select {
	case c.trigger <- struct{}{}:
	default:
	}
}

// Run watches BGPPeer resources until ctx is cancelled, applying them as they change.
func (c *Controller) Run(ctx context.Context) error {
	factory := dynamicinformer.NewDynamicSharedInformerFactory(c.dyn, resync)
	inf := factory.ForResource(v1alpha1.BGPPeerResource).Informer()
	if _, err := inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(interface{}) { c.poke() },
		UpdateFunc: func(_, _ interface{}) { c.poke() },
		DeleteFunc: func(interface{}) { c.poke() },
	}); err != nil {
		return fmt.Errorf("watch BGPPeer: %w", err)
	}
	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), inf.HasSynced) {
		return fmt.Errorf("timed out waiting for the BGPPeer cache to sync")
	}
	c.logger.Info("BGPPeer resources are honoured", "node", c.nodeName)

	c.poke() // one pass for what already exists
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-c.trigger:
			c.sync(ctx, inf.GetStore().List())
		}
	}
}

func (c *Controller) sync(ctx context.Context, raw []interface{}) {
	var objs []v1alpha1.BGPPeer
	for _, r := range raw {
		u, ok := r.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		var o v1alpha1.BGPPeer
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.UnstructuredContent(), &o); err != nil {
			c.logger.Error("BGPPeer is unreadable and is ignored", "name", u.GetName(), "err", err)
			continue
		}
		objs = append(objs, o)
	}

	in := Inputs{
		LocalASN: c.localASN,
		Static:   c.static,
		Secret:   func(name, key string) (string, error) { return c.secret(ctx, name, key) },
	}
	if c.nodeName != "" {
		node, err := c.kube.CoreV1().Nodes().Get(ctx, c.nodeName, metav1.GetOptions{})
		if err != nil {
			// Without the node's labels a nodeSelector cannot be judged; applying the peer anyway
			// could peer this node with a router meant for another rack. Keep what is running.
			c.logger.Error("cannot read this node's labels; BGPPeer changes are not applied until it can", "node", c.nodeName, "err", err)
			return
		}
		in.NodeLabels, in.HaveNode = node.Labels, true
	}

	peers, problems := Resolve(objs, in)
	c.report(problems)
	res, err := c.speaker.SetPeers(append(append([]config.BGPPeer(nil), c.static...), peers...))
	if res != (bgp.PeerChanges{}) {
		c.logger.Info("BGP peers changed", "added", res.Added, "updated", res.Updated, "removed", res.Removed)
	}
	if err != nil {
		c.logger.Error("some BGP peers could not be applied", "err", err)
	}
}

func (c *Controller) secret(ctx context.Context, name, key string) (string, error) {
	sec, err := c.kube.CoreV1().Secrets(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return secretValue(sec, key)
}

func secretValue(sec *corev1.Secret, key string) (string, error) {
	raw, ok := sec.Data[key]
	if !ok {
		return "", fmt.Errorf("no key %q", key)
	}
	v := strings.TrimRight(string(raw), "\r\n")
	if v == "" {
		return "", fmt.Errorf("key %q is empty", key)
	}
	return v, nil
}

// report logs each problem once per distinct message, so a bad BGPPeer is named when it appears
// or changes and not on every minute's resync.
func (c *Controller) report(problems []Problem) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := map[string]string{}
	for _, p := range problems {
		msg := p.Err.Error()
		now[p.Name] = msg
		if c.reported[p.Name] != msg {
			c.logger.Warn("BGPPeer is not applied", "name", p.Name, "err", p.Err)
		}
	}
	c.reported = now
}
