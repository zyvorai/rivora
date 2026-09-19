// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package gatewayapi

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	gwapi "github.com/zyvorai/rivora/api/gatewayapi"
)

// The decisions in this file — may this route attach to this listener, may this route reference that
// Service — are shared by the two things that must agree on them: the per-node dataplane reconciler,
// which programs only what is allowed, and the status writer in rivora-controller, which reports it.
// They read the world through Env so neither depends on the other's informers.

// Env is what attachment and reference checks need beyond the objects themselves.
type Env interface {
	// NamespaceLabels returns a namespace's labels; ok is false if it is not known.
	NamespaceLabels(ns string) (labels map[string]string, ok bool)
	// Grants lists the ReferenceGrants that live in namespace ns.
	Grants(ns string) []gwapi.ReferenceGrant
	// ServiceExists reports whether Service ns/name exists.
	ServiceExists(ns, name string) bool
}

// RouteRef is one TCPRoute or UDPRoute, whichever it is.
type RouteRef struct {
	Kind      string // gwapi.KindTCPRoute or gwapi.KindUDPRoute
	Namespace string
	Name      string
	Spec      gwapi.RouteSpec
}

// KindForProtocol is the route kind a listener protocol serves.
func KindForProtocol(protocol string) (string, bool) {
	switch protocol {
	case "TCP":
		return gwapi.KindTCPRoute, true
	case "UDP":
		return gwapi.KindUDPRoute, true
	}
	return "", false
}

// parentNamespace is the Gateway namespace a parentRef names: its own, else the route's.
func parentNamespace(rr RouteRef, pr gwapi.ParentRef) string {
	if pr.Namespace != nil && *pr.Namespace != "" {
		return *pr.Namespace
	}
	return rr.Namespace
}

// namesGateway reports whether pr refers to gw.
func namesGateway(rr RouteRef, pr gwapi.ParentRef, gw *gwapi.Gateway) bool {
	if pr.Name != gw.Name || parentNamespace(rr, pr) != gw.Namespace {
		return false
	}
	if pr.Group != nil && *pr.Group != gwapi.GroupName {
		return false
	}
	return pr.Kind == nil || *pr.Kind == "Gateway"
}

// Attach is the outcome for one (route, parentRef, listener).
type Attach struct {
	Attached bool
	Reason   string
	Message  string
}

// attachToListener decides whether rr, through parentRef pr, attaches to listener l of gw.
func attachToListener(gw *gwapi.Gateway, l gwapi.GatewayListener, rr RouteRef, pr gwapi.ParentRef, env Env) Attach {
	if !namesGateway(rr, pr, gw) {
		return Attach{Reason: gwapi.ReasonNoMatchingParent, Message: "the parentRef does not name this Gateway"}
	}
	if pr.SectionName != nil && *pr.SectionName != l.Name {
		return Attach{Reason: gwapi.ReasonNoMatchingParent, Message: fmt.Sprintf("sectionName %q is not this listener", *pr.SectionName)}
	}
	served, ok := KindForProtocol(l.Protocol)
	if !ok {
		return Attach{Reason: gwapi.ReasonNotAllowedByListeners, Message: fmt.Sprintf("listener protocol %q is not TCP or UDP", l.Protocol)}
	}
	if rr.Kind != served {
		return Attach{Reason: gwapi.ReasonNotAllowedByListeners, Message: fmt.Sprintf("a %s listener serves %s, not %s", l.Protocol, served, rr.Kind)}
	}

	ar := l.AllowedRoutes
	if ar != nil && len(ar.Kinds) > 0 {
		allowed := false
		for _, k := range ar.Kinds {
			if (k.Group == nil || *k.Group == gwapi.GroupName) && k.Kind == rr.Kind {
				allowed = true
				break
			}
		}
		if !allowed {
			return Attach{Reason: gwapi.ReasonNotAllowedByListeners, Message: fmt.Sprintf("the listener's allowedRoutes.kinds does not include %s", rr.Kind)}
		}
	}

	from := "Same"
	var selector *metav1.LabelSelector
	if ar != nil && ar.Namespaces != nil {
		if ar.Namespaces.From != nil {
			from = *ar.Namespaces.From
		}
		selector = ar.Namespaces.Selector
	}
	switch from {
	case "Same":
		if rr.Namespace != gw.Namespace {
			return Attach{Reason: gwapi.ReasonNotAllowedByListeners, Message: fmt.Sprintf("the listener only accepts routes from namespace %q (allowedRoutes.namespaces.from: Same)", gw.Namespace)}
		}
	case "All":
	case "Selector":
		if selector == nil {
			return Attach{Reason: gwapi.ReasonNotAllowedByListeners, Message: "allowedRoutes.namespaces.from is Selector but no selector is set"}
		}
		sel, err := metav1.LabelSelectorAsSelector(selector)
		if err != nil {
			return Attach{Reason: gwapi.ReasonNotAllowedByListeners, Message: "allowedRoutes.namespaces.selector is invalid: " + err.Error()}
		}
		nsLabels, known := env.NamespaceLabels(rr.Namespace)
		if !known || !sel.Matches(labels.Set(nsLabels)) {
			return Attach{Reason: gwapi.ReasonNotAllowedByListeners, Message: fmt.Sprintf("namespace %q does not match the listener's namespace selector", rr.Namespace)}
		}
	default:
		return Attach{Reason: gwapi.ReasonNotAllowedByListeners, Message: fmt.Sprintf("allowedRoutes.namespaces.from %q is not Same, All or Selector", from)}
	}
	return Attach{Attached: true, Reason: gwapi.ReasonAccepted}
}

