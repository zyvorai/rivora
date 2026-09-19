// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package bgp

// Per-VIP peer selection: a route can be limited to some of the peers (route.peers). gobgp
// originates a path into its global RIB, which every peer's export sees; a path cannot be pointed at
// one neighbour. (Per-neighbour export policy exists in gobgp only for route-server clients, which
// also changes how a session behaves, so it is not used.) Instead the speaker adds, to gobgp's one
// global export policy, a rule per peer: "when exporting to this neighbour, reject these prefixes",
// and keeps the list of prefixes equal to the routes that peer must not receive, on every resync,
// before any route is advertised or changed.
//
// gobgp constraints this works around: a prefix set holds one address family, so each peer has one
// set per family; and a set must be edited in place (members added and removed), because a policy
// keeps the set object it was built with.

import (
	"context"
	"crypto/sha1" //nolint:gosec // a name, not security
	"encoding/hex"
	"fmt"
	"net/netip"
	"sort"

	"github.com/osrg/gobgp/v4/api"
)

// normAddr is addr in canonical text form, so a peer written "2001:DB8::1" matches "2001:db8::1".
// Text that is not an address is returned as is.
func normAddr(addr string) string {
	a, err := netip.ParseAddr(addr)
	if err != nil {
		return addr
	}
	return a.Unmap().String()
}

// peerObjects are the gobgp objects that implement peer addr's deny list.
type peerObjects struct {
	neighbors, deny4, deny6, policy, stmt4, stmt6 string
}

