// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package gatewayapi is rivorad's second, parallel Kubernetes reconciler:
// it watches GatewayClass/Gateway/TCPRoute/UDPRoute (the Gateway API's L4
// "experimental channel" resources — see api/gatewayapi's doc comment for
// why HTTPRoute is out of scope) and programs the same node-local
// Dataplane internal/controller's Service/EndpointSlice reconciler
// already writes to. A node can run Service-sourced and Gateway-sourced
// VIPs side by side — both funnel into the same dataplane.Dataplane.
// Unleadered, same reasoning as internal/controller: it only mirrors
// cluster state into this node's own BPF maps.
package gatewayapi

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	gwapi "github.com/zyvorai/rivora/api/gatewayapi"
	"github.com/zyvorai/rivora/internal/config"
	"github.com/zyvorai/rivora/internal/dataplane"
	"github.com/zyvorai/rivora/internal/initsync"
)

const (
	serviceNameLabel = "kubernetes.io/service-name"
	resyncPeriod     = 30 * time.Second
)

// dataplaner is the subset of *dataplane.Dataplane this reconciler needs
// — its own narrow interface, same as internal/controller and
// internal/speaker each independently define rather than sharing one.
type dataplaner interface {
	UpsertVIP(vip config.VIP) error
	RemoveVIP(key string) error
	SetBackendDrainingByKey(vipKey, backendKey string, draining bool) error
}

var _ dataplaner = (*dataplane.Dataplane)(nil)

// routeShape decodes just the one field (spec.parentRefs/spec.rules)
// TCPRoute and UDPRoute share identically — used for both Kinds, since
// only the GVR (which lister a given unstructured object came from)
// distinguishes them, never anything in this shape.
type routeShape struct {
	Spec gwapi.RouteSpec `json:"spec"`
}

// attachedRoute is one Route (TCPRoute or UDPRoute, indistinguishable
// past this point) known to attach to the Gateway being reconciled.
type attachedRoute struct {
	ref RouteRef
}

// Reconciler is rivorad's GatewayClass/Gateway/TCPRoute/UDPRoute ->
// Dataplane control loop.
type Reconciler struct {
	plane  dataplaner
	logger *slog.Logger

	serviceLister corelisters.ServiceLister
	sliceLister   discoverylisters.EndpointSliceLister
	classLister   cache.GenericLister
	gatewayLister cache.GenericLister
	tcpLister     cache.GenericLister
	udpLister     cache.GenericLister
	// nsLister and grantLister feed allowedRoutes namespace selectors and cross-namespace
	// backendRefs (ReferenceGrant); see the Env methods below.
	nsLister    corelisters.NamespaceLister
	grantLister cache.GenericLister

	queue      workqueue.TypedRateLimitingInterface[string] // "namespace/name" of a Gateway
	classQueue workqueue.TypedRateLimitingInterface[string] // trigger-only, mirrors ipamctrl's poolsQueue

	// installed tracks, per Gateway key, the VIP keys most recently
	// programmed for it — same role as internal/controller.Reconciler's
	// field of the same name, and guarded by mu for the same reason: different
	// Gateways reconcile on concurrent workers and unguarded writes to this map
	// crash the process ("concurrent map read and map write"). Held only around
	// the map access, never across a dataplane call.
	mu        sync.Mutex
	installed map[string][]string

	// OnChange mirrors internal/controller.Reconciler.OnChange — rivorad
	// wires both reconcilers' OnChange to the same health-checker-refresh
	// callback.
	OnChange func()

	// init, if set, is told when each Gateway present at start-up has been reconciled once.
	init *initsync.Tracker
}

// SetInitTracker makes Run report, through t, when every Gateway that existed once the caches
// synced has been reconciled successfully. Set before Run.
func (r *Reconciler) SetInitTracker(t *initsync.Tracker) { r.init = t }

