// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package config loads rivorad's static YAML configuration (v0.1: no
// control plane, one file per node — see config/examples/single-vip.yaml).
package config

import (
	"fmt"
	"net"
	"os"
	"reflect"
	"strconv"
	"strings"
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
	Address string `yaml:"address"`
	// Port is the VIP's port, or the first port of a range when PortEnd is set.
	Port uint16 `yaml:"port"`
	// PortEnd, when set, makes this VIP own every port from Port to PortEnd inclusive
	// (a "range VIP"), for things like passive FTP, RTP or game servers. A range VIP
	// keeps the destination port the client used, so its backends listen on the same
	// ports and their own port is left 0; because there is then no backend port to
	// probe, healthCheck.port is required. An exact-port VIP on the same address may
	// sit inside a range and takes precedence over it for that one port.
	PortEnd uint16 `yaml:"portEnd,omitempty"`
	// Ports and PortRange are conveniences read only from a config file: Ports
	// ([80, 443]) becomes one VIP per port sharing the rest of this entry, and
	// PortRange ("30000-30100") becomes Port/PortEnd. Load expands and clears them,
	// so nothing after Load ever sees either.
	Ports     []uint16  `yaml:"ports,omitempty"`
	PortRange string    `yaml:"portRange,omitempty"`
	Protocol  Protocol  `yaml:"protocol"`
	Mode      Mode      `yaml:"mode"`
	Backends  []Backend `yaml:"backends"`
	// SessionAffinity says whether one client sticks to one backend. Unset (or
	// "none") spreads a client's connections across backends; "clientIP" sends
	// every connection from a source address to the same backend, for as long as
	// the backend set is unchanged. It maps Kubernetes' Service.spec.sessionAffinity.
	SessionAffinity SessionAffinity `yaml:"sessionAffinity,omitempty"`
	// HealthCheck says how this VIP's backends are probed. Unset means the
	// default: a TCP connect to the backend's service port, exactly as before.
	HealthCheck ProbeSpec `yaml:"healthCheck,omitempty"`
	// RateLimit is this VIP's own per-source SYN limit. Unset (zero) means the VIP
	// follows the node-wide rateLimit; set, it replaces that limit for this VIP,
	// whether or not the node-wide one is enabled.
	RateLimit VIPRateLimit `yaml:"rateLimit,omitempty"`
}

// VIPRateLimit is a per-source-IP token bucket on one VIP's new TCP connections
// (SYN packets). Like the node-wide RateLimit it leaves established connections and
// UDP alone. The zero value means "not set", not "limit to nothing".
type VIPRateLimit struct {
	PerSourcePacketsPerSecond uint64 `yaml:"perSourcePacketsPerSecond,omitempty"`
	Burst                     uint64 `yaml:"burst,omitempty"`
}

// Set reports whether a limit is configured.
func (r VIPRateLimit) Set() bool { return r != (VIPRateLimit{}) }

// Validate accepts the zero value or a limit with both fields positive.
func (r VIPRateLimit) Validate() error {
	if r.Set() && (r.PerSourcePacketsPerSecond == 0 || r.Burst == 0) {
		return fmt.Errorf("rateLimit: perSourcePacketsPerSecond and burst must both be > 0")
	}
	return nil
}

// IsRange reports whether v owns a port range rather than one port.
func (v VIP) IsRange() bool { return v.PortEnd != 0 }

// PortLabel is the VIP's port as text: "443" or, for a range, "30000-30100".
func (v VIP) PortLabel() string {
	if v.IsRange() {
		return fmt.Sprintf("%d-%d", v.Port, v.PortEnd)
	}
	return strconv.Itoa(int(v.Port))
}

// Contains reports whether port falls inside the VIP's port or port range.
func (v VIP) Contains(port uint16) bool {
	if v.IsRange() {
		return port >= v.Port && port <= v.PortEnd
	}
	return port == v.Port
}

// SessionAffinity is how a VIP chooses a backend for a new connection.
type SessionAffinity string

const (
	// AffinityNone hashes the whole 5-tuple: a client's connections spread out.
	AffinityNone SessionAffinity = "none"
	// AffinityClientIP hashes the source address only, so a client's connections all
	// go to the same backend. Unlike Kubernetes' ClientIP affinity there is no
	// timeout: it holds while the backend set is stable, and when that changes
	// Maglev moves only a small share of clients.
	AffinityClientIP SessionAffinity = "clientIP"
)

// Effective treats the empty value as the default, none.
func (a SessionAffinity) Effective() SessionAffinity {
	if a == "" {
		return AffinityNone
	}
	return a
}

