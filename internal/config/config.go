// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package config loads rivorad's static YAML configuration (v0.1: no
// control plane, one file per node — see config/examples/single-vip.yaml).
package config

import (
	"fmt"
	"net"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Mode string

const (
	ModeDSR Mode = "dsr"
	ModeNAT Mode = "nat"
)

type Protocol string

const (
	ProtoTCP Protocol = "tcp"
	ProtoUDP Protocol = "udp"
)

type Backend struct {
	Address string `yaml:"address"`
	Port    uint16 `yaml:"port"`
	MAC     string `yaml:"mac,omitempty"`
	// Weight is this backend's relative share of Maglev table slots
	// within its VIP. 0 (the zero value, i.e. unset) is treated as 1 by
	// internal/maglev.BuildTable — equal weighting, matching pre-weighting
	// behavior exactly.
	Weight uint32 `yaml:"weight,omitempty"`
}

type VIP struct {
	Address  string    `yaml:"address"`
	Port     uint16    `yaml:"port"`
	Protocol Protocol  `yaml:"protocol"`
	Mode     Mode      `yaml:"mode"`
	Backends []Backend `yaml:"backends"`
}

type HealthCheck struct {
	Interval         time.Duration `yaml:"interval"`
	Timeout          time.Duration `yaml:"timeout"`
	FailThreshold    int           `yaml:"failThreshold"`
	SuccessThreshold int           `yaml:"successThreshold"`
}

// RateLimit is an opt-in, per-source-IP token bucket applied to new TCP
// connections (SYN packets only) across every VIP this node owns —
// established connections' data packets and all UDP traffic are
// unaffected. Off by default, matching this project's other security
// knobs (TLS/auth): a zero-value RateLimit changes nothing.
type RateLimit struct {
	Enabled                   bool   `yaml:"enabled"`
	PerSourcePacketsPerSecond uint64 `yaml:"perSourcePacketsPerSecond"`
	Burst                     uint64 `yaml:"burst"`
}

// BGP is an opt-in BGP+BFD speaker for active/active ECMP HA across
// multiple Rivora nodes: each node independently advertises a /32 host
// route for every VIP it currently has at least one healthy backend for,
// and withdraws it the instant that stops being true. This health-gating
// substitutes for the ARP speaker's Lease-based mutual exclusion — unlike
// ARP, BGP+ECMP's whole point is multiple nodes advertising the *same*
// VIP simultaneously, with upstream routers hashing traffic across
// next-hops (active/active, not active/passive), so there's no leader
// election here. BFD is wired per-peer (not a separate component) for
// fast peer-down detection feeding the same health-gated withdraw path.
//
// Caveat, not hidden: in full-NAT mode a flow's connection state lives
// only on the node that first received it — if the router's ECMP hash
// rebalances (a peer flaps, a node joins/leaves), in-flight NAT'd
// connections on the rebalanced-away node can be disrupted, since there's
// no cluster-shared connection table. DSR mode doesn't have this problem
// (the backend itself owns the reply path). Same tradeoff MetalLB's BGP
// mode documents. Off by default.
type BGP struct {
	Enabled  bool   `yaml:"enabled"`
	ASN      uint32 `yaml:"asn"`
	RouterID string `yaml:"routerId"`
	// IPv6NextHop is the IPv6 address advertised as next-hop for IPv6 VIP
	// host routes (/128). Required when any IPv6 VIP is advertised;
	// routerId stays the BGP identifier and IPv4 next-hop (always IPv4).
	IPv6NextHop string    `yaml:"ipv6NextHop,omitempty"`
	Peers       []BGPPeer `yaml:"peers"`
}

type BGPPeer struct {
	Address string `yaml:"address"`
	ASN     uint32 `yaml:"asn"`
	// BFD enables Bidirectional Forwarding Detection on this peer session
	// for sub-second down detection, instead of relying solely on BGP's
	// own (much slower) hold-timer expiry.
	BFD bool `yaml:"bfd,omitempty"`
}

// Validate checks b in isolation — reused both by Config.Validate() for
// static-YAML mode and directly by rivorad's -kubernetes flag parsing,
// since kube mode's cfg.VIPs is always empty (VIPs come from the
// Kubernetes reconcilers, not -config) and so can't go through the full
// Config.Validate() without tripping its unrelated "at least one VIP is
// required" check.
func (b BGP) Validate() error {
	if !b.Enabled {
		return nil
	}
	if b.ASN == 0 {
		return fmt.Errorf("bgp: asn must be > 0 when enabled")
	}
	if net.ParseIP(b.RouterID) == nil {
		return fmt.Errorf("bgp: routerId must be a valid IP address")
	}
	if b.IPv6NextHop != "" {
		ip := net.ParseIP(b.IPv6NextHop)
		if ip == nil || ip.To4() != nil {
			return fmt.Errorf("bgp: ipv6NextHop must be a valid IPv6 address")
		}
	}
	if len(b.Peers) == 0 {
		return fmt.Errorf("bgp: at least one peer is required when enabled")
	}
	for _, p := range b.Peers {
		if net.ParseIP(p.Address) == nil {
			return fmt.Errorf("bgp peer %q: invalid address", p.Address)
		}
		if p.ASN == 0 {
			return fmt.Errorf("bgp peer %s: asn must be > 0", p.Address)
		}
	}
	return nil
}

type Config struct {
	Interface   string      `yaml:"interface"`
	APIListen   string      `yaml:"apiListen"`
	HealthCheck HealthCheck `yaml:"healthCheck"`
	RateLimit   RateLimit   `yaml:"rateLimit"`
	BGP         BGP         `yaml:"bgp"`
	VIPs        []VIP       `yaml:"vips"`
}

func defaults() Config {
	return Config{
		APIListen: "127.0.0.1:9870",
		HealthCheck: HealthCheck{
			Interval:         3 * time.Second,
			Timeout:          time.Second,
			FailThreshold:    2,
			SuccessThreshold: 2,
		},
	}
}

func Load(path string) (Config, error) {
	cfg := defaults()
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.Interface == "" {
		return fmt.Errorf("interface is required")
	}
	if c.RateLimit.Enabled && (c.RateLimit.PerSourcePacketsPerSecond == 0 || c.RateLimit.Burst == 0) {
		return fmt.Errorf("rateLimit: perSourcePacketsPerSecond and burst must both be > 0 when enabled")
	}
	if err := c.BGP.Validate(); err != nil {
		return err
	}
	if len(c.VIPs) == 0 {
		return fmt.Errorf("at least one VIP is required")
	}
	seen := make(map[string]bool, len(c.VIPs))
	for _, v := range c.VIPs {
		key := fmt.Sprintf("%s:%d:%s", v.Address, v.Port, v.Protocol)
		if seen[key] {
			return fmt.Errorf("vip %s:%d/%s: duplicate VIP", v.Address, v.Port, v.Protocol)
		}
		seen[key] = true
	}
	for _, v := range c.VIPs {
		vipIP := net.ParseIP(v.Address)
		if vipIP == nil {
			return fmt.Errorf("vip %q: invalid address", v.Address)
		}
		if v.Protocol != ProtoTCP && v.Protocol != ProtoUDP {
			return fmt.Errorf("vip %s:%d: protocol must be tcp or udp", v.Address, v.Port)
		}
		if v.Mode != ModeDSR && v.Mode != ModeNAT {
			return fmt.Errorf("vip %s:%d: mode must be dsr or nat", v.Address, v.Port)
		}
		if len(v.Backends) == 0 {
			return fmt.Errorf("vip %s:%d: at least one backend is required", v.Address, v.Port)
		}
		vipIsV4 := vipIP.To4() != nil
		for _, b := range v.Backends {
			beIP := net.ParseIP(b.Address)
			if beIP == nil {
				return fmt.Errorf("backend %q: invalid address", b.Address)
			}
			// A VIP's backends must all share its address family — mixed
			// v4/v6 backends behind one VIP isn't a sane concept (the
			// dataplane picks one map set, vip_map/backend_map or
			// vip_map6/backend_map6, per VIP based on the VIP's own
			// family; see internal/dataplane's UpsertVIP).
			if (beIP.To4() != nil) != vipIsV4 {
				return fmt.Errorf("vip %s:%d: backend %s is a different address family than the VIP", v.Address, v.Port, b.Address)
			}
			if v.Mode == ModeDSR {
				if b.MAC == "" {
					return fmt.Errorf("backend %s: mac is required in dsr mode", b.Address)
				}
				if _, err := net.ParseMAC(b.MAC); err != nil {
					return fmt.Errorf("backend %s: invalid mac %q: %w", b.Address, b.MAC, err)
				}
			}
		}
	}
	return nil
}
