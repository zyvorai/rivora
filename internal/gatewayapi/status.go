// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package gatewayapi

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	gwapi "github.com/zyvorai/rivora/api/gatewayapi"
)

// StatusReconciler reports, on the objects themselves, what the dataplane reconciler decided: for each
// route, per parentRef, whether a managed Gateway Accepted it and whether its backendRefs ResolvedRefs;
// for each Gateway, per listener, the kinds it serves and how many routes are attached. It runs in
// rivora-controller (one leader-elected writer): the per-node reconcilers in rivorad program the
// dataplane but must not each write status. It reads the world through the same attach and reference
// checks rivorad uses, so what it reports is what rivorad programs.
//
// Several controllers may share a route, each owning the status.parents entries that carry its own
// controllerName. This writer only ever replaces its own.
type StatusReconciler struct {
	dyn    dynamic.Interface
	logger *slog.Logger

	classLister   cache.GenericLister
	gatewayLister cache.GenericLister
	tcpLister     cache.GenericLister
	udpLister     cache.GenericLister
	grantLister   cache.GenericLister
	nsLister      corelisters.NamespaceLister
	svcLister     corelisters.ServiceLister

	queue workqueue.TypedRateLimitingInterface[string] // "namespace/name" of a Gateway
}

// NewStatusReconciler builds the reconciler and the informer factories the caller must start.
func NewStatusReconciler(clientset kubernetes.Interface, dyn dynamic.Interface, logger *slog.Logger) (*StatusReconciler, informers.SharedInformerFactory, dynamicinformer.DynamicSharedInformerFactory) {
	factory := informers.NewSharedInformerFactory(clientset, resyncPeriod)
	dynFactory := dynamicinformer.NewDynamicSharedInformerFactory(dyn, resyncPeriod)

	classInf := dynFactory.ForResource(gwapi.GatewayClassResource)
	gwInf := dynFactory.ForResource(gwapi.GatewayResource)
	tcpInf := dynFactory.ForResource(gwapi.TCPRouteResource)
	udpInf := dynFactory.ForResource(gwapi.UDPRouteResource)
	grantInf := dynFactory.ForResource(gwapi.ReferenceGrantResource)
	nsInf := factory.Core().V1().Namespaces()
	svcInf := factory.Core().V1().Services()

	r := &StatusReconciler{
		dyn: dyn, logger: logger,
		classLister: classInf.Lister(), gatewayLister: gwInf.Lister(),
		tcpLister: tcpInf.Lister(), udpLister: udpInf.Lister(), grantLister: grantInf.Lister(),
		nsLister: nsInf.Lister(), svcLister: svcInf.Lister(),
		queue: workqueue.NewTypedRateLimitingQueue[string](workqueue.DefaultTypedControllerRateLimiter[string]()),
	}

	gwEvent := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(o interface{}) { r.enqueueObj(o) },
		UpdateFunc: func(_, o interface{}) { r.enqueueObj(o) },
		DeleteFunc: func(o interface{}) { r.enqueueObj(o) },
	}
	gwInf.Informer().AddEventHandler(gwEvent)
	routeEvent := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(o interface{}) { r.enqueueRouteParents(o) },
		UpdateFunc: func(_, o interface{}) { r.enqueueRouteParents(o) },
		DeleteFunc: func(o interface{}) { r.enqueueRouteParents(o) },
	}
	tcpInf.Informer().AddEventHandler(routeEvent)
	udpInf.Informer().AddEventHandler(routeEvent)
	// Anything that can change what is allowed anywhere: resync every Gateway.
	resync := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(interface{}) { r.enqueueAll() },
		UpdateFunc: func(interface{}, interface{}) { r.enqueueAll() },
		DeleteFunc: func(interface{}) { r.enqueueAll() },
	}
	classInf.Informer().AddEventHandler(resync)
	grantInf.Informer().AddEventHandler(resync)
	nsInf.Informer().AddEventHandler(resync)
	// A Service appearing or vanishing changes ResolvedRefs (BackendNotFound) for the routes using it.
	svcInf.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(interface{}) { r.enqueueAll() },
		DeleteFunc: func(interface{}) { r.enqueueAll() },
	})
	return r, factory, dynFactory
}

func (r *StatusReconciler) enqueueObj(obj interface{}) {
	if key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj); err == nil {
		r.queue.Add(key)
	}
}

func (r *StatusReconciler) enqueueRouteParents(obj interface{}) {
	u, rt, ok := decodeRoute(obj)
	if !ok {
		return
	}
	rr := RouteRef{Namespace: u.GetNamespace()}
	for _, pr := range rt.Spec.ParentRefs {
		r.queue.Add(parentNamespace(rr, pr) + "/" + pr.Name)
	}
}