// New builds a Reconciler. Unlike internal/controller there's no lbClass
// equivalent — a Gateway's "do we manage this" test is entirely
// GatewayClass.spec.controllerName == gwapi.ControllerName, not a
// user-configurable class string, matching Gateway API's own model
// (GatewayClass already *is* the class-selection mechanism).
func New(clientset kubernetes.Interface, dyn dynamic.Interface, plane dataplaner, logger *slog.Logger) (*Reconciler, informers.SharedInformerFactory, dynamicinformer.DynamicSharedInformerFactory) {
	factory := informers.NewSharedInformerFactory(clientset, resyncPeriod)
	dynFactory := dynamicinformer.NewDynamicSharedInformerFactory(dyn, resyncPeriod)

	svcInformer := factory.Core().V1().Services()
	sliceInformer := factory.Discovery().V1().EndpointSlices()
	classInformer := dynFactory.ForResource(gwapi.GatewayClassResource)
	gwInformer := dynFactory.ForResource(gwapi.GatewayResource)
	tcpInformer := dynFactory.ForResource(gwapi.TCPRouteResource)
	udpInformer := dynFactory.ForResource(gwapi.UDPRouteResource)
	nsInformer := factory.Core().V1().Namespaces()
	grantInformer := dynFactory.ForResource(gwapi.ReferenceGrantResource)

	r := &Reconciler{
		plane:         plane,
		logger:        logger,
		serviceLister: svcInformer.Lister(),
		sliceLister:   sliceInformer.Lister(),
		classLister:   classInformer.Lister(),
		gatewayLister: gwInformer.Lister(),
		tcpLister:     tcpInformer.Lister(),
		udpLister:     udpInformer.Lister(),
		nsLister:      nsInformer.Lister(),
		grantLister:   grantInformer.Lister(),
		queue: workqueue.NewTypedRateLimitingQueue[string](
			workqueue.DefaultTypedControllerRateLimiter[string](),
		),
		classQueue: workqueue.NewTypedRateLimitingQueue[string](
			workqueue.DefaultTypedControllerRateLimiter[string](),
		),
		installed: map[string][]string{},
	}

	gwInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { r.enqueueGatewayObj(obj) },
		UpdateFunc: func(_, obj interface{}) { r.enqueueGatewayObj(obj) },
		DeleteFunc: func(obj interface{}) { r.enqueueGatewayObj(obj) },
	})
	classInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(interface{}) { r.classQueue.Add("resync") },
		UpdateFunc: func(interface{}, interface{}) { r.classQueue.Add("resync") },
		DeleteFunc: func(interface{}) { r.classQueue.Add("resync") },
	})
	tcpInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { r.enqueueRoute(obj) },
		UpdateFunc: func(_, obj interface{}) { r.enqueueRoute(obj) },
		DeleteFunc: func(obj interface{}) { r.enqueueRoute(obj) },
	})
	// A namespace's labels or a ReferenceGrant changing can change what is allowed anywhere, and
	// both are rare, so resync every Gateway (the same trigger a GatewayClass change uses).
	resync := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(interface{}) { r.classQueue.Add("resync") },
		UpdateFunc: func(interface{}, interface{}) { r.classQueue.Add("resync") },
		DeleteFunc: func(interface{}) { r.classQueue.Add("resync") },
	}
	nsInformer.Informer().AddEventHandler(resync)
	grantInformer.Informer().AddEventHandler(resync)
	udpInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { r.enqueueRoute(obj) },
		UpdateFunc: func(_, obj interface{}) { r.enqueueRoute(obj) },
		DeleteFunc: func(obj interface{}) { r.enqueueRoute(obj) },
	})
	sliceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { r.enqueueSlice(obj) },
		UpdateFunc: func(_, obj interface{}) { r.enqueueSlice(obj) },
		DeleteFunc: func(obj interface{}) { r.enqueueSlice(obj) },
	})

	return r, factory, dynFactory
}

func (r *Reconciler) enqueueGatewayObj(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		return
	}
	r.queue.Add(key)
}

func toUnstructured(obj interface{}) (*unstructured.Unstructured, bool) {
	u, ok := obj.(*unstructured.Unstructured)
	if ok {
		return u, true
	}
	if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		u, ok = tomb.Obj.(*unstructured.Unstructured)
		return u, ok
	}
	return nil, false
}

func decodeRoute(obj interface{}) (*unstructured.Unstructured, routeShape, bool) {
	u, ok := toUnstructured(obj)
	if !ok {
		return nil, routeShape{}, false
	}
	var rt routeShape
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.UnstructuredContent(), &rt); err != nil {
		return nil, routeShape{}, false
	}
	return u, rt, true
}

