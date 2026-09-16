// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package ipamctrl is cmd/rivora-controller's reconcile logic: watch
// AddressPool and Service(type=LoadBalancer), feed pool definitions into
// internal/ipam's Allocator, and keep each managed Service's
// Status.LoadBalancer.Ingress[].IP in sync with what the allocator assigned
// it. Unlike internal/controller (rivorad's per-node dataplane mirror,
// running unleadered on every node), this is a single cluster-wide writer
// to Service.Status and so is meant to run behind leader election — see
// cmd/rivora-controller/main.go.
package ipamctrl

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/zyvorai/rivora/api/v1alpha1"
	"github.com/zyvorai/rivora/internal/ipam"
)

// Finalizer gates Service deletion until its address is released back to
// the allocator — without it, a delete could remove the Service (and with
// it, our only record of which address it held) before we've freed that
// address, leaking it.
const (
	Finalizer      = "rivora.zyvor.dev/ipam"
	poolAnnotation = "rivora.zyvor.dev/address-pool"
	resyncPeriod   = 30 * time.Second
)

// Reconciler is cmd/rivora-controller's control loop.
type Reconciler struct {
	clientset kubernetes.Interface
	dynamic   dynamic.Interface
	logger    *slog.Logger
	lbClass   string

	serviceLister corelisters.ServiceLister
	allocator     *ipam.Allocator

	queue      workqueue.TypedRateLimitingInterface[string]
	poolsQueue workqueue.TypedRateLimitingInterface[string]
}

// New builds a Reconciler. lbClass must match internal/controller's (the
// per-node reconciler) — both need to agree on which Services either side
// owns.
func New(clientset kubernetes.Interface, dyn dynamic.Interface, lbClass string, logger *slog.Logger) (*Reconciler, informers.SharedInformerFactory, dynamicinformer.DynamicSharedInformerFactory) {
	factory := informers.NewSharedInformerFactory(clientset, resyncPeriod)
	dynFactory := dynamicinformer.NewDynamicSharedInformerFactory(dyn, resyncPeriod)

	svcInformer := factory.Core().V1().Services()
	poolInformer := dynFactory.ForResource(v1alpha1.AddressPoolResource)

	r := &Reconciler{
		clientset:     clientset,
		dynamic:       dyn,
		logger:        logger,
		lbClass:       lbClass,
		serviceLister: svcInformer.Lister(),
		allocator:     ipam.NewAllocator(),
		queue: workqueue.NewTypedRateLimitingQueue[string](
			workqueue.DefaultTypedControllerRateLimiter[string](),
		),
		poolsQueue: workqueue.NewTypedRateLimitingQueue[string](
			workqueue.DefaultTypedControllerRateLimiter[string](),
		),
	}

	svcInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { r.enqueueService(obj) },
		UpdateFunc: func(_, obj interface{}) { r.enqueueService(obj) },
		DeleteFunc: func(obj interface{}) { r.enqueueService(obj) },
	})
	poolInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(interface{}) { r.poolsQueue.Add("pools") },
		UpdateFunc: func(interface{}, interface{}) { r.poolsQueue.Add("pools") },
		DeleteFunc: func(interface{}) { r.poolsQueue.Add("pools") },
	})

	return r, factory, dynFactory
}

func (r *Reconciler) enqueueService(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		return
	}
	r.queue.Add(key)
}

// Run starts both informer factories, waits for their caches to sync,
// rebuilds the allocator's in-memory state from every already-assigned
// Service (so a restart doesn't forget live allocations and double-assign
// their addresses), and then serves both work queues until ctx is
// cancelled. Intended to run only on the elected leader.
func (r *Reconciler) Run(ctx context.Context, factory informers.SharedInformerFactory, dynFactory dynamicinformer.DynamicSharedInformerFactory, workers int) error {
	defer r.queue.ShutDown()
	defer r.poolsQueue.ShutDown()

	factory.Start(ctx.Done())
	dynFactory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(),
		factory.Core().V1().Services().Informer().HasSynced,
		dynFactory.ForResource(v1alpha1.AddressPoolResource).Informer().HasSynced,
	) {
		return fmt.Errorf("timed out waiting for informer caches to sync")
	}
	r.logger.Info("ipam controller caches synced")

	if err := r.reconcilePools(ctx, dynFactory); err != nil {
		return fmt.Errorf("initial pool sync: %w", err)
	}
	if err := r.rebuildFromExistingServices(ctx); err != nil {
		return fmt.Errorf("rebuild allocator state: %w", err)
	}

	go r.poolWorker(ctx, dynFactory)
	for i := 0; i < workers; i++ {
		go r.serviceWorker(ctx)
	}
	<-ctx.Done()
	return nil
}

