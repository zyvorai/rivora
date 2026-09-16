// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// gateway.go extends Reconciler with Gateway API IPAM: watching
// GatewayClass/Gateway and assigning addresses to managed Gateways via
// the same *ipam.Allocator Service consumers already use. Only active
// when New was called with enableGatewayAPI — see initGatewayAPI.
package ipamctrl

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	gwapi "github.com/zyvorai/rivora/api/gatewayapi"
)

func (r *Reconciler) initGatewayAPI(dynFactory dynamicinformer.DynamicSharedInformerFactory) {
	classInformer := dynFactory.ForResource(gwapi.GatewayClassResource)
	gwInformer := dynFactory.ForResource(gwapi.GatewayResource)

	r.gatewayAPI = &gatewayIPAM{
		classLister:   classInformer.Lister(),
		gatewayLister: gwInformer.Lister(),
		queue: workqueue.NewTypedRateLimitingQueue[string](
			workqueue.DefaultTypedControllerRateLimiter[string](),
		),
	}

	gwInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { r.enqueueGateway(obj) },
		UpdateFunc: func(_, obj interface{}) { r.enqueueGateway(obj) },
		DeleteFunc: func(obj interface{}) { r.enqueueGateway(obj) },
	})
	// A GatewayClass change (created/deleted/controllerName edited) can
	// flip which Gateways are managed — resync every known Gateway,
	// mirroring internal/gatewayapi's own classQueue/classWorker for the
	// identical reason (rare event, few enough Gateways that a full
	// resync is simpler than a reverse index).
	classInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(interface{}) { r.resyncAllGateways() },
		UpdateFunc: func(interface{}, interface{}) { r.resyncAllGateways() },
		DeleteFunc: func(interface{}) { r.resyncAllGateways() },
	})
}

func (r *Reconciler) resyncAllGateways() {
	objs, err := r.gatewayAPI.gatewayLister.List(labels.Everything())
	if err != nil {
		return
	}
	for _, obj := range objs {
		if key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj); err == nil {
			r.gatewayAPI.queue.Add(key)
		}
	}
}

func (r *Reconciler) enqueueGateway(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		return
	}
	r.gatewayAPI.queue.Add(key)
}

func (r *Reconciler) gatewayWorker(ctx context.Context) {
	for r.processNextGateway(ctx) {
	}
}

func (r *Reconciler) processNextGateway(ctx context.Context) bool {
	key, shutdown := r.gatewayAPI.queue.Get()
	if shutdown {
		return false
	}
	defer r.gatewayAPI.queue.Done(key)

	if err := r.reconcileGateway(ctx, key); err != nil {
		r.logger.Error("reconcile gateway", "gateway", key, "err", err)
		r.gatewayAPI.queue.AddRateLimited(key)
		return true
	}
	r.gatewayAPI.queue.Forget(key)
	return true
}

// reconcileGateway mirrors reconcileService closely: finalizer-gated
// allocate/release against the shared allocator, patching
// Gateway.status.addresses instead of Service.Status.LoadBalancer.
// Ingress[].IP.
func (r *Reconciler) reconcileGateway(ctx context.Context, key string) error {
	ns, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return nil
	}

	// A direct API read, not the cached gatewayLister — matching
	// reconcileService's r.clientset...Get() precisely and for the same
	// reason: reconcileGateway calls itself again on requeue right after
	// patching this same object (finalizer add, then allocate+status),
	// and the informer cache only updates asynchronously once its watch
	// observes that write, so reading through it here could see a stale
	// pre-patch version. The lister is still the right tool for
	// list/enqueue paths (resyncAllGateways, rebuildFromExistingGateways)
	// where staleness just means "catch it on the next event," not a
	// same-key read-after-write.
	u, err := r.dynamic.Resource(gwapi.GatewayResource).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		r.allocator.Release(key)
		r.refreshPoolStatuses(ctx)
		return nil
	}
	if err != nil {
		return err
	}
	var gw gwapi.Gateway
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.UnstructuredContent(), &gw); err != nil {
		return fmt.Errorf("gateway %s: decode: %w", key, err)
	}

	deleting := gw.DeletionTimestamp != nil
	hasFinalizer := containsString(gw.Finalizers, Finalizer)
	managed := r.managedGatewayClass(gw.Spec.GatewayClassName)

	if !managed || deleting {
		if hasFinalizer {
			r.allocator.Release(key)
			r.refreshPoolStatuses(ctx)
			return r.patchGatewayFinalizers(ctx, &gw, removeString(gw.Finalizers, Finalizer))
		}
		return nil
	}

	if !hasFinalizer {
		return r.patchGatewayFinalizers(ctx, &gw, append(append([]string{}, gw.Finalizers...), Finalizer))
	}

	pinnedIP := ""
	if len(gw.Spec.Addresses) > 0 {
		pinnedIP = gw.Spec.Addresses[0].Value
	}
	ip, err := r.allocator.Allocate(key, gw.Annotations[poolAnnotation], pinnedIP)
	if err != nil {
		return fmt.Errorf("allocate address: %w", err)
	}
	r.refreshPoolStatuses(ctx)

	if err := r.patchGatewayClassAccepted(ctx, gw.Spec.GatewayClassName); err != nil {
		r.logger.Error("patch gatewayclass status", "class", gw.Spec.GatewayClassName, "err", err)
	}

	if gatewayAssignedIPv4(&gw) == ip {
		return nil
	}
	return r.patchGatewayStatus(ctx, &gw, ip)
}

