// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package config loads rivorad's static YAML configuration (v0.1: no
// control plane, one file per node — see config/examples/single-vip.yaml).
package config

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Mode string

const (
	// ModeDSR rewrites the frame's MAC and sends it straight back out: the backend must be on
	// the load balancer's own L2 segment and own the VIP locally.
	ModeDSR Mode = "dsr"
	ModeNAT Mode = "nat"
	// ModeDSRIPIP and ModeDSRGRE are "L3 DSR": the packet is tunnelled to the backend
	// (IP-in-IP, or GRE) and the backend, which owns the VIP locally, unwraps it and answers the
	// client directly. The backend only has to be routable from the load balancer, not on its L2
	// segment. IPv4 VIPs get an IPv4 outer header, IPv6 VIPs an IPv6 one. The path to the
	// backends must carry the extra 20 (24 with GRE, 40/44 for IPv6) bytes.
	ModeDSRIPIP Mode = "dsr-ipip"
	ModeDSRGRE  Mode = "dsr-gre"
)

// IsTunnel reports whether m encapsulates towards the backend.
func (m Mode) IsTunnel() bool { return m == ModeDSRIPIP || m == ModeDSRGRE }

// Valid reports whether m is a mode rivorad knows.
func (m Mode) Valid() bool { return m == ModeDSR || m == ModeNAT || m.IsTunnel() }

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
	// BGPCommunities are added to the BGP route advertised for this VIP (see BGP.Communities),
	// for example to tag an anycast VIP for a different upstream policy.
	BGPCommunities []string `yaml:"bgpCommunities,omitempty"`
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

	// Communities are attached to every route this speaker originates: "65000:100" style
	// (both halves 0-65535) or the well-known names no-export, no-advertise and
	// no-export-subconfed. A VIP's own bgpCommunities are added to these for that VIP's route.
	Communities []string `yaml:"communities,omitempty"`
	// LocalPref sets the LOCAL_PREF attribute on originated routes. It is only carried to iBGP
	// peers (peers in this speaker's own AS); an eBGP peer never receives it. Unset sends none.
	LocalPref *uint32 `yaml:"localPref,omitempty"`
	// Aggregates advertise a covering prefix (say a /24 of VIPs) while at least one VIP inside it
	// has a healthy backend, and withdraw it when none does.
	Aggregates []BGPAggregate `yaml:"aggregates,omitempty"`
}

// BGPAggregate is a prefix advertised in place of, or alongside, the /32 (/128) host routes of the
// VIPs it covers.
type BGPAggregate struct {
	Prefix string `yaml:"prefix"`
	// SuppressSpecifics stops the covered VIPs' host routes being advertised while this aggregate
	// is. Off, the aggregate is advertised in addition to them.
	SuppressSpecifics bool `yaml:"suppressSpecifics,omitempty"`
	// Communities are attached to the aggregate route (on top of the global ones).
	Communities []string `yaml:"communities,omitempty"`
}

type BGPPeer struct {
	Address string `yaml:"address"`
	ASN     uint32 `yaml:"asn"`
	// BFD enables Bidirectional Forwarding Detection on this peer session
	// for sub-second down detection, instead of relying solely on BGP's
	// own (much slower) hold-timer expiry.
	BFD bool `yaml:"bfd,omitempty"`

	// Password enables TCP MD5 authentication (RFC 2385) on the session; the peer must be
	// configured with the same one. PasswordFile reads it from a file instead (a mounted Secret),
	// so it need not sit in a config that gets committed or logged. Set one or neither.
	Password     string `yaml:"password,omitempty"`
	PasswordFile string `yaml:"passwordFile,omitempty"`
	// Multihop is the TTL for an eBGP session to a peer more than one hop away, 2-255. Unset
	// keeps the default, where an eBGP peer must be directly connected.
	Multihop uint32 `yaml:"multihop,omitempty"`
	// GracefulRestart negotiates BGP graceful restart (RFC 4724) so the peer keeps forwarding to
	// this node's routes for RestartTime seconds if the session drops, instead of withdrawing them
	// at once: a rivorad restart then does not black-hole its VIPs.
	GracefulRestart *BGPGracefulRestart `yaml:"gracefulRestart,omitempty"`
	// Nodes limits the peer to the named nodes (by -node-name / hostname). Unset applies it
	// everywhere, so one shared config can give each node its own top-of-rack peer.
	Nodes []string `yaml:"nodes,omitempty"`
}