func (r *StatusReconciler) enqueueAll() {
	objs, err := r.gatewayLister.List(labels.Everything())
	if err != nil {
		return
	}
	for _, o := range objs {
		r.enqueueObj(o)
	}
}

// Run starts the informers, waits for them, and serves the queue until ctx is done.
func (r *StatusReconciler) Run(ctx context.Context, factory informers.SharedInformerFactory, dynFactory dynamicinformer.DynamicSharedInformerFactory, workers int) error {
	defer r.queue.ShutDown()
	factory.Start(ctx.Done())
	dynFactory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(),
		dynFactory.ForResource(gwapi.GatewayClassResource).Informer().HasSynced,
		dynFactory.ForResource(gwapi.GatewayResource).Informer().HasSynced,
		dynFactory.ForResource(gwapi.TCPRouteResource).Informer().HasSynced,
		dynFactory.ForResource(gwapi.UDPRouteResource).Informer().HasSynced,
		dynFactory.ForResource(gwapi.ReferenceGrantResource).Informer().HasSynced,
		factory.Core().V1().Namespaces().Informer().HasSynced,
		factory.Core().V1().Services().Informer().HasSynced,
	) {
		return fmt.Errorf("timed out waiting for informer caches to sync")
	}
	r.logger.Info("gateway API status writer caches synced")
	for i := 0; i < workers; i++ {
		go func() {
			for r.processNext(ctx) {
			}
		}()
	}
	<-ctx.Done()
	return nil
}

func (r *StatusReconciler) processNext(ctx context.Context) bool {
	key, shutdown := r.queue.Get()
	if shutdown {
		return false
	}
	defer r.queue.Done(key)
	if err := r.reconcile(ctx, key); err != nil {
		r.logger.Error("reconcile gateway status", "gateway", key, "err", err)
		r.queue.AddRateLimited(key)
		return true
	}
	r.queue.Forget(key)
	return true
}

// Env: the same three lookups the dataplane reconciler makes.
func (r *StatusReconciler) NamespaceLabels(ns string) (map[string]string, bool) {
	n, err := r.nsLister.Get(ns)
	if err != nil {
		return nil, false
	}
	return n.Labels, true
}
func (r *StatusReconciler) Grants(ns string) []gwapi.ReferenceGrant {
	return grantsIn(r.grantLister, ns)
}
func (r *StatusReconciler) ServiceExists(ns, name string) bool {
	_, err := r.svcLister.Services(ns).Get(name)
	return err == nil
}

var _ Env = (*StatusReconciler)(nil)