// Validate accepts none, clientIP, or empty (the default).
func (a SessionAffinity) Validate() error {
	switch a.Effective() {
	case AffinityNone, AffinityClientIP:
		return nil
	}
	return fmt.Errorf("sessionAffinity %q: must be none or clientIP", string(a))
}

// ProbeType is what an active health probe does.
type ProbeType string

const (
	// ProbeTCP opens a TCP connection and closes it. It proves a port accepts
	// connections, not that the application behind it works.
	ProbeTCP ProbeType = "tcp"
	// ProbeHTTP sends a GET and checks the status code, so a backend whose app is
	// wedged, erroring or still warming up is taken out of rotation even though
	// its port is open.
	ProbeHTTP ProbeType = "http"
)

// DefaultExpectStatus is what an HTTP probe accepts unless told otherwise: any
// success or redirect. Redirects are not followed, only counted, so a backend
// that answers 302 to a login page is not marked down by it.
const DefaultExpectStatus = "200-399"

// ProbeSpec configures one VIP's active health probe. The zero value is the
// legacy TCP connect.
//
// A backend address is probed once however many VIPs list it, so VIPs that share
// a backend must agree on its probe; Config.Validate rejects a conflict rather
// than letting one silently win.
type ProbeSpec struct {
	Type ProbeType `yaml:"type,omitempty"` // tcp (default) | http
	// Port probes this port instead of the backend's service port. Health
	// endpoints are often on their own port, and a UDP service has no TCP port to
	// connect to at all. 0 means the service port.
	Port uint16 `yaml:"port,omitempty"`
	// The remaining fields apply to type http only.
	Path         string `yaml:"path,omitempty"`         // default "/"; must start with "/"
	Host         string `yaml:"host,omitempty"`         // Host header; default is the backend address
	ExpectStatus string `yaml:"expectStatus,omitempty"` // "200" or "200-299"; default 200-399
}

// Effective returns the spec with defaults filled in, so callers and the
// conflict check compare like with like (unset and explicit-default are equal).
func (p ProbeSpec) Effective() ProbeSpec {
	if p.Type == "" {
		p.Type = ProbeTCP
	}
	if p.Type == ProbeHTTP {
		if p.Path == "" {
			p.Path = "/"
		}
		if p.ExpectStatus == "" {
			p.ExpectStatus = DefaultExpectStatus
		}
	}
	return p
}

// StatusRange parses ExpectStatus ("200" or "200-299") into inclusive bounds.
func (p ProbeSpec) StatusRange() (lo, hi int, err error) {
	e := p.Effective().ExpectStatus
	parse := func(s string) (int, error) {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < 100 || n > 599 {
			return 0, fmt.Errorf("expectStatus %q: %q is not an HTTP status code (100-599)", e, s)
		}
		return n, nil
	}
	if a, b, isRange := strings.Cut(e, "-"); isRange {
		if lo, err = parse(a); err != nil {
			return 0, 0, err
		}
		if hi, err = parse(b); err != nil {
			return 0, 0, err
		}
		if lo > hi {
			return 0, 0, fmt.Errorf("expectStatus %q: range start is after its end", e)
		}
		return lo, hi, nil
	}
	if lo, err = parse(e); err != nil {
		return 0, 0, err
	}
	return lo, lo, nil
}