// enqueueRoute resolves a TCPRoute/UDPRoute's parentRefs to Gateway keys. A parentRef may name a
// Gateway in another namespace, so the key uses its namespace, defaulting to the route's own.
func (r *Reconciler) enqueueRoute(obj interface{}) {
	u, rt, ok := decodeRoute(obj)
	if !ok {
		return
	}
	rr := RouteRef{Namespace: u.GetNamespace()}
	for _, pr := range rt.Spec.ParentRefs {
		r.queue.Add(parentNamespace(rr, pr) + "/" + pr.Name)
	}
}

func (r *Reconciler) enqueueSlice(obj interface{}) {
	slice, ok := toEndpointSlice(obj)
	if !ok {
		return
	}
	svcName, ok := slice.Labels[serviceNameLabel]
	if !ok || svcName == "" {
		return
	}
	r.enqueueGatewaysForService(slice.Namespace, svcName)
}

// enqueueGatewaysForService finds every Route, in any namespace, whose backendRefs reference the
// Service and enqueues the Gateway(s) it attaches to — the reverse index an EndpointSlice event
// needs, since a slice only carries its own Service's name. A route may reference a Service in
// another namespace (through a ReferenceGrant), so all namespaces are scanned.
func (r *Reconciler) enqueueGatewaysForService(namespace, serviceName string) {
	for _, lister := range []cache.GenericLister{r.tcpLister, r.udpLister} {
		objs, err := lister.List(labels.Everything())
		if err != nil {
			continue
		}
		for _, obj := range objs {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				continue
			}
			var rt routeShape
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.UnstructuredContent(), &rt); err != nil {
				continue
			}
			rr := RouteRef{Namespace: u.GetNamespace(), Spec: rt.Spec}
			if !routeReferencesService(rr, namespace, serviceName) {
				continue
			}
			for _, pr := range rt.Spec.ParentRefs {
				r.queue.Add(parentNamespace(rr, pr) + "/" + pr.Name)
			}
		}
	}
}

// routeReferencesService reports whether rr has a backendRef to Service ns/name.
func routeReferencesService(rr RouteRef, ns, name string) bool {
	for _, rule := range rr.Spec.Rules {
		for _, br := range rule.BackendRefs {
			bns := rr.Namespace
			if br.Namespace != nil && *br.Namespace != "" {
				bns = *br.Namespace
			}
			if bns == ns && br.Name == name {
				return true
			}
		}
	}
	return false
}

func toEndpointSlice(obj interface{}) (*discoveryv1.EndpointSlice, bool) {
	slice, ok := obj.(*discoveryv1.EndpointSlice)
	if ok {
		return slice, true
	}
	if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		slice, ok = tomb.Obj.(*discoveryv1.EndpointSlice)
		return slice, ok
	}
	return nil, false
}

// Run starts both informer factories, waits for every cache to sync, and
// serves the class-resync and Gateway work queues until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context, factory informers.SharedInformerFactory, dynFactory dynamicinformer.DynamicSharedInformerFactory, workers int) error {
	defer r.queue.ShutDown()
	defer r.classQueue.ShutDown()

	factory.Start(ctx.Done())
	dynFactory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(),
		factory.Core().V1().Services().Informer().HasSynced,
		factory.Discovery().V1().EndpointSlices().Informer().HasSynced,
		dynFactory.ForResource(gwapi.GatewayClassResource).Informer().HasSynced,
		dynFactory.ForResource(gwapi.GatewayResource).Informer().HasSynced,
		dynFactory.ForResource(gwapi.TCPRouteResource).Informer().HasSynced,
		dynFactory.ForResource(gwapi.UDPRouteResource).Informer().HasSynced,
		factory.Core().V1().Namespaces().Informer().HasSynced,
		dynFactory.ForResource(gwapi.ReferenceGrantResource).Informer().HasSynced,
	) {
		return fmt.Errorf("timed out waiting for informer caches to sync")
	}
	r.logger.Info("gateway API controller caches synced")

	if r.init != nil {
		var keys []string
		if objs, err := r.gatewayLister.List(labels.Everything()); err == nil {
			for _, obj := range objs {
				if k, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj); err == nil {
					keys = append(keys, k)
				}
			}
		} else {
			r.logger.Error("list gateways for the initial sync", "err", err)
		}
		r.init.Arm(keys)
	}

	go r.classWorker(ctx)
	for i := 0; i < workers; i++ {
		go r.worker(ctx)
	}
	<-ctx.Done()
	return nil
}

