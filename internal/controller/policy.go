// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package controller

import (
	"fmt"
	"sort"

	discoveryv1 "k8s.io/api/discovery/v1"

	"github.com/zyvorai/rivora/api/v1alpha1"
	"github.com/zyvorai/rivora/internal/config"
	"github.com/zyvorai/rivora/internal/dataplane"
)

// servicePolicy is a ServicePolicy checked and converted into what the dataplane
// consumes. The zero value (or nil) changes nothing.
type servicePolicy struct {
	probe      config.ProbeSpec
	rateLimit  config.VIPRateLimit
	weights    map[string]uint32 // node name -> weight
	defaultW   uint32
	sourceName string // "namespace/name", for logs
}

// compilePolicy validates p and converts it. A policy that fails validation is
// rejected whole rather than half-applied: a typo in one field must not silently
// leave the others in force with the intended safeguard missing.
func compilePolicy(p *v1alpha1.ServicePolicy) (*servicePolicy, error) {
	out := &servicePolicy{sourceName: p.Namespace + "/" + p.Name}

	if k := p.Spec.TargetRef.Kind; k != "" && k != "Service" {
		return nil, fmt.Errorf("targetRef.kind %q: only Service is supported", k)
	}
	if h := p.Spec.HealthCheck; h != nil {
		out.probe = config.ProbeSpec{
			Type: config.ProbeType(h.Type), Port: h.Port,
			Path: h.Path, Host: h.Host, ExpectStatus: h.ExpectStatus,
		}
		if err := out.probe.Validate(); err != nil {
			return nil, err
		}
	}
	if r := p.Spec.RateLimit; r != nil {
		out.rateLimit = config.VIPRateLimit{PerSourcePacketsPerSecond: r.PerSourcePacketsPerSecond, Burst: r.Burst}
		if !out.rateLimit.Set() {
			return nil, fmt.Errorf("rateLimit: perSourcePacketsPerSecond and burst must both be > 0")
		}
		if err := out.rateLimit.Validate(); err != nil {
			return nil, err
		}
	}
	if w := p.Spec.Weights; w != nil {
		check := func(what string, v uint32) error {
			if v > dataplane.MaxBackendWeight {
				return fmt.Errorf("weights.%s: %d is above the maximum %d", what, v, dataplane.MaxBackendWeight)
			}
			return nil
		}
		if err := check("default", w.Default); err != nil {
			return nil, err
		}
		out.defaultW = w.Default
		if len(w.Nodes) > 0 {
			out.weights = make(map[string]uint32, len(w.Nodes))
		}
		for node, v := range w.Nodes {
			if v == 0 {
				return nil, fmt.Errorf("weights.nodes[%q]: weight must be 1 or more (to take a node out of rotation, drain it instead)", node)
			}
			if err := check(fmt.Sprintf("nodes[%q]", node), v); err != nil {
				return nil, err
			}
			out.weights[node] = v
		}
	}
	return out, nil
}

// weightFor returns the weight an endpoint gets: its node's entry, else the
// default, else 0, which Maglev treats as 1. A nil policy weights nothing.
func (sp *servicePolicy) weightFor(ep discoveryv1.Endpoint) uint32 {
	if sp == nil {
		return 0
	}
	if ep.NodeName != nil {
		if w, ok := sp.weights[*ep.NodeName]; ok {
			return w
		}
	}
	return sp.defaultW
}

// pickPolicy chooses the policy that governs a Service when several target it: the
// oldest wins (then the lowest name), so adding a second policy never changes which
// one is in force. The rest are returned so the caller can say they are ignored.
func pickPolicy(policies []*v1alpha1.ServicePolicy) (winner *v1alpha1.ServicePolicy, ignored []*v1alpha1.ServicePolicy) {
	if len(policies) == 0 {
		return nil, nil
	}
	sorted := append([]*v1alpha1.ServicePolicy(nil), policies...)
	sort.SliceStable(sorted, func(i, j int) bool {
		ti, tj := sorted[i].CreationTimestamp, sorted[j].CreationTimestamp
		if !ti.Equal(&tj) {
			return ti.Before(&tj)
		}
		return sorted[i].Name < sorted[j].Name
	})
	return sorted[0], sorted[1:]
}

// probeSpec and vipRateLimit tolerate a nil policy, so callers need no guard.
func (sp *servicePolicy) probeSpec() config.ProbeSpec {
	if sp == nil {
		return config.ProbeSpec{}
	}
	return sp.probe
}

func (sp *servicePolicy) vipRateLimit() config.VIPRateLimit {
	if sp == nil {
		return config.VIPRateLimit{}
	}
	return sp.rateLimit
}