// Validate checks one probe spec in isolation.
func (p ProbeSpec) Validate() error {
	switch e := p.Effective(); e.Type {
	case ProbeTCP:
		if p.Path != "" || p.Host != "" || p.ExpectStatus != "" {
			return fmt.Errorf("healthCheck: path, host and expectStatus only apply to type http")
		}
	case ProbeHTTP:
		if !strings.HasPrefix(e.Path, "/") {
			return fmt.Errorf("healthCheck: path %q must start with /", p.Path)
		}
		if strings.ContainsAny(e.Path, " \t\r\n") {
			return fmt.Errorf("healthCheck: path %q must not contain whitespace", p.Path)
		}
		if strings.ContainsAny(e.Host, " /\t\r\n") {
			return fmt.Errorf("healthCheck: host %q is not a valid Host header value", p.Host)
		}
		if _, _, err := p.StatusRange(); err != nil {
			return fmt.Errorf("healthCheck: %w", err)
		}
	default:
		return fmt.Errorf("healthCheck: type %q must be tcp or http", string(p.Type))
	}
	return nil
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

// XDPMode is how the XDP ingress program attaches to the interface.
//
// Generic (the default) runs XDP in the kernel's network stack after the driver:
// it works on any interface, including veth, but skips most of the performance
// advantage of XDP. Native runs it inside the NIC driver before an skb is even
// allocated, which needs driver support. Auto tries native and falls back to
// generic (with a warning) when the driver can't.
type XDPMode string

const (
	XDPGeneric XDPMode = "generic"
	XDPNative  XDPMode = "native"
	XDPAuto    XDPMode = "auto"
)

// Effective returns the mode to use: the empty value (a Config built in code
// rather than loaded from a file) means the default, generic.
func (m XDPMode) Effective() XDPMode {
	if m == "" {
		return XDPGeneric
	}
	return m
}

// Validate rejects anything but the three modes (or empty, meaning generic).
func (m XDPMode) Validate() error {
	switch m.Effective() {
	case XDPGeneric, XDPNative, XDPAuto:
		return nil
	}
	return fmt.Errorf("xdpMode %q: must be generic, native or auto", string(m))
}

type Config struct {
	Interface string `yaml:"interface"`
	// XDPMode defaults to generic, the only mode that works on every interface
	// (native XDP_TX on veth, for one, does not reliably cross a bridge).
	XDPMode     XDPMode     `yaml:"xdpMode,omitempty"`
	APIListen   string      `yaml:"apiListen"`
	HealthCheck HealthCheck `yaml:"healthCheck"`
	RateLimit   RateLimit   `yaml:"rateLimit"`
	BGP         BGP         `yaml:"bgp"`
	VIPs        []VIP       `yaml:"vips"`
}

// HasNATVIP reports whether any of vips forwards in full-NAT mode. rivorad
// loads and attaches the tc_nat egress program only when this is true at
// startup, so it also decides whether a config reload may introduce NAT VIPs.
func HasNATVIP(vips []VIP) bool {
	for _, v := range vips {
		if v.Mode == ModeNAT {
			return true
		}
	}
	return false
}

// RestartRequired names the top-level settings that differ between the running
// config and a reloaded one but are only read once, at startup: the attached
// interface, the XDP attach mode, the API listener, health-check parameters, the
// SYN rate limit and the BGP speaker. A reload applies the VIP set only; callers report these so
// an operator isn't left believing an edit took effect.
func RestartRequired(running, next Config) []string {
	var changed []string
	if running.Interface != next.Interface {
		changed = append(changed, "interface")
	}
	if running.XDPMode.Effective() != next.XDPMode.Effective() {
		changed = append(changed, "xdpMode")
	}
	if running.APIListen != next.APIListen {
		changed = append(changed, "apiListen")
	}
	if running.HealthCheck != next.HealthCheck {
		changed = append(changed, "healthCheck")
	}
	if running.RateLimit != next.RateLimit {
		changed = append(changed, "rateLimit")
	}
	if !reflect.DeepEqual(running.BGP, next.BGP) {
		changed = append(changed, "bgp")
	}
	return changed
}

func defaults() Config {
	return Config{
		XDPMode:   XDPGeneric,
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
	if err := cfg.expandPorts(); err != nil {
		return cfg, err
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// expandPorts turns the file-only conveniences into plain VIPs: portRange
// ("30000-30100") into Port/PortEnd, and ports ([80, 443]) into one VIP per port
// that shares the rest of the entry. The expanded VIPs share nothing mutable.
func (c *Config) expandPorts() error {
	var out []VIP
	for i, v := range c.VIPs {
		if len(v.Ports) == 0 && v.PortRange == "" {
			out = append(out, v)
			continue
		}
		where := fmt.Sprintf("vip #%d (%s)", i+1, v.Address)
		if len(v.Ports) > 0 && v.PortRange != "" {
			return fmt.Errorf("%s: ports and portRange are mutually exclusive", where)
		}
		if v.Port != 0 || v.PortEnd != 0 {
			return fmt.Errorf("%s: port/portEnd cannot be combined with ports or portRange", where)
		}
		if v.PortRange != "" {
			lo, hi, err := parsePortRange(v.PortRange)
			if err != nil {
				return fmt.Errorf("%s: %w", where, err)
			}
			v.Port, v.PortEnd, v.PortRange = lo, hi, ""
			out = append(out, v)
			continue
		}
		seen := map[uint16]bool{}
		for _, p := range v.Ports {
			if p == 0 {
				return fmt.Errorf("%s: ports must be 1-65535", where)
			}
			if seen[p] {
				return fmt.Errorf("%s: port %d is listed twice", where, p)
			}
			seen[p] = true
			one := v
			one.Ports = nil
			one.Port = p
			one.Backends = append([]Backend(nil), v.Backends...)
			out = append(out, one)
		}
	}
	c.VIPs = out
	return nil
}

// parsePortRange reads "lo-hi" with 1 <= lo < hi <= 65535.
func parsePortRange(s string) (lo, hi uint16, err error) {
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return 0, 0, fmt.Errorf("portRange %q: want first-last, e.g. 30000-30100", s)
	}
	l, err1 := strconv.ParseUint(strings.TrimSpace(a), 10, 16)
	h, err2 := strconv.ParseUint(strings.TrimSpace(b), 10, 16)
	if err1 != nil || err2 != nil || l == 0 || h == 0 {
		return 0, 0, fmt.Errorf("portRange %q: ports must be numbers from 1 to 65535", s)
	}
	if l >= h {
		return 0, 0, fmt.Errorf("portRange %q: the first port must be below the last (use port for a single port)", s)
	}
	return uint16(l), uint16(h), nil
}

func (c Config) Validate() error {
	if c.Interface == "" {
		return fmt.Errorf("interface is required")
	}
	if err := c.XDPMode.Validate(); err != nil {
		return err
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
		if len(v.Ports) > 0 || v.PortRange != "" {
			return fmt.Errorf("vip %s: ports/portRange are only read from a config file (Load expands them)", v.Address)
		}
		key := fmt.Sprintf("%s:%s:%s", v.Address, v.PortLabel(), v.Protocol)
		if seen[key] {
			return fmt.Errorf("vip %s:%s/%s: duplicate VIP", v.Address, v.PortLabel(), v.Protocol)
		}
		seen[key] = true
	}
	if err := c.checkRanges(); err != nil {
		return err
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
		if v.IsRange() {
			if v.PortEnd <= v.Port || v.Port == 0 {
				return fmt.Errorf("vip %s:%s: a port range needs 1 <= port < portEnd", v.Address, v.PortLabel())
			}
			if v.HealthCheck.Port == 0 {
				return fmt.Errorf("vip %s:%s: healthCheck.port is required for a port range (its backends have no single port to probe)", v.Address, v.PortLabel())
			}
			for _, b := range v.Backends {
				if b.Port != 0 {
					return fmt.Errorf("vip %s:%s: backend %s must not set a port: a port range reaches the backend on the port the client used", v.Address, v.PortLabel(), b.Address)
				}
			}
		}
		if err := v.SessionAffinity.Validate(); err != nil {
			return fmt.Errorf("vip %s:%d: %w", v.Address, v.Port, err)
		}
		if err := v.HealthCheck.Validate(); err != nil {
			return fmt.Errorf("vip %s:%d: %w", v.Address, v.Port, err)
		}
		if err := v.RateLimit.Validate(); err != nil {
			return fmt.Errorf("vip %s:%d: %w", v.Address, v.Port, err)
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
	return c.checkSharedBackendProbes()
}

// checkSharedBackendProbes rejects two VIPs that list the same backend address
// and port but ask for different probes. The health checker probes each backend
// once and its result is shared by every VIP that uses it, so conflicting
// settings can't both be honoured; failing here beats one silently winning.
func (c Config) checkSharedBackendProbes() error {
	type use struct {
		vip   string
		probe ProbeSpec
	}
	first := map[string]use{}
	for _, v := range c.VIPs {
		vipName := fmt.Sprintf("%s:%d/%s", v.Address, v.Port, v.Protocol)
		probe := v.HealthCheck.Effective()
		for _, b := range v.Backends {
			key := fmt.Sprintf("%s:%d", b.Address, b.Port)
			prev, seen := first[key]
			if !seen {
				first[key] = use{vip: vipName, probe: probe}
				continue
			}
			if prev.probe != probe {
				return fmt.Errorf("backend %s is used by vip %s and vip %s with different healthCheck settings; a backend is probed once, so they must agree", key, prev.vip, vipName)
			}
		}
	}
	return nil
}

// checkRanges rejects two range VIPs on the same address and protocol whose ports
// overlap: which one owns an overlapping port would be a coin toss. An exact-port
// VIP inside a range is fine and intentional, and takes precedence.
func (c Config) checkRanges() error {
	type span struct {
		lo, hi uint16
		name   string
	}
	byAddr := map[string][]span{}
	for _, v := range c.VIPs {
		if !v.IsRange() {
			continue
		}
		ip := net.ParseIP(v.Address)
		if ip == nil {
			continue // reported by the per-VIP checks
		}
		k := ip.String() + "/" + string(v.Protocol)
		name := v.Address + ":" + v.PortLabel()
		for _, o := range byAddr[k] {
			if v.Port <= o.hi && o.lo <= v.PortEnd {
				return fmt.Errorf("vip %s overlaps vip %s/%s on the same address; port ranges must not overlap", name, o.name, v.Protocol)
			}
		}
		byAddr[k] = append(byAddr[k], span{v.Port, v.PortEnd, name})
	}
	return nil
}