func objectsFor(addr string) peerObjects {
	sum := sha1.Sum([]byte(normAddr(addr)))
	id := hex.EncodeToString(sum[:6])
	return peerObjects{
		neighbors: "rivora-peer-" + id,
		deny4:     "rivora-deny4-" + id,
		deny6:     "rivora-deny6-" + id,
		policy:    "rivora-export-" + id,
		stmt4:     "rivora-reject4-" + id,
		stmt6:     "rivora-reject6-" + id,
	}
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// denyList is the prefixes peer addr must not receive: those of the routes limited to other peers.
func denyList(addr string, restrict map[netip.Prefix][]string) []string {
	n := normAddr(addr)
	var out []string
	for p, allowed := range restrict {
		if !containsStr(allowed, n) {
			out = append(out, p.String())
		}
	}
	sort.Strings(out)
	return out
}

func prefixSet(name string, prefixes []string) *api.DefinedSet {
	ds := &api.DefinedSet{DefinedType: api.DefinedType_DEFINED_TYPE_PREFIX, Name: name}
	for _, s := range prefixes {
		p := netip.MustParsePrefix(s)
		ds.Prefixes = append(ds.Prefixes, &api.Prefix{IpPrefix: s, MaskLengthMin: uint32(p.Bits()), MaskLengthMax: uint32(p.Bits())})
	}
	return ds
}

// splitFamilies separates prefixes (canonical text) into IPv4 and IPv6.
func splitFamilies(prefixes []string) (v4, v6 []string) {
	for _, s := range prefixes {
		if netip.MustParsePrefix(s).Addr().Is4() {
			v4 = append(v4, s)
		} else {
			v6 = append(v6, s)
		}
	}
	return v4, v6
}

// diffPrefixes returns what is in want but not have, and what is in have but not want.
func diffPrefixes(have, want []string) (add, del []string) {
	h, w := map[string]bool{}, map[string]bool{}
	for _, x := range have {
		h[x] = true
	}
	for _, x := range want {
		w[x] = true
		if !h[x] {
			add = append(add, x)
		}
	}
	for _, x := range have {
		if !w[x] {
			del = append(del, x)
		}
	}
	return add, del
}

// editPrefixSet moves prefix set name from the members have to want, in place.
func (sp *Speaker) editPrefixSet(ctx context.Context, name string, have, want []string) error {
	add, del := diffPrefixes(have, want)
	if len(add) > 0 {
		if err := sp.server.AddDefinedSet(ctx, &api.AddDefinedSetRequest{DefinedSet: prefixSet(name, add)}); err != nil {
			return err
		}
	}
	if len(del) > 0 {
		if err := sp.server.DeleteDefinedSet(ctx, &api.DeleteDefinedSetRequest{DefinedSet: prefixSet(name, del)}); err != nil {
			return err
		}
	}
	return nil
}

// editDeny moves peer addr's two deny sets from the deny list have to want.
func (sp *Speaker) editDeny(ctx context.Context, addr string, have, want []string) error {
	o := objectsFor(addr)
	have4, have6 := splitFamilies(have)
	want4, want6 := splitFamilies(want)
	if err := sp.editPrefixSet(ctx, o.deny4, have4, want4); err != nil {
		return err
	}
	return sp.editPrefixSet(ctx, o.deny6, have6, want6)
}

// createPeerLimits defines what keeps peer addr from receiving the routes in deny: its two prefix
// sets (holding them), a neighbour set naming the peer, an export policy rejecting a route that
// matches both, and the addition of that policy to the global export policy. It must run before
// the peer is added, so that no route is exported to it ahead of the rule. Anything a previous peer
// of that address left behind is cleared first.
func (sp *Speaker) createPeerLimits(ctx context.Context, addr string, deny []string) error {
	sp.dropPeerLimits(ctx, addr)
	o := objectsFor(addr)
	v4, v6 := splitFamilies(deny)

	a := netip.MustParseAddr(normAddr(addr))
	if err := sp.server.AddDefinedSet(ctx, &api.AddDefinedSetRequest{DefinedSet: &api.DefinedSet{
		DefinedType: api.DefinedType_DEFINED_TYPE_NEIGHBOR, Name: o.neighbors, List: []string{netip.PrefixFrom(a, a.BitLen()).String()},
	}}); err != nil {
		return fmt.Errorf("define neighbour set: %w", err)
	}
	for name, members := range map[string][]string{o.deny4: v4, o.deny6: v6} {
		if err := sp.server.AddDefinedSet(ctx, &api.AddDefinedSetRequest{DefinedSet: prefixSet(name, members)}); err != nil {
			return fmt.Errorf("define prefix set %s: %w", name, err)
		}
	}
	reject := func(stmt, deny string) *api.Statement {
		return &api.Statement{
			Name: stmt,
			Conditions: &api.Conditions{
				NeighborSet: &api.MatchSet{Type: api.MatchSet_TYPE_ANY, Name: o.neighbors},
				PrefixSet:   &api.MatchSet{Type: api.MatchSet_TYPE_ANY, Name: deny},
			},
			Actions: &api.Actions{RouteAction: api.RouteAction_ROUTE_ACTION_REJECT},
		}
	}
	if err := sp.server.AddPolicy(ctx, &api.AddPolicyRequest{Policy: &api.Policy{
		Name: o.policy, Statements: []*api.Statement{reject(o.stmt4, o.deny4), reject(o.stmt6, o.deny6)},
	}}); err != nil {
		return fmt.Errorf("define export policy: %w", err)
	}
	if err := sp.server.AddPolicyAssignment(ctx, &api.AddPolicyAssignmentRequest{Assignment: &api.PolicyAssignment{
		Name: "global", Direction: api.PolicyDirection_POLICY_DIRECTION_EXPORT,
		Policies: []*api.Policy{{Name: o.policy}}, DefaultAction: api.RouteAction_ROUTE_ACTION_ACCEPT,
	}}); err != nil {
		return fmt.Errorf("apply export policy: %w", err)
	}
	sp.denied[normAddr(addr)] = deny
	return nil
}

// dropPeerLimits removes what createPeerLimits defined, in the order gobgp requires (nothing that is
// in use can be deleted). Best effort: it also runs before a create, when there may be nothing.
func (sp *Speaker) dropPeerLimits(ctx context.Context, addr string) {
	o := objectsFor(addr)
	_ = sp.server.DeletePolicyAssignment(ctx, &api.DeletePolicyAssignmentRequest{Assignment: &api.PolicyAssignment{
		Name: "global", Direction: api.PolicyDirection_POLICY_DIRECTION_EXPORT, Policies: []*api.Policy{{Name: o.policy}},
	}})
	_ = sp.server.DeletePolicy(ctx, &api.DeletePolicyRequest{Policy: &api.Policy{Name: o.policy}, All: true})
	for _, set := range []*api.DefinedSet{
		{DefinedType: api.DefinedType_DEFINED_TYPE_PREFIX, Name: o.deny4},
		{DefinedType: api.DefinedType_DEFINED_TYPE_PREFIX, Name: o.deny6},
		{DefinedType: api.DefinedType_DEFINED_TYPE_NEIGHBOR, Name: o.neighbors},
	} {
		_ = sp.server.DeleteDefinedSet(ctx, &api.DeleteDefinedSetRequest{DefinedSet: set, All: true})
	}
	delete(sp.denied, normAddr(addr))
}

// syncDenySets records which routes are limited to which peers and brings every peer's deny list in
// line, returning an error when one could not be updated. The caller must then not advertise a
// limited route: it would reach a peer that must not have it. Holds peerMu itself.
func (sp *Speaker) syncDenySets(ctx context.Context, desired map[netip.Prefix]route) error {
	restrict := map[netip.Prefix][]string{}
	for p, r := range desired {
		if r.peers != nil {
			restrict[p] = r.peers
		}
	}

	sp.peerMu.Lock()
	defer sp.peerMu.Unlock()
	sp.restrict = restrict
	var firstErr error
	for addr := range sp.peers {
		want := denyList(addr, restrict)
		if equalStrings(sp.denied[normAddr(addr)], want) {
			continue
		}
		if err := sp.editDeny(ctx, addr, sp.denied[normAddr(addr)], want); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("update the routes peer %s must not receive: %w", addr, err)
			}
			continue
		}
		sp.denied[normAddr(addr)] = want
	}
	return firstErr
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