func (r *Reconciler) poolWorker(ctx context.Context, dynFactory dynamicinformer.DynamicSharedInformerFactory) {
	for {
		key, shutdown := r.poolsQueue.Get()
		if shutdown {
			return
		}
		if err := r.reconcilePools(ctx, dynFactory); err != nil {
			r.logger.Error("reconcile address pools", "err", err)
			r.poolsQueue.AddRateLimited(key)
			r.poolsQueue.Done(key)
			continue
		}
		r.poolsQueue.Forget(key)
		r.poolsQueue.Done(key)
	}
}

// reconcilePools rebuilds the allocator's full pool set from every current
// AddressPool object. Expansion failures (a malformed CIDR, IPv6, etc.) are
// logged and that one pool is skipped rather than failing the whole sync —
// one bad AddressPool shouldn't take every other pool's addresses offline.
func (r *Reconciler) reconcilePools(ctx context.Context, dynFactory dynamicinformer.DynamicSharedInformerFactory) error {
	lister := dynFactory.ForResource(v1alpha1.AddressPoolResource).Lister()
	objs, err := lister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("list addresspools: %w", err)
	}

	pools := make(map[string]ipam.PoolSpec, len(objs))
	for _, obj := range objs {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		var pool v1alpha1.AddressPool
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.UnstructuredContent(), &pool); err != nil {
			r.logger.Error("decode addresspool", "err", err)
			continue
		}
		autoAssign := true
		if pool.Spec.AutoAssign != nil {
			autoAssign = *pool.Spec.AutoAssign
		}
		addrs, err := ipam.ExpandPool(pool.Spec.Addresses, pool.Spec.AvoidBuggyIPs)
		if err != nil {
			r.logger.Error("expand addresspool", "pool", pool.Name, "err", err)
			continue
		}
		pools[pool.Name] = ipam.PoolSpec{Addresses: addrs, AutoAssign: autoAssign}
	}
	r.allocator.SetPools(pools)
	r.logger.Info("address pools synced", "pools", len(pools))

	for name := range pools {
		if err := r.patchPoolStatus(ctx, name); err != nil {
			r.logger.Error("patch addresspool status", "pool", name, "err", err)
		}
	}
	return nil
}

// patchPoolStatus writes name's current Allocator.Counts into its
// AddressPool.Status — coarse counts only (not per-allocation state, which
// would make Service churn contend on this object's status subresource;
// Service objects remain the allocation source of truth, see
// rebuildFromExistingServices).
func (r *Reconciler) patchPoolStatus(ctx context.Context, name string) error {
	available, assigned := r.allocator.Counts(name)
	patch, err := json.Marshal(map[string]interface{}{
		"status": map[string]interface{}{
			"availableIPs": available,
			"assignedIPs":  assigned,
		},
	})
	if err != nil {
		return err
	}
	_, err = r.dynamic.Resource(v1alpha1.AddressPoolResource).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}, "status")
	return err
}

// rebuildFromExistingServices calls Allocator.Reserve for every managed
// Service that already has an assigned address, so a rivora-controller
// restart (or leadership handover) doesn't lose track of live allocations.
func (r *Reconciler) rebuildFromExistingServices(ctx context.Context) error {
	svcs, err := r.serviceLister.List(labels.Everything())
	if err != nil {
		return err
	}
	for _, svc := range svcs {
		if !r.managed(svc) {
			continue
		}
		ip := ingressIPv4(svc)
		if ip == "" {
			continue
		}
		key := cacheKey(svc)
		if err := r.allocator.Reserve(ip, key); err != nil {
			r.logger.Warn("reserve existing allocation", "service", key, "address", ip, "err", err)
		}
	}
	return nil
}