// BGPGracefulRestart configures graceful restart on one peer.
type BGPGracefulRestart struct {
	Enabled bool `yaml:"enabled"`
	// RestartTime is how long, in seconds, the peer should hold this node's routes while it
	// restarts. Default 120; at most 4095 (the protocol's 12-bit field).
	RestartTime uint32 `yaml:"restartTime,omitempty"`
}

// Well-known BGP communities (RFC 1997 and RFC 8326-era names) accepted by name.
var wellKnownCommunities = map[string]uint32{
	"no-export":           0xFFFFFF01,
	"no-advertise":        0xFFFFFF02,
	"no-export-subconfed": 0xFFFFFF03,
}

// ParseCommunities converts "asn:value" (both 0-65535) and well-known names to their 32-bit
// values, in order. It is the single parser for every place a community is written.
func ParseCommunities(in []string) ([]uint32, error) {
	out := make([]uint32, 0, len(in))
	for _, c := range in {
		c = strings.TrimSpace(c)
		if v, ok := wellKnownCommunities[strings.ToLower(c)]; ok {
			out = append(out, v)
			continue
		}
		a, b, ok := strings.Cut(c, ":")
		hi, err1 := strconv.ParseUint(a, 10, 16)
		lo, err2 := strconv.ParseUint(b, 10, 16)
		if !ok || err1 != nil || err2 != nil {
			return nil, fmt.Errorf("community %q: want asn:value (0-65535 each) or one of no-export, no-advertise, no-export-subconfed", c)
		}
		out = append(out, uint32(hi)<<16|uint32(lo))
	}
	return out, nil
}

// ForNode returns b with only the peers that apply to node: a peer with no Nodes applies
// everywhere, one with Nodes only on those. With no node name known, a peer restricted to
// particular nodes is left out rather than guessed at.
func (b BGP) ForNode(node string) BGP {
	out := b
	out.Peers = nil
	for _, p := range b.Peers {
		if len(p.Nodes) == 0 {
			out.Peers = append(out.Peers, p)
			continue
		}
		for _, n := range p.Nodes {
			if node != "" && n == node {
				out.Peers = append(out.Peers, p)
				break
			}
		}
	}
	return out
}

