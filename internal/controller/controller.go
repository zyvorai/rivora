// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package controller is rivorad's in-process Kubernetes reconciler: it
// watches Service (type=LoadBalancer) and the EndpointSlices backing them,
// and programs the node's local Dataplane to match. Every node in the
// DaemonSet runs one of these independently and reconciles the same
// cluster state into its own BPF maps — there's no leader election here
// (contrast cmd/rivora-controller's IPAM, which is a single cluster-wide
// writer to Service.Status and so does need one), and no reconciler-level
// coordination between nodes is needed since each just mirrors what it
// observes.
package controller

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/zyvorai/rivora/internal/config"
	"github.com/zyvorai/rivora/internal/dataplane"
)

// serviceNameLabel is set by Kubernetes on every EndpointSlice to the name
// of the Service it belongs to (same namespace) — the standard way to map
// a slice back to its owning Service without an OwnerReference lookup.
const serviceNameLabel = "kubernetes.io/service-name"

// dataplaner is the subset of *dataplane.Dataplane the reconciler needs,
// so tests can substitute a fake without real BPF maps.
type dataplaner interface {
	UpsertVIP(vip config.VIP) error
	RemoveVIP(key string) error
	SetBackendDrainingByKey(vipKey, backendKey string, draining bool) error
}

var _ dataplaner = (*dataplane.Dataplane)(nil)

// Reconciler is rivorad's Service/EndpointSlice -> Dataplane control loop.
type Reconciler struct {
	plane   dataplaner
	logger  *slog.Logger
	lbClass string // matched against Service.Spec.LoadBalancerClass; "" manages every LoadBalancer Service with no class set

	serviceLister corelisters.ServiceLister
	sliceLister   discoverylisters.EndpointSliceLister

	queue workqueue.TypedRateLimitingInterface[string]

	// installed tracks, per "namespace/name" Service key, the VIP keys
	// (dataplane.VIPKey) this reconciler most recently programmed for it —
	// so a Reconcile that finds fewer (or zero, e.g. on delete) desired
	// VIPs knows exactly which stale ones to RemoveVIP, without needing
	// the now-gone Service object to reconstruct them.
	//
	// mu guards installed. The workqueue guarantees one Service key is never
	// processed by two workers at once, but different Services run concurrently
	// (-workers, default 2) and all write this map: unguarded, two reconciles at
	// the same moment crash the process with "concurrent map read and map write".
	// It is held only around the map access, never across a dataplane call.
	mu        sync.Mutex
	installed map[string][]string

	// localPolicy says how externalTrafficPolicy: Local is treated; see LocalPolicy.
	// Set before Run; read-only afterwards.
	localPolicy LocalPolicy
	// warnedLocal remembers which Services have already been warned that their Local
	// policy isn't honoured, so the warning is once per Service, not per reconcile.
	// Guarded by mu.
	warnedLocal map[string]bool

	// OnChange, if set, is called after every reconcile that touched the
	// dataplane (create/update/remove) — rivorad uses it to refresh the
	// active health checker's target list, since Targets() only reflects
	// whatever's currently in the maps.
	OnChange func()
}

// New builds a Reconciler. lbClass selects which LoadBalancer Services this
// node manages (see managed()); pass "" for "every LoadBalancer Service
// with no explicit class" (the common single-LB-controller-per-cluster
// case).
func New(clientset kubernetes.Interface, plane dataplaner, lbClass string, logger *slog.Logger) (*Reconciler, informers.SharedInformerFactory) {
	factory := informers.NewSharedInformerFactory(clientset, 30*time.Second)
	svcInformer := factory.Core().V1().Services()
	sliceInformer := factory.Discovery().V1().EndpointSlices()

	r := &Reconciler{
		plane:         plane,
		logger:        logger,
		lbClass:       lbClass,
		serviceLister: svcInformer.Lister(),
		sliceLister:   sliceInformer.Lister(),
		queue: workqueue.NewTypedRateLimitingQueue[string](
			workqueue.DefaultTypedControllerRateLimiter[string](),
		),
		installed:   map[string][]string{},
		warnedLocal: map[string]bool{},
	}

	svcInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { r.enqueueService(obj) },
		UpdateFunc: func(_, obj interface{}) { r.enqueueService(obj) },
		DeleteFunc: func(obj interface{}) { r.enqueueService(obj) },
	})
	sliceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { r.enqueueSlice(obj) },
		UpdateFunc: func(_, obj interface{}) { r.enqueueSlice(obj) },
		DeleteFunc: func(obj interface{}) { r.enqueueSlice(obj) },
	})

	return r, factory
}

// LocalPolicy says how a Service's externalTrafficPolicy: Local is treated.
type LocalPolicy struct {
	// Node, when set, honours Local: the Service's VIP uses only endpoints on this
	// node. Set it only where a node without local endpoints won't attract the
	// traffic, which in practice means BGP with the L2 speaker off.
	Node string
	// NotHonouredReason is logged, once per Service, when a Service asks for Local
	// but Node is empty, so the operator knows it was treated as Cluster.
	NotHonouredReason string
}

// SetLocalPolicy configures externalTrafficPolicy: Local handling. Call before Run.
func (r *Reconciler) SetLocalPolicy(p LocalPolicy) { r.localPolicy = p }

func (r *Reconciler) enqueueService(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		return
	}
	r.queue.Add(key)
}

