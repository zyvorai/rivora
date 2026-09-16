// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package ipam expands AddressPool CIDR/range strings into concrete
// addresses and allocates them to Services. Pure logic, no Kubernetes
// client dependency here — internal/controller/ipam_controller.go wires
// this to the actual watch/patch loop.
package ipam

import (
	"fmt"
	"net"
	"strings"
)

// ExpandPool returns every IPv4 address addrs (a mix of CIDRs like
// "10.0.0.0/24" and explicit ranges like "10.0.0.10-10.0.0.40") covers, in
// order. avoidBuggyIPs skips a /24 subnet's .0 and .255 addresses
// (matching MetalLB's option of the same name — some older client stacks
// mishandle them).
func ExpandPool(addrs []string, avoidBuggyIPs bool) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, spec := range addrs {
		ips, err := expandOne(spec, avoidBuggyIPs)
		if err != nil {
			return nil, fmt.Errorf("address %q: %w", spec, err)
		}
		for _, ip := range ips {
			if seen[ip] {
				continue
			}
			seen[ip] = true
			out = append(out, ip)
		}
	}
	return out, nil
}

func expandOne(spec string, avoidBuggyIPs bool) ([]string, error) {
	if strings.Contains(spec, "/") {
		return expandCIDR(spec, avoidBuggyIPs)
	}
	if strings.Contains(spec, "-") {
		return expandRange(spec)
	}
	ip := net.ParseIP(spec).To4()
	if ip == nil {
		return nil, fmt.Errorf("not a valid IPv4 address, CIDR, or range")
	}
	return []string{ip.String()}, nil
}

func expandCIDR(spec string, avoidBuggyIPs bool) ([]string, error) {
	ip, ipnet, err := net.ParseCIDR(spec)
	if err != nil {
		return nil, err
	}
	if ip.To4() == nil {
		return nil, fmt.Errorf("v0.2 is IPv4-only")
	}

	var out []string
	for cur := ipnet.IP.Mask(ipnet.Mask); ipnet.Contains(cur); cur = nextIP(cur) {
		last := cur[len(cur)-1]
		if avoidBuggyIPs && (last == 0 || last == 255) {
			continue
		}
		out = append(out, cur.String())
	}
	return out, nil
}

func expandRange(spec string) ([]string, error) {
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("expected start-end")
	}
	start := net.ParseIP(strings.TrimSpace(parts[0])).To4()
	end := net.ParseIP(strings.TrimSpace(parts[1])).To4()
	if start == nil || end == nil {
		return nil, fmt.Errorf("invalid range endpoints")
	}
	if ipToUint32(start) > ipToUint32(end) {
		return nil, fmt.Errorf("start > end")
	}

	var out []string
	for cur := start; ; cur = nextIP(cur) {
		out = append(out, cur.String())
		if cur.Equal(end) {
			break
		}
		if len(out) > 1<<20 { // sanity cap: a /12 worth of addresses
			return nil, fmt.Errorf("range too large")
		}
	}
	return out, nil
}

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