func (r *Reconciler) managedGatewayClass(className string) bool {
	obj, err := r.gatewayAPI.classLister.Get(className)
	if err != nil {
		return false
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return false
	}
	var gc gwapi.GatewayClass
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.UnstructuredContent(), &gc); err != nil {
		return false
	}
	return gwapi.IsManagedClass(&gc)
}

func gatewayAssignedIPv4(gw *gwapi.Gateway) string {
	for _, a := range gw.Status.Addresses {
		if a.Value != "" {
			return a.Value
		}
	}
	return ""
}

func (r *Reconciler) patchGatewayFinalizers(ctx context.Context, gw *gwapi.Gateway, finalizers []string) error {
	patch, err := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{"finalizers": finalizers},
	})
	if err != nil {
		return err
	}
	_, err = r.dynamic.Resource(gwapi.GatewayResource).Namespace(gw.Namespace).Patch(ctx, gw.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// patchGatewayStatus writes the assigned address plus a minimal Accepted/
// Programmed condition pair — enough for `kubectl get gateway` to show
// sane status, not a full Gateway API conformance-suite implementation
// (no per-listener status, no detailed condition reasons beyond the
// happy path).
func (r *Reconciler) patchGatewayStatus(ctx context.Context, gw *gwapi.Gateway, ip string) error {
	now := metav1.Now().UTC().Format(time.RFC3339)
	condition := func(condType string) map[string]interface{} {
		return map[string]interface{}{
			"type":               condType,
			"status":             "True",
			"reason":             condType,
			"message":            "Managed by rivora.zyvor.dev",
			"lastTransitionTime": now,
			"observedGeneration": gw.Generation,
		}
	}
	patch, err := json.Marshal(map[string]interface{}{
		"status": map[string]interface{}{
			"addresses": []map[string]interface{}{{"type": "IPAddress", "value": ip}},
			"conditions": []map[string]interface{}{
				condition("Accepted"),
				condition("Programmed"),
			},
		},
	})
	if err != nil {
		return err
	}
	_, err = r.dynamic.Resource(gwapi.GatewayResource).Namespace(gw.Namespace).Patch(ctx, gw.Name, types.MergePatchType, patch, metav1.PatchOptions{}, "status")
	return err
}

// patchGatewayClassAccepted writes a one-time Accepted condition onto the
// GatewayClass itself, the same minimal-but-sane-status contract
// patchGatewayStatus documents.
func (r *Reconciler) patchGatewayClassAccepted(ctx context.Context, className string) error {
	now := metav1.Now().UTC().Format(time.RFC3339)
	patch, err := json.Marshal(map[string]interface{}{
		"status": map[string]interface{}{
			"conditions": []map[string]interface{}{{
				"type":               "Accepted",
				"status":             "True",
				"reason":             "Accepted",
				"message":            "Managed by rivora.zyvor.dev",
				"lastTransitionTime": now,
			}},
		},
	})
	if err != nil {
		return err
	}
	_, err = r.dynamic.Resource(gwapi.GatewayClassResource).Patch(ctx, className, types.MergePatchType, patch, metav1.PatchOptions{}, "status")
	return err
}

// rebuildFromExistingGateways calls Allocator.Reserve for every managed
// Gateway that already has an assigned address, mirroring
// rebuildFromExistingServices — a restart (or leadership handover)
// mustn't forget live allocations and double-assign their addresses.
func (r *Reconciler) rebuildFromExistingGateways(ctx context.Context) error {
	objs, err := r.gatewayAPI.gatewayLister.List(labels.Everything())
	if err != nil {
		return err
	}
	for _, obj := range objs {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		var gw gwapi.Gateway
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.UnstructuredContent(), &gw); err != nil {
			continue
		}
		if !r.managedGatewayClass(gw.Spec.GatewayClassName) {
			continue
		}
		ip := gatewayAssignedIPv4(&gw)
		if ip == "" {
			continue
		}
		key := gw.Namespace + "/" + gw.Name
		if err := r.allocator.Reserve(ip, key); err != nil {
			r.logger.Warn("reserve existing gateway allocation", "gateway", key, "address", ip, "err", err)
		}
	}
	return nil
}