// ParentOutcome is whether one parentRef of a route is accepted by a Gateway, and by which listeners.
type ParentOutcome struct {
	Accepted  bool
	Reason    string
	Message   string
	Listeners []string // names of the listeners it attaches to
}

// EvaluateParent decides, for one parentRef of rr, whether gw accepts the route. It is accepted if it
// attaches to at least one listener. When it does not, the reason distinguishes a parentRef that
// matches nothing (NoMatchingParent) from a match the listeners refuse (NotAllowedByListeners).
func EvaluateParent(gw *gwapi.Gateway, rr RouteRef, pr gwapi.ParentRef, env Env) ParentOutcome {
	out := ParentOutcome{Reason: gwapi.ReasonNoMatchingParent, Message: "the parentRef matches no listener of the Gateway"}
	if !namesGateway(rr, pr, gw) {
		return out
	}
	for _, l := range gw.Spec.Listeners {
		res := attachToListener(gw, l, rr, pr, env)
		if res.Attached {
			out.Accepted, out.Reason, out.Message = true, gwapi.ReasonAccepted, "the route is attached to the Gateway"
			out.Listeners = append(out.Listeners, l.Name)
			continue
		}
		// A listener the parentRef did select but which refused the route is the more useful reason.
		if !out.Accepted && res.Reason == gwapi.ReasonNotAllowedByListeners {
			out.Reason, out.Message = res.Reason, res.Message
		}
	}
	return out
}

// RefOutcome is whether one backendRef may be used.
type RefOutcome struct {
	Resolved bool
	Reason   string
	Message  string
}

// CheckBackendRef decides whether rr may use br: it must be a Service, and if that Service is in
// another namespace, a ReferenceGrant there must allow this kind of route from this namespace.
func CheckBackendRef(rr RouteRef, br gwapi.BackendRef, env Env) RefOutcome {
	if br.Kind != nil && *br.Kind != "Service" || br.Group != nil && *br.Group != "" {
		return RefOutcome{Reason: gwapi.ReasonInvalidKind, Message: "only Service backendRefs are supported"}
	}
	ns := rr.Namespace
	if br.Namespace != nil && *br.Namespace != "" {
		ns = *br.Namespace
	}
	if ns != rr.Namespace && !grantAllows(env.Grants(ns), rr, br.Name) {
		return RefOutcome{Reason: gwapi.ReasonRefNotPermitted, Message: fmt.Sprintf("no ReferenceGrant in namespace %q allows %s from namespace %q to reference Service %q", ns, rr.Kind, rr.Namespace, br.Name)}
	}
	if !env.ServiceExists(ns, br.Name) {
		return RefOutcome{Reason: gwapi.ReasonBackendNotFound, Message: fmt.Sprintf("Service %s/%s does not exist", ns, br.Name)}
	}
	return RefOutcome{Resolved: true, Reason: gwapi.ReasonResolvedRefs}
}

// grantAllows reports whether any grant permits routes of rr's kind, in rr's namespace, to reference
// the Service named svc in the grant's own namespace.
func grantAllows(grants []gwapi.ReferenceGrant, rr RouteRef, svc string) bool {
	for _, g := range grants {
		fromOK := false
		for _, f := range g.Spec.From {
			if f.Group == gwapi.GroupName && f.Kind == rr.Kind && f.Namespace == rr.Namespace {
				fromOK = true
				break
			}
		}
		if !fromOK {
			continue
		}
		for _, t := range g.Spec.To {
			if t.Group == "" && t.Kind == "Service" && (t.Name == nil || *t.Name == svc) {
				return true
			}
		}
	}
	return false
}