func (r *Reconciler) serviceWorker(ctx context.Context) {
	for r.processNextService(ctx) {
	}
}

func (r *Reconciler) processNextService(ctx context.Context) bool {
	key, shutdown := r.queue.Get()
	if shutdown {
		return false
	}
	defer r.queue.Done(key)

	if err := r.reconcileService(ctx, key); err != nil {
		r.logger.Error("reconcile service", "service", key, "err", err)
		r.queue.AddRateLimited(key)
		return true
	}
	r.queue.Forget(key)
	return true
}

func (r *Reconciler) reconcileService(ctx context.Context, key string) error {
	ns, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return nil
	}

	svc, err := r.clientset.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		// Should already have been released via the finalizer path below
		// before Kubernetes let the delete complete; Release is idempotent
		// if that already happened, and a safety net if it somehow didn't.
		r.allocator.Release(key)
		r.refreshPoolStatuses(ctx)
		return nil
	}
	if err != nil {
		return err
	}

	deleting := svc.DeletionTimestamp != nil
	hasFinalizer := containsString(svc.Finalizers, Finalizer)

	if !r.managed(svc) || deleting {
		if hasFinalizer {
			r.allocator.Release(key)
			r.refreshPoolStatuses(ctx)
			return r.patchFinalizers(ctx, svc, removeString(svc.Finalizers, Finalizer))
		}
		return nil
	}

	if !hasFinalizer {
		return r.patchFinalizers(ctx, svc, append(append([]string{}, svc.Finalizers...), Finalizer))
	}

	ip, err := r.allocator.Allocate(key, svc.Annotations[poolAnnotation], svc.Spec.LoadBalancerIP)
	if err != nil {
		return fmt.Errorf("allocate address: %w", err)
	}
	r.refreshPoolStatuses(ctx)
	if ingressIPv4(svc) == ip {
		return nil
	}
	return r.patchIngress(ctx, svc, ip)
}

// refreshPoolStatuses re-patches every known pool's status counts. Called
// after an allocation or release so AddressPool.Status reflects the change
// promptly rather than waiting for the next periodic pool resync; errors
// are logged, not returned — a stale status count is a display nit, not
// worth failing (and requeuing) the Service reconcile over.
func (r *Reconciler) refreshPoolStatuses(ctx context.Context) {
	for _, name := range r.allocator.PoolNames() {
		if err := r.patchPoolStatus(ctx, name); err != nil {
			r.logger.Error("patch addresspool status", "pool", name, "err", err)
		}
	}
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

func (r *Reconciler) patchFinalizers(ctx context.Context, svc *corev1.Service, finalizers []string) error {
	patch, err := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{"finalizers": finalizers},
	})
	if err != nil {
		return err
	}
	_, err = r.clientset.CoreV1().Services(svc.Namespace).Patch(ctx, svc.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

func (r *Reconciler) patchIngress(ctx context.Context, svc *corev1.Service, ip string) error {
	patch, err := json.Marshal(map[string]interface{}{
		"status": map[string]interface{}{
			"loadBalancer": map[string]interface{}{
				"ingress": []map[string]interface{}{{"ip": ip}},
			},
		},
	})
	if err != nil {
		return err
	}
	_, err = r.clientset.CoreV1().Services(svc.Namespace).Patch(ctx, svc.Name, types.MergePatchType, patch, metav1.PatchOptions{}, "status")
	return err
}

func cacheKey(svc *corev1.Service) string {
	return svc.Namespace + "/" + svc.Name
}

func ingressIPv4(svc *corev1.Service) string {
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		if ing.IP != "" {
			return ing.IP
		}
	}
	return ""
}

func containsString(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func removeString(ss []string, s string) []string {
	out := make([]string, 0, len(ss))
	for _, v := range ss {
		if v != s {
			out = append(out, v)
		}
	}
	return out
}