// validateOptions checks a peer's optional settings. localASN is this speaker's own AS: multihop is an
// eBGP setting, so it is refused on a peer in the same AS.
func (p BGPPeer) validateOptions(localASN uint32) error {
	if p.Password != "" && p.PasswordFile != "" {
		return fmt.Errorf("set password or passwordFile, not both")
	}
	if len(p.Password) > 80 {
		return fmt.Errorf("password is longer than the 80 bytes TCP MD5 allows")
	}
	if p.Multihop != 0 {
		if p.Multihop < 2 || p.Multihop > 255 {
			return fmt.Errorf("multihop %d: must be 2-255 (leave it unset for a directly connected peer)", p.Multihop)
		}
		if p.ASN == localASN {
			return fmt.Errorf("multihop applies to eBGP; this peer is in the same AS (%d)", p.ASN)
		}
	}
	if g := p.GracefulRestart; g != nil && g.RestartTime > 4095 {
		return fmt.Errorf("gracefulRestart.restartTime %d: at most 4095 seconds", g.RestartTime)
	}
	return nil
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
		if err := p.validateOptions(b.ASN); err != nil {
			return fmt.Errorf("bgp peer %s: %w", p.Address, err)
		}
	}
	if _, err := ParseCommunities(b.Communities); err != nil {
		return fmt.Errorf("bgp: %w", err)
	}
	for _, a := range b.Aggregates {
		if _, err := netip.ParsePrefix(a.Prefix); err != nil {
			return fmt.Errorf("bgp aggregate %q: not a valid prefix: %w", a.Prefix, err)
		}
		if _, err := ParseCommunities(a.Communities); err != nil {
			return fmt.Errorf("bgp aggregate %s: %w", a.Prefix, err)
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
	XDPMode XDPMode `yaml:"xdpMode,omitempty"`
	// TunnelSource and TunnelSource6 are the source addresses of the outer header for the
	// dsr-ipip / dsr-gre modes, IPv4 and IPv6 respectively. Unset, each defaults to the
	// attached interface's own address of that family. It should be an address the backends can
	// route back to (they do not reply through the tunnel, so it only needs to be a plausible
	// source, and must not be filtered by their reverse-path checks).
	TunnelSource  string      `yaml:"tunnelSource,omitempty"`
	TunnelSource6 string      `yaml:"tunnelSource6,omitempty"`
	APIListen     string      `yaml:"apiListen"`
	HealthCheck   HealthCheck `yaml:"healthCheck"`
	RateLimit     RateLimit   `yaml:"rateLimit"`
	BGP           BGP         `yaml:"bgp"`
	VIPs          []VIP       `yaml:"vips"`
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

// HasTunnelVIP reports whether any of vips uses a tunnel mode (dsr-ipip / dsr-gre).
func HasTunnelVIP(vips []VIP) bool {
	for _, v := range vips {
		if v.Mode.IsTunnel() {
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
	if running.TunnelSource != next.TunnelSource || running.TunnelSource6 != next.TunnelSource6 {
		changed = append(changed, "tunnelSource")
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
	if err := c.validateTunnelSources(); err != nil {
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
		if !v.Mode.Valid() {
			return fmt.Errorf("vip %s:%d: mode must be dsr, nat, dsr-ipip or dsr-gre", v.Address, v.Port)
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
		if _, err := ParseCommunities(v.BGPCommunities); err != nil {
			return fmt.Errorf("vip %s:%d: bgpCommunities: %w", v.Address, v.Port, err)
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
			if v.Mode.IsTunnel() && b.MAC != "" {
				return fmt.Errorf("backend %s: mac only applies to mode dsr; a %s backend is reached by its address", b.Address, v.Mode)
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
	if err := c.checkSharedBackendMACs(); err != nil {
		return err
	}
	return c.checkSharedBackendProbes()
}

// checkSharedBackendMACs rejects two VIPs that give the same backend address and port
// different MACs. A backend has one MAC in the datapath, shared by every VIP that uses it,
// so they cannot both be honoured. A VIP that gives none (full-NAT) never conflicts: it
// simply shares the MAC the DSR VIP supplied.
func (c Config) checkSharedBackendMACs() error {
	type use struct{ vip, mac string }
	first := map[string]use{}
	for _, v := range c.VIPs {
		vipName := fmt.Sprintf("%s:%s/%s", v.Address, v.PortLabel(), v.Protocol)
		for _, b := range v.Backends {
			if b.MAC == "" {
				continue
			}
			hw, err := net.ParseMAC(b.MAC)
			if err != nil {
				continue // reported by the per-backend checks
			}
			key := fmt.Sprintf("%s:%d", b.Address, b.Port)
			if prev, seen := first[key]; seen && prev.mac != hw.String() {
				return fmt.Errorf("backend %s has mac %s in vip %s but %s in vip %s; a backend has one MAC", key, prev.mac, prev.vip, hw.String(), vipName)
			}
			first[key] = use{vipName, hw.String()}
		}
	}
	return nil
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

// validateTunnelSources checks tunnelSource is an IPv4 address and tunnelSource6 an IPv6 one.
func (c Config) validateTunnelSources() error {
	if c.TunnelSource != "" {
		if ip := net.ParseIP(c.TunnelSource); ip == nil || ip.To4() == nil {
			return fmt.Errorf("tunnelSource %q: must be an IPv4 address", c.TunnelSource)
		}
	}
	if c.TunnelSource6 != "" {
		if ip := net.ParseIP(c.TunnelSource6); ip == nil || ip.To4() != nil {
			return fmt.Errorf("tunnelSource6 %q: must be an IPv6 address", c.TunnelSource6)
		}
	}
	return nil
}

// LoadBGP reads a YAML file whose top-level bgp: section is a BGP block (the same schema as the
// static config's) and returns it enabled and validated. It exists so a Kubernetes deployment can
// mount BGP settings, including peer passwords, from a Secret instead of putting them on a command
// line.
func LoadBGP(path string) (BGP, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return BGP{}, fmt.Errorf("read bgp config: %w", err)
	}
	var doc struct {
		BGP BGP `yaml:"bgp"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return BGP{}, fmt.Errorf("parse bgp config: %w", err)
	}
	doc.BGP.Enabled = true
	if err := doc.BGP.Validate(); err != nil {
		return BGP{}, err
	}
	return doc.BGP, nil
}