// classWorker resyncs every known Gateway whenever any GatewayClass
// changes — a class's controllerName flipping (or a class being
// created/deleted) can change which Gateways are managed, and there are
// normally few enough Gateways that a full resync is simpler and cheap
// enough to not need a reverse index for this rare event.
func (r *Reconciler) classWorker(ctx context.Context) {
	for {
		key, shutdown := r.classQueue.Get()
		if shutdown {
			return
		}
		objs, err := r.gatewayLister.List(labels.Everything())
		if err != nil {
			r.logger.Error("list gateways for class resync", "err", err)
		} else {
			for _, obj := range objs {
				if gwKey, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj); err == nil {
					r.queue.Add(gwKey)
				}
			}
		}
		r.classQueue.Forget(key)
		r.classQueue.Done(key)
	}
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
		r.logger.Error("reconcile", "gateway", key, "err", err)
		r.queue.AddRateLimited(key)
		return true
	}
	r.queue.Forget(key)
	if r.init != nil {
		r.init.Finished(key)
	}
	return true
}

func (r *Reconciler) reconcile(key string) error {
	ns, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return nil // malformed key: nothing we can do, don't requeue forever
	}

	obj, err := r.gatewayLister.ByNamespace(ns).Get(name)
	if apierrors.IsNotFound(err) {
		return r.removeAllFor(key, nil)
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
		return r.removeAllFor(key, nil)
	}

	routes, err := r.attachedRoutes(&gw)
	if err != nil {
		return fmt.Errorf("gateway %s: list attached routes: %w", key, err)
	}

	desired, err := r.buildDesiredVIPs(&gw, routes)
	if err != nil {
		return err
	}
	return r.removeAllFor(key, desired)
}

// managed reports whether className resolves to a GatewayClass this
// controller owns.
func (r *Reconciler) managed(className string) bool {
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

// attachedRoutes lists every TCPRoute/UDPRoute, in any namespace, with a parentRef naming gw. Whether
// a listener actually accepts it (allowedRoutes) is decided per listener in buildDesiredVIPs.
func (r *Reconciler) attachedRoutes(gw *gwapi.Gateway) ([]attachedRoute, error) {
	var out []attachedRoute
	for _, src := range []struct {
		lister cache.GenericLister
		kind   string
	}{{r.tcpLister, gwapi.KindTCPRoute}, {r.udpLister, gwapi.KindUDPRoute}} {
		objs, err := src.lister.List(labels.Everything())
		if err != nil {
			return nil, err
		}
		for _, obj := range objs {
			u, ok := obj.(*unstructured.Unstructured)
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
					out = append(out, attachedRoute{ref: ref})
					break
				}
			}
		}
	}
	return out, nil
}

// NamespaceLabels, Grants and ServiceExists make the Reconciler the Env its attach checks read.
func (r *Reconciler) NamespaceLabels(ns string) (map[string]string, bool) {
	n, err := r.nsLister.Get(ns)
	if err != nil {
		return nil, false
	}
	return n.Labels, true
}

func (r *Reconciler) Grants(ns string) []gwapi.ReferenceGrant { return grantsIn(r.grantLister, ns) }

// grantsIn lists and decodes the ReferenceGrants in namespace ns.
func grantsIn(lister cache.GenericLister, ns string) []gwapi.ReferenceGrant {
	objs, err := lister.ByNamespace(ns).List(labels.Everything())
	if err != nil {
		return nil
	}
	var out []gwapi.ReferenceGrant
	for _, o := range objs {
		u, ok := o.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		var g gwapi.ReferenceGrant
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.UnstructuredContent(), &g); err == nil {
			out = append(out, g)
		}
	}
	return out
}

func (r *Reconciler) ServiceExists(ns, name string) bool {
	_, err := r.serviceLister.Services(ns).Get(name)
	return err == nil
}

var _ Env = (*Reconciler)(nil)

// removeAllFor applies desired's VIP set for key — identical shape and
// reasoning to internal/controller.Reconciler.removeAllFor.
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