func (r *StatusReconciler) managed(className string) bool {
	obj, err := r.classLister.Get(className)
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

// routesFor lists every route, in any namespace, with a parentRef naming gw.
func (r *StatusReconciler) routesFor(gw *gwapi.Gateway) ([]RouteRef, error) {
	var out []RouteRef
	for _, src := range []struct {
		l    cache.GenericLister
		kind string
	}{{r.tcpLister, gwapi.KindTCPRoute}, {r.udpLister, gwapi.KindUDPRoute}} {
		objs, err := src.l.List(labels.Everything())
		if err != nil {
			return nil, err
		}
		for _, o := range objs {
			u, ok := o.(*unstructured.Unstructured)
			if !ok {
				continue
			}
			var rt routeShape
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.UnstructuredContent(), &rt); err != nil {
				continue
			}
			ref := RouteRef{Kind: src.kind, Namespace: u.GetNamespace(), Name: u.GetName(), Spec: rt.Spec}
			for _, pr := range rt.Spec.ParentRefs {
				if namesGateway(ref, pr, gw) {
					out = append(out, ref)
					break
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Namespace+"/"+out[i].Kind+"/"+out[i].Name < out[j].Namespace+"/"+out[j].Kind+"/"+out[j].Name
	})
	return out, nil
}

// resolvedRefs summarises a route's backendRefs: True if every one is usable, else False with the reason
// of the most serious failure (a forbidden reference over a wrong kind over a missing Service).
func (r *StatusReconciler) resolvedRefs(rr RouteRef) (bool, string, string) {
	rank := map[string]int{gwapi.ReasonRefNotPermitted: 3, gwapi.ReasonInvalidKind: 2, gwapi.ReasonBackendNotFound: 1}
	bestReason, bestMsg, best := "", "", 0
	for _, rule := range rr.Spec.Rules {
		for _, br := range rule.BackendRefs {
			if out := CheckBackendRef(rr, br, r); !out.Resolved && rank[out.Reason] > best {
				best, bestReason, bestMsg = rank[out.Reason], out.Reason, out.Message
			}
		}
	}
	if best == 0 {
		return true, gwapi.ReasonResolvedRefs, "all backendRefs resolved"
	}
	return false, bestReason, bestMsg
}

// reconcile writes the status of Gateway key and of every route attached to it.
func (r *StatusReconciler) reconcile(ctx context.Context, key string) error {
	ns, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return nil
	}
	obj, err := r.gatewayLister.ByNamespace(ns).Get(name)
	if apierrors.IsNotFound(err) {
		return nil // routes keep their (now stale) parents entry until they are deleted or re-parented
	}
	if err != nil {
		return err
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("gateway %s: unexpected object type %T", key, obj)
	}
	var gw gwapi.Gateway
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.UnstructuredContent(), &gw); err != nil {
		return fmt.Errorf("gateway %s: decode: %w", key, err)
	}
	if !r.managed(gw.Spec.GatewayClassName) {
		return nil
	}

	routes, err := r.routesFor(&gw)
	if err != nil {
		return err
	}

	attached := map[string]int32{} // listener name -> routes attached
	var firstErr error
	for _, rr := range routes {
		resolved, refReason, refMsg := r.resolvedRefs(rr)
		var parents []parentResult
		for _, pr := range rr.Spec.ParentRefs {
			if !namesGateway(rr, pr, &gw) {
				continue
			}
			out := EvaluateParent(&gw, rr, pr, r)
			for _, l := range out.Listeners {
				attached[l]++
			}
			parents = append(parents, parentResult{pr: pr, out: out, resolved: resolved, refReason: refReason, refMsg: refMsg})
		}
		if err := r.writeRouteStatus(ctx, &gw, rr, parents); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := r.writeGatewayListeners(ctx, &gw, attached); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

type parentResult struct {
	pr        gwapi.ParentRef
	out       ParentOutcome
	resolved  bool
	refReason string
	refMsg    string
}

// writeRouteStatus replaces this controller's parents entries for the parentRefs that name gw, leaving
// its entries for other Gateways and every other controller's entries as they are.
func (r *StatusReconciler) writeRouteStatus(ctx context.Context, gw *gwapi.Gateway, rr RouteRef, results []parentResult) error {
	gvr := gwapi.TCPRouteResource
	if rr.Kind == gwapi.KindUDPRoute {
		gvr = gwapi.UDPRouteResource
	}
	cur, err := r.dyn.Resource(gvr).Namespace(rr.Namespace).Get(ctx, rr.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var st gwapi.RouteStatus
	if raw, found, _ := unstructured.NestedMap(cur.Object, "status"); found {
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &st); err != nil {
			return fmt.Errorf("route %s/%s: decode status: %w", rr.Namespace, rr.Name, err)
		}
	}

	names := func(pr gwapi.ParentRef) bool { return namesGateway(rr, pr, gw) }
	var keep []gwapi.RouteParentStatus
	old := map[string]gwapi.RouteParentStatus{}
	for _, p := range st.Parents {
		if p.ControllerName == gwapi.ControllerName && names(p.ParentRef) {
			old[parentKey(rr, p.ParentRef)] = p
			continue // replaced below
		}
		keep = append(keep, p)
	}
	gen := cur.GetGeneration()
	next := append([]gwapi.RouteParentStatus(nil), keep...)
	for _, res := range results {
		conds := old[parentKey(rr, res.pr)].Conditions
		set := func(t string, ok bool, okReason, badReason, msg string) {
			status, reason := metav1.ConditionTrue, okReason
			if !ok {
				status, reason = metav1.ConditionFalse, badReason
			}
			meta.SetStatusCondition(&conds, metav1.Condition{Type: t, Status: status, Reason: reason, Message: msg, ObservedGeneration: gen})
		}
		set(gwapi.ConditionAccepted, res.out.Accepted, gwapi.ReasonAccepted, res.out.Reason, res.out.Message)
		set(gwapi.ConditionResolvedRefs, res.resolved, gwapi.ReasonResolvedRefs, res.refReason, res.refMsg)
		next = append(next, gwapi.RouteParentStatus{ParentRef: res.pr, ControllerName: gwapi.ControllerName, Conditions: conds})
	}
	if equalParents(st.Parents, next) {
		return nil
	}

	statusMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&gwapi.RouteStatus{Parents: next})
	if err != nil {
		return err
	}
	out := cur.DeepCopy()
	out.Object["status"] = statusMap
	_, err = r.dyn.Resource(gvr).Namespace(rr.Namespace).UpdateStatus(ctx, out, metav1.UpdateOptions{})
	return err
}

func parentKey(rr RouteRef, pr gwapi.ParentRef) string {
	sec := ""
	if pr.SectionName != nil {
		sec = *pr.SectionName
	}
	return parentNamespace(rr, pr) + "/" + pr.Name + "/" + sec
}

// equalParents compares parents ignoring condition timestamps and order of entries.
func equalParents(a, b []gwapi.RouteParentStatus) bool {
	norm := func(in []gwapi.RouteParentStatus) []gwapi.RouteParentStatus {
		out := make([]gwapi.RouteParentStatus, len(in))
		for i, p := range in {
			c := make([]metav1.Condition, len(p.Conditions))
			for j, cond := range p.Conditions {
				cond.LastTransitionTime = metav1.Time{}
				c[j] = cond
			}
			sort.Slice(c, func(x, y int) bool { return c[x].Type < c[y].Type })
			p.Conditions = c
			out[i] = p
		}
		sort.Slice(out, func(i, j int) bool {
			return fmt.Sprint(out[i].ControllerName, out[i].ParentRef) < fmt.Sprint(out[j].ControllerName, out[j].ParentRef)
		})
		return out
	}
	return reflect.DeepEqual(norm(a), norm(b))
}

// writeGatewayListeners fills status.listeners: per listener, the route kinds it serves, how many routes
// are attached, and Accepted/ResolvedRefs conditions. It leaves the rest of the Gateway's status alone.
func (r *StatusReconciler) writeGatewayListeners(ctx context.Context, gw *gwapi.Gateway, attached map[string]int32) error {
	oldByName := map[string]gwapi.ListenerStatus{}
	for _, l := range gw.Status.Listeners {
		oldByName[l.Name] = l
	}
	var next []gwapi.ListenerStatus
	for _, l := range gw.Spec.Listeners {
		var kinds []gwapi.RouteGroupKind
		accepted, reason, msg := true, gwapi.ReasonListenerAccepted, "the listener is accepted"
		if kind, ok := KindForProtocol(l.Protocol); ok {
			group := gwapi.GroupName
			kinds = []gwapi.RouteGroupKind{{Group: &group, Kind: kind}}
		} else {
			accepted, reason, msg = false, gwapi.ReasonUnsupportedProtocol, fmt.Sprintf("protocol %q is not TCP or UDP", l.Protocol)
		}
		if kinds == nil {
			kinds = []gwapi.RouteGroupKind{}
		}
		conds := oldByName[l.Name].Conditions
		status := func(ok bool) metav1.ConditionStatus {
			if ok {
				return metav1.ConditionTrue
			}
			return metav1.ConditionFalse
		}
		meta.SetStatusCondition(&conds, metav1.Condition{Type: gwapi.ConditionAccepted, Status: status(accepted), Reason: reason, Message: msg, ObservedGeneration: gw.Generation})
		meta.SetStatusCondition(&conds, metav1.Condition{Type: gwapi.ConditionResolvedRefs, Status: metav1.ConditionTrue, Reason: gwapi.ReasonListenerResolvedRefs, Message: "no references to resolve", ObservedGeneration: gw.Generation})
		next = append(next, gwapi.ListenerStatus{Name: l.Name, SupportedKinds: kinds, AttachedRoutes: attached[l.Name], Conditions: conds})
	}
	if equalListeners(gw.Status.Listeners, next) {
		return nil
	}

	cur, err := r.dyn.Resource(gwapi.GatewayResource).Namespace(gw.Namespace).Get(ctx, gw.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	lst := make([]interface{}, len(next))
	for i, l := range next {
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&l)
		if err != nil {
			return err
		}
		lst[i] = m
	}
	out := cur.DeepCopy()
	if err := unstructured.SetNestedSlice(out.Object, lst, "status", "listeners"); err != nil {
		return err
	}
	_, err = r.dyn.Resource(gwapi.GatewayResource).Namespace(gw.Namespace).UpdateStatus(ctx, out, metav1.UpdateOptions{})
	return err
}

func equalListeners(a, b []gwapi.ListenerStatus) bool {
	norm := func(in []gwapi.ListenerStatus) []gwapi.ListenerStatus {
		out := make([]gwapi.ListenerStatus, len(in))
		for i, l := range in {
			c := make([]metav1.Condition, len(l.Conditions))
			for j, cond := range l.Conditions {
				cond.LastTransitionTime = metav1.Time{}
				c[j] = cond
			}
			sort.Slice(c, func(x, y int) bool { return c[x].Type < c[y].Type })
			l.Conditions = c
			if l.SupportedKinds == nil {
				l.SupportedKinds = []gwapi.RouteGroupKind{}
			}
			out[i] = l
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out
	}
	return reflect.DeepEqual(norm(a), norm(b))
}
