// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package ipam expands AddressPool CIDR/range strings into concrete
// addresses (or sparse prefixes for large IPv6 blocks) and allocates them
// to Services. Pure logic, no Kubernetes client dependency here —
// internal/ipamctrl wires this to the actual watch/patch loop.
package ipam

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// maxEagerHostBits is the largest host portion ExpandPool/ParsePool will
// eagerly materialize into a concrete address list. A /16 IPv4 (16 host
// bits → 65 536 addresses) is the practical upper bound matching the
// range sanity cap below; anything larger (notably an IPv6 /64) is kept
// as a sparse Prefix for randomized-within-prefix allocation.
const maxEagerHostBits = 16

// Expanded is the result of ParsePool: a mix of eagerly-expanded concrete
// addresses and sparse prefixes that are too large to enumerate.
type Expanded struct {
	Addresses []string
	Prefixes  []netip.Prefix
}

// ExpandPool returns every address addrs covers when the set is small
// enough to enumerate. Prefer ParsePool for new callers — ExpandPool
// rejects sparse (large IPv6) prefixes that ParsePool keeps as Prefixes.
func ExpandPool(addrs []string, avoidBuggyIPs bool) ([]string, error) {
	exp, err := ParsePool(addrs, avoidBuggyIPs)
	if err != nil {
		return nil, err
	}
	if len(exp.Prefixes) > 0 {
		return nil, fmt.Errorf("pool contains prefix(es) too large to expand eagerly (host bits > %d); use ParsePool for sparse allocation", maxEagerHostBits)
	}
	return exp.Addresses, nil
}

// ParsePool accepts IPv4 and IPv6 CIDRs, ranges, and single addresses.
// Small prefixes are expanded into Addresses; large ones (host bits >
// maxEagerHostBits) are kept as Prefixes for sparse allocation.
// avoidBuggyIPs only applies to IPv4 (skips .0 / .255 in each /24-aligned
// last octet), matching MetalLB's option of the same name.
func ParsePool(addrs []string, avoidBuggyIPs bool) (Expanded, error) {
	var out Expanded
	seenAddr := map[string]bool{}
	seenPfx := map[string]bool{}
	for _, spec := range addrs {
		part, err := parseOne(spec, avoidBuggyIPs)
		if err != nil {
			return Expanded{}, fmt.Errorf("address %q: %w", spec, err)
		}
		for _, ip := range part.Addresses {
			if seenAddr[ip] {
				continue
			}
			seenAddr[ip] = true
			out.Addresses = append(out.Addresses, ip)
		}
		for _, pfx := range part.Prefixes {
			key := pfx.String()
			if seenPfx[key] {
				continue
			}
			seenPfx[key] = true
			out.Prefixes = append(out.Prefixes, pfx)
		}
	}
	return out, nil
}

func parseOne(spec string, avoidBuggyIPs bool) (Expanded, error) {
	if strings.Contains(spec, "/") {
		return parseCIDR(spec, avoidBuggyIPs)
	}
	if strings.Contains(spec, "-") {
		return parseRange(spec)
	}
	addr, err := netip.ParseAddr(spec)
	if err != nil {
		return Expanded{}, fmt.Errorf("not a valid IP address, CIDR, or range")
	}
	return Expanded{Addresses: []string{addr.String()}}, nil
}

func parseCIDR(spec string, avoidBuggyIPs bool) (Expanded, error) {
	pfx, err := netip.ParsePrefix(spec)
	if err != nil {
		return Expanded{}, err
	}
	pfx = pfx.Masked()
	hostBits := pfx.Addr().BitLen() - pfx.Bits()
	if hostBits > maxEagerHostBits {
		return Expanded{Prefixes: []netip.Prefix{pfx}}, nil
	}

	var out []string
	addr := pfx.Addr()
	for {
		if !pfx.Contains(addr) {
			break
		}
		if !(avoidBuggyIPs && addr.Is4() && isBuggyIPv4(addr)) {
			out = append(out, addr.String())
		}
		next, ok := nextAddr(addr)
		if !ok {
			break
		}
		addr = next
	}
	return Expanded{Addresses: out}, nil
}

func parseRange(spec string) (Expanded, error) {
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return Expanded{}, fmt.Errorf("expected start-end")
	}
	start, err := netip.ParseAddr(strings.TrimSpace(parts[0]))
	if err != nil {
		return Expanded{}, fmt.Errorf("invalid range start: %w", err)
	}
	end, err := netip.ParseAddr(strings.TrimSpace(parts[1]))
	if err != nil {
		return Expanded{}, fmt.Errorf("invalid range end: %w", err)
	}
	if start.Is4() != end.Is4() {
		return Expanded{}, fmt.Errorf("range endpoints must be the same address family")
	}
	if start.Compare(end) > 0 {
		return Expanded{}, fmt.Errorf("start > end")
	}

	var out []string
	for cur := start; ; {
		out = append(out, cur.String())
		if cur == end {
			break
		}
		if len(out) > 1<<20 {
			return Expanded{}, fmt.Errorf("range too large")
		}
		next, ok := nextAddr(cur)
		if !ok {
			return Expanded{}, fmt.Errorf("range overflow")
		}
		cur = next
	}
	return Expanded{Addresses: out}, nil
}

func isBuggyIPv4(addr netip.Addr) bool {
	a := addr.As4()
	return a[3] == 0 || a[3] == 255
}

func nextAddr(addr netip.Addr) (netip.Addr, bool) {
	b := addr.AsSlice()
	next := make([]byte, len(b))
	copy(next, b)
	for i := len(next) - 1; i >= 0; i-- {
		next[i]++
		if next[i] != 0 {
			out, ok := netip.AddrFromSlice(next)
			return out, ok
		}
	}
	return netip.Addr{}, false // overflow
}

// legacy helpers kept for any residual callers / tests using net.IP.
func nextIP(ip net.IP) net.IP {
	next := make(net.IP, len(ip))
	copy(next, ip)
	for i := len(next) - 1; i >= 0; i-- {
		next[i]++
		if next[i] != 0 {
			break
		}
	}
	return next
}

func ipToUint32(ip net.IP) uint32 {
	ip = ip.To4()
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}