func (r *Reconciler) enqueueSlice(obj interface{}) {
	slice, ok := obj.(*discoveryv1.EndpointSlice)
	if !ok {
		if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
			slice, ok = tomb.Obj.(*discoveryv1.EndpointSlice)
			if !ok {
				return
			}
		} else {
			return
		}
	}
	svcName, ok := slice.Labels[serviceNameLabel]
	if !ok || svcName == "" {
		return
	}
	r.queue.Add(slice.Namespace + "/" + svcName)
}

// Run starts the reconcile loop and blocks until ctx is cancelled. The
// caller starts the informer factory (factory.Start) separately — Run just
// waits for its caches to sync before serving the queue.
func (r *Reconciler) Run(ctx context.Context, factory informers.SharedInformerFactory, workers int) error {
	defer r.queue.ShutDown()

	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(),
		factory.Core().V1().Services().Informer().HasSynced,
		factory.Discovery().V1().EndpointSlices().Informer().HasSynced,
	) {
		return fmt.Errorf("timed out waiting for informer caches to sync")
	}
	r.logger.Info("controller caches synced")

	for i := 0; i < workers; i++ {
		go r.worker(ctx)
	}
	<-ctx.Done()
	return nil
}

func (r *Reconciler) worker(ctx context.Context) {
	for r.processNextItem(ctx) {
	}
}

func (r *Reconciler) processNextItem(ctx context.Context) bool {
	key, shutdown := r.queue.Get()
	if shutdown {
		return false
	}
	defer r.queue.Done(key)

	if err := r.reconcile(key); err != nil {
		r.logger.Error("reconcile", "service", key, "err", err)
		r.queue.AddRateLimited(key)
		return true
	}
	r.queue.Forget(key)
	return true
}

func (r *Reconciler) reconcile(key string) error {
	ns, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return nil // malformed key: nothing we can do, don't requeue forever
	}

	svc, err := r.serviceLister.Services(ns).Get(name)
	if errors.IsNotFound(err) {
		r.forgetLocalWarning(key)
		return r.removeAllFor(key, nil)
	}
	if err != nil {
		return err
	}
	if !r.managed(svc) {
		return r.removeAllFor(key, nil)
	}

	slices, err := r.sliceLister.EndpointSlices(ns).List(labels.SelectorFromSet(labels.Set{
		serviceNameLabel: name,
	}))
	if err != nil {
		return fmt.Errorf("list endpointslices: %w", err)
	}

	r.noteLocalPolicy(key, svc)
	desired, err := buildDesiredVIPs(svc, slices, buildOptions{LocalNode: r.localPolicy.Node})
	if err != nil {
		return err
	}
	return r.removeAllFor(key, desired)
}

// forgetLocalWarning lets a re-created Service of the same name be warned again.
func (r *Reconciler) forgetLocalWarning(key string) {
	r.mu.Lock()
	delete(r.warnedLocal, key)
	r.mu.Unlock()
}

// noteLocalPolicy warns, once per Service, that externalTrafficPolicy: Local is
// being treated as Cluster (the node isn't set up to honour it safely). Traffic
// still works, since the NAT preserves the client address either way; what is lost
// is only the "deliver on the node that has the pod" locality.
func (r *Reconciler) noteLocalPolicy(key string, svc *corev1.Service) {
	if r.localPolicy.Node != "" || svc.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal {
		return
	}
	r.mu.Lock()
	already := r.warnedLocal[key]
	r.warnedLocal[key] = true
	r.mu.Unlock()
	if already {
		return
	}
	reason := r.localPolicy.NotHonouredReason
	if reason == "" {
		reason = "this node is not configured to honour it"
	}
	r.logger.Warn("externalTrafficPolicy: Local is being treated as Cluster for this Service", "service", key, "why", reason)
}

// removeAllFor applies desired's VIP set for key, upserting each one and
// removing whatever was previously installed for key but is no longer
// desired (nil desired removes everything — the Service is gone or no
// longer managed). Named for its common case (desired == nil, a pure
// teardown) but handles both; changed is used to fire OnChange at most
// once per reconcile.
func (r *Reconciler) removeAllFor(key string, desired []desiredVIP) error {
	changed := false
	newKeys := make(map[string]bool, len(desired))

	for _, d := range desired {
		if err := r.plane.UpsertVIP(d.VIP); err != nil {
			return fmt.Errorf("upsert vip %s:%d: %w", d.VIP.Address, d.VIP.Port, err)
		}
		changed = true
		vipKey := dataplane.VIPKey(d.VIP)
		newKeys[vipKey] = true
		for _, b := range d.DrainingBackends {
			if err := r.plane.SetBackendDrainingByKey(vipKey, dataplane.BackendKey(b), true); err != nil {
				return fmt.Errorf("set backend draining %s:%d: %w", b.Address, b.Port, err)
			}
		}
	}

	r.mu.Lock()
	previous := append([]string(nil), r.installed[key]...)
	r.mu.Unlock()
	for _, oldKey := range previous {
		if newKeys[oldKey] {
			continue
		}
		if err := r.plane.RemoveVIP(oldKey); err != nil {
			return fmt.Errorf("remove vip %s: %w", oldKey, err)
		}
		changed = true
	}

	r.mu.Lock()
	if len(newKeys) == 0 {
		delete(r.installed, key)
	} else {
		keys := make([]string, 0, len(newKeys))
		for k := range newKeys {
			keys = append(keys, k)
		}
		r.installed[key] = keys
	}
	r.mu.Unlock()

	if changed && r.OnChange != nil {
		r.OnChange()
	}
	return nil
}

func (r *Reconciler) managed(svc *corev1.Service) bool {
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		return false
	}
	class := ""
	if svc.Spec.LoadBalancerClass != nil {
		class = *svc.Spec.LoadBalancerClass
	}
	return class == r.lbClass
}
