// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package config

import (
	"os"
	"strings"
	"testing"
)

func validConfig() Config {
	return Config{
		Interface: "eth0",
		VIPs: []VIP{
			{
				Address:  "10.0.0.100",
				Port:     80,
				Protocol: ProtoTCP,
				Mode:     ModeNAT,
				Backends: []Backend{{Address: "10.0.0.11", Port: 8080}},
			},
		},
	}
}

func TestValidateAcceptsRateLimitDisabled(t *testing.T) {
	cfg := validConfig() // RateLimit left at its zero value: Enabled == false
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected disabled RateLimit (the default) to be valid, got: %v", err)
	}
}

func TestValidateAcceptsRateLimitEnabledWithPositiveValues(t *testing.T) {
	cfg := validConfig()
	cfg.RateLimit = RateLimit{Enabled: true, PerSourcePacketsPerSecond: 1000, Burst: 2000}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected a fully-specified enabled RateLimit to be valid, got: %v", err)
	}
}

func TestValidateRejectsRateLimitEnabledWithZeroRate(t *testing.T) {
	cfg := validConfig()
	cfg.RateLimit = RateLimit{Enabled: true, PerSourcePacketsPerSecond: 0, Burst: 2000}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an error for rateLimit.enabled with perSourcePacketsPerSecond == 0")
	}
}

func TestValidateRejectsRateLimitEnabledWithZeroBurst(t *testing.T) {
	cfg := validConfig()
	cfg.RateLimit = RateLimit{Enabled: true, PerSourcePacketsPerSecond: 1000, Burst: 0}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an error for rateLimit.enabled with burst == 0")
	}
}

func TestValidateAcceptsBGPDisabled(t *testing.T) {
	cfg := validConfig() // BGP left at its zero value: Enabled == false
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected disabled BGP (the default) to be valid, got: %v", err)
	}
}

func TestValidateAcceptsBGPEnabledWithPeers(t *testing.T) {
	cfg := validConfig()
	cfg.BGP = BGP{
		Enabled:  true,
		ASN:      65001,
		RouterID: "10.0.0.1",
		Peers:    []BGPPeer{{Address: "10.0.0.2", ASN: 65000, BFD: true}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected a fully-specified enabled BGP to be valid, got: %v", err)
	}
}

func TestValidateRejectsBGPEnabledWithZeroASN(t *testing.T) {
	cfg := validConfig()
	cfg.BGP = BGP{Enabled: true, ASN: 0, RouterID: "10.0.0.1", Peers: []BGPPeer{{Address: "10.0.0.2", ASN: 65000}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an error for bgp.enabled with asn == 0")
	}
}

func TestValidateRejectsBGPEnabledWithInvalidRouterID(t *testing.T) {
	cfg := validConfig()
	cfg.BGP = BGP{Enabled: true, ASN: 65001, RouterID: "not-an-ip", Peers: []BGPPeer{{Address: "10.0.0.2", ASN: 65000}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an error for bgp.enabled with an invalid routerId")
	}
}

func TestValidateRejectsBGPEnabledWithNoPeers(t *testing.T) {
	cfg := validConfig()
	cfg.BGP = BGP{Enabled: true, ASN: 65001, RouterID: "10.0.0.1"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an error for bgp.enabled with no peers")
	}
}

func TestValidateRejectsBGPPeerWithInvalidAddress(t *testing.T) {
	cfg := validConfig()
	cfg.BGP = BGP{Enabled: true, ASN: 65001, RouterID: "10.0.0.1", Peers: []BGPPeer{{Address: "not-an-ip", ASN: 65000}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an error for a bgp peer with an invalid address")
	}
}

func TestValidateRejectsBGPPeerWithZeroASN(t *testing.T) {
	cfg := validConfig()
	cfg.BGP = BGP{Enabled: true, ASN: 65001, RouterID: "10.0.0.1", Peers: []BGPPeer{{Address: "10.0.0.2", ASN: 0}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an error for a bgp peer with asn == 0")
	}
}

func TestValidateAcceptsIPv6VIP(t *testing.T) {
	cfg := validConfig()
	cfg.VIPs = []VIP{{
		Address:  "fd00:77::100",
		Port:     80,
		Protocol: ProtoTCP,
		Mode:     ModeNAT,
		Backends: []Backend{{Address: "fd00:77::11", Port: 8080}},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected an all-IPv6 VIP+backend to be valid, got: %v", err)
	}
}

func TestValidateRejectsMixedFamilyBackend(t *testing.T) {
	cfg := validConfig()
	cfg.VIPs = []VIP{{
		Address:  "10.0.0.100", // IPv4 VIP
		Port:     80,
		Protocol: ProtoTCP,
		Mode:     ModeNAT,
		Backends: []Backend{{Address: "fd00:77::11", Port: 8080}}, // IPv6 backend
	}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an error for a v4 VIP with a v6 backend")
	}
}

func TestValidateRejectsIPv6VIPWithIPv4Backend(t *testing.T) {
	cfg := validConfig()
	cfg.VIPs = []VIP{{
		Address:  "fd00:77::100", // IPv6 VIP
		Port:     80,
		Protocol: ProtoTCP,
		Mode:     ModeNAT,
		Backends: []Backend{{Address: "10.0.0.11", Port: 8080}}, // IPv4 backend
	}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an error for a v6 VIP with a v4 backend")
	}
}

func TestHasNATVIP(t *testing.T) {
	if HasNATVIP(nil) {
		t.Error("no VIPs cannot need NAT")
	}
	if HasNATVIP([]VIP{{Mode: ModeDSR}, {Mode: ModeDSR}}) {
		t.Error("DSR-only VIPs reported as needing NAT")
	}
	if !HasNATVIP([]VIP{{Mode: ModeDSR}, {Mode: ModeNAT}}) {
		t.Error("a NAT VIP among DSR ones was missed")
	}
}

func TestRestartRequired(t *testing.T) {
	base := Config{
		Interface: "eth0", APIListen: "127.0.0.1:9870",
		HealthCheck: HealthCheck{FailThreshold: 2, SuccessThreshold: 2},
		BGP:         BGP{Enabled: true, ASN: 65001, Peers: []BGPPeer{{Address: "10.0.0.1", ASN: 65000}}},
	}
	if got := RestartRequired(base, base); len(got) != 0 {
		t.Errorf("identical configs reported %v", got)
	}

	// VIP edits are exactly what a reload applies, so they must not appear.
	withVIPs := base
	withVIPs.VIPs = []VIP{{Address: "10.0.0.1", Port: 80}}
	if got := RestartRequired(base, withVIPs); len(got) != 0 {
		t.Errorf("VIP-only change reported %v; reload handles those", got)
	}

	next := base
	next.Interface = "eth1"
	next.XDPMode = XDPNative
	next.APIListen = "0.0.0.0:9870"
	next.HealthCheck.FailThreshold = 5
	next.RateLimit.Enabled = true
	next.BGP = BGP{Enabled: true, ASN: 65001, Peers: []BGPPeer{{Address: "10.0.0.2", ASN: 65000}}}
	got := RestartRequired(base, next)
	want := []string{"interface", "xdpMode", "apiListen", "healthCheck", "rateLimit", "bgp"}
	if len(got) != len(want) {
		t.Fatalf("RestartRequired = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("RestartRequired[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestXDPModeValidation(t *testing.T) {
	for _, m := range []XDPMode{XDPGeneric, XDPNative, XDPAuto} {
		if err := m.Validate(); err != nil {
			t.Errorf("%q rejected: %v", m, err)
		}
	}
	for _, m := range []XDPMode{"driver", "GENERIC", "offload", "skb"} {
		if err := m.Validate(); err == nil {
			t.Errorf("%q accepted; only generic, native and auto are modes", m)
		}
	}
}

func TestXDPModeEffective(t *testing.T) {
	// A Config built in code (zero value) must behave as generic, not as invalid.
	if got := XDPMode("").Effective(); got != XDPGeneric {
		t.Errorf("empty mode effective = %q, want generic", got)
	}
	if err := XDPMode("").Validate(); err != nil {
		t.Errorf("the zero value must validate as the default, got %v", err)
	}
	for _, m := range []XDPMode{XDPGeneric, XDPNative, XDPAuto} {
		if m.Effective() != m {
			t.Errorf("%q changed by Effective()", m)
		}
	}
}

func TestXDPModeDefaultsToGenericAndLoads(t *testing.T) {
	// Unset means generic: the only mode that works on every interface, so an
	// existing config keeps doing exactly what it did.
	if got := defaults().XDPMode; got != XDPGeneric {
		t.Errorf("default xdpMode = %q, want generic", got)
	}
	dir := t.TempDir()
	write := func(name, extra string) string {
		p := dir + "/" + name
		body := "interface: eth0\n" + extra + "vips:\n  - {address: 10.0.0.1, port: 80, protocol: tcp, mode: nat, backends: [{address: 10.1.0.1, port: 80}]}\n"
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cfg, err := Load(write("unset.yaml", ""))
	if err != nil || cfg.XDPMode != XDPGeneric {
		t.Fatalf("unset: mode %q, err %v; want generic", cfg.XDPMode, err)
	}
	cfg, err = Load(write("native.yaml", "xdpMode: native\n"))
	if err != nil || cfg.XDPMode != XDPNative {
		t.Fatalf("native: mode %q, err %v", cfg.XDPMode, err)
	}
	if _, err := Load(write("bad.yaml", "xdpMode: driver\n")); err == nil {
		t.Error("an unknown xdpMode loaded; a typo must fail at start-up, not silently mean generic")
	}
}

func TestProbeSpecEffectiveDefaults(t *testing.T) {
	if got := (ProbeSpec{}).Effective(); got.Type != ProbeTCP {
		t.Errorf("zero spec = %+v, want a plain tcp probe (the legacy behaviour)", got)
	}
	h := ProbeSpec{Type: ProbeHTTP}.Effective()
	if h.Path != "/" || h.ExpectStatus != DefaultExpectStatus {
		t.Errorf("http defaults = path %q status %q, want / and %s", h.Path, h.ExpectStatus, DefaultExpectStatus)
	}
	// Explicit-default and unset must compare equal, or the shared-backend
	// conflict check would flag a VIP that spells out what another leaves unset.
	if (ProbeSpec{Type: ProbeHTTP}).Effective() != (ProbeSpec{Type: ProbeHTTP, Path: "/", ExpectStatus: "200-399"}).Effective() {
		t.Error("an http spec with defaults spelled out differs from one with them unset")
	}
}

func TestProbeSpecStatusRange(t *testing.T) {
	for _, c := range []struct {
		in     string
		lo, hi int
	}{
		{"", 200, 399}, {"200", 200, 200}, {"200-299", 200, 299}, {" 204 - 206 ", 204, 206}, {"500-599", 500, 599},
	} {
		lo, hi, err := ProbeSpec{Type: ProbeHTTP, ExpectStatus: c.in}.StatusRange()
		if err != nil || lo != c.lo || hi != c.hi {
			t.Errorf("%q -> %d-%d, %v; want %d-%d", c.in, lo, hi, err, c.lo, c.hi)
		}
	}
	for _, bad := range []string{"abc", "99", "600", "300-200", "200-", "-200", "200-abc", "2xx"} {
		if _, _, err := (ProbeSpec{Type: ProbeHTTP, ExpectStatus: bad}).StatusRange(); err == nil {
			t.Errorf("expectStatus %q accepted", bad)
		}
	}
}

func TestProbeSpecValidate(t *testing.T) {
	ok := []ProbeSpec{
		{}, {Type: ProbeTCP}, {Type: ProbeTCP, Port: 8081},
		{Type: ProbeHTTP}, {Type: ProbeHTTP, Path: "/healthz?deep=1", Host: "app.example", ExpectStatus: "200", Port: 9000},
	}
	for _, p := range ok {
		if err := p.Validate(); err != nil {
			t.Errorf("%+v rejected: %v", p, err)
		}
	}
	bad := map[string]ProbeSpec{
		"unknown type":            {Type: "grpc"},
		"http path without slash": {Type: ProbeHTTP, Path: "healthz"},
		"path with a space":       {Type: ProbeHTTP, Path: "/a b"},
		"host with a slash":       {Type: ProbeHTTP, Host: "a/b"},
		"bad status":              {Type: ProbeHTTP, ExpectStatus: "ok"},
		"tcp with a path":         {Type: ProbeTCP, Path: "/x"},
		"unset type with a path":  {Path: "/x"}, // a path means http was intended: don't silently ignore it
		"tcp with a status":       {ExpectStatus: "200"},
	}
	for name, p := range bad {
		if err := p.Validate(); err == nil {
			t.Errorf("%s: %+v accepted", name, p)
		}
	}
}

func loadYAML(t *testing.T, body string) (Config, error) {
	t.Helper()
	p := t.TempDir() + "/c.yaml"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

func TestLoadParsesPerVIPHealthCheck(t *testing.T) {
	cfg, err := loadYAML(t, `
interface: eth0
vips:
  - address: 10.0.0.1
    port: 80
    protocol: tcp
    mode: nat
    healthCheck: {type: http, path: /ready, expectStatus: 200, port: 9000, host: app.local}
    backends: [{address: 10.1.0.1, port: 80}]
  - address: 10.0.0.2
    port: 80
    protocol: tcp
    mode: nat
    backends: [{address: 10.1.0.2, port: 80}]
`)
	if err != nil {
		t.Fatal(err)
	}
	want := ProbeSpec{Type: ProbeHTTP, Path: "/ready", ExpectStatus: "200", Port: 9000, Host: "app.local"}
	if cfg.VIPs[0].HealthCheck != want {
		t.Errorf("vip 0 healthCheck = %+v, want %+v", cfg.VIPs[0].HealthCheck, want)
	}
	if cfg.VIPs[1].HealthCheck.Effective().Type != ProbeTCP {
		t.Errorf("a VIP with no healthCheck must probe with tcp, got %+v", cfg.VIPs[1].HealthCheck.Effective())
	}
}

func TestLoadRejectsABadHealthCheck(t *testing.T) {
	_, err := loadYAML(t, `
interface: eth0
vips:
  - address: 10.0.0.1
    port: 80
    protocol: tcp
    mode: nat
    healthCheck: {type: http, path: healthz}
    backends: [{address: 10.1.0.1, port: 80}]
`)
	if err == nil || !strings.Contains(err.Error(), "path") {
		t.Errorf("err = %v, want a complaint about the path", err)
	}
}

func TestSharedBackendMustAgreeOnItsProbe(t *testing.T) {
	vip := func(addr string, hc ProbeSpec) VIP {
		return VIP{Address: addr, Port: 80, Protocol: ProtoTCP, Mode: ModeNAT, HealthCheck: hc,
			Backends: []Backend{{Address: "10.1.0.1", Port: 8080}}}
	}
	mk := func(vips ...VIP) Config { return Config{Interface: "eth0", VIPs: vips} }

	// Same backend, one VIP http and one tcp: cannot both be honoured.
	err := mk(vip("10.0.0.1", ProbeSpec{Type: ProbeHTTP}), vip("10.0.0.2", ProbeSpec{})).Validate()
	if err == nil || !strings.Contains(err.Error(), "10.1.0.1:8080") || !strings.Contains(err.Error(), "must agree") {
		t.Errorf("conflicting probes accepted or unclear: %v", err)
	}
	// Different path is a conflict too.
	if err := mk(vip("10.0.0.1", ProbeSpec{Type: ProbeHTTP, Path: "/a"}), vip("10.0.0.2", ProbeSpec{Type: ProbeHTTP, Path: "/b"})).Validate(); err == nil {
		t.Error("two different http paths for one backend were accepted")
	}
	// Agreeing — including one spelling out the defaults — is fine.
	if err := mk(vip("10.0.0.1", ProbeSpec{Type: ProbeHTTP}), vip("10.0.0.2", ProbeSpec{Type: ProbeHTTP, Path: "/", ExpectStatus: "200-399"})).Validate(); err != nil {
		t.Errorf("identical effective probes rejected: %v", err)
	}
	// Different backends may of course differ.
	other := vip("10.0.0.2", ProbeSpec{})
	other.Backends[0].Address = "10.1.0.9"
	if err := mk(vip("10.0.0.1", ProbeSpec{Type: ProbeHTTP}), other).Validate(); err != nil {
		t.Errorf("different backends with different probes rejected: %v", err)
	}
}

func TestSessionAffinityValidation(t *testing.T) {
	for _, a := range []SessionAffinity{"", AffinityNone, AffinityClientIP} {
		if err := a.Validate(); err != nil {
			t.Errorf("%q rejected: %v", a, err)
		}
	}
	for _, a := range []SessionAffinity{"ClientIP", "clientip", "source", "true", "sticky"} {
		if err := a.Validate(); err == nil {
			t.Errorf("%q accepted; the spellings are none and clientIP", a)
		}
	}
	if SessionAffinity("").Effective() != AffinityNone {
		t.Error("unset must mean none")
	}
}

func TestLoadParsesSessionAffinity(t *testing.T) {
	cfg, err := loadYAML(t, `
interface: eth0
vips:
  - {address: 10.0.0.1, port: 80, protocol: tcp, mode: nat, sessionAffinity: clientIP, backends: [{address: 10.1.0.1, port: 80}]}
  - {address: 10.0.0.2, port: 80, protocol: tcp, mode: nat, backends: [{address: 10.1.0.2, port: 80}]}
`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VIPs[0].SessionAffinity != AffinityClientIP {
		t.Errorf("vip 0 sessionAffinity = %q", cfg.VIPs[0].SessionAffinity)
	}
	if cfg.VIPs[1].SessionAffinity.Effective() != AffinityNone {
		t.Errorf("an unset sessionAffinity must be none, got %q", cfg.VIPs[1].SessionAffinity)
	}
	if _, err := loadYAML(t, `
interface: eth0
vips:
  - {address: 10.0.0.1, port: 80, protocol: tcp, mode: nat, sessionAffinity: ClientIP, backends: [{address: 10.1.0.1, port: 80}]}
`); err == nil {
		t.Error("a mis-cased sessionAffinity loaded; a typo must fail, not silently mean none")
	}
}

func TestVIPRateLimitValidation(t *testing.T) {
	for _, c := range []struct {
		name string
		rl   VIPRateLimit
		ok   bool
	}{
		{"unset", VIPRateLimit{}, true},
		{"both set", VIPRateLimit{PerSourcePacketsPerSecond: 5, Burst: 10}, true},
		{"rate only", VIPRateLimit{PerSourcePacketsPerSecond: 5}, false},
		{"burst only", VIPRateLimit{Burst: 10}, false},
	} {
		cfg := validConfig()
		cfg.VIPs[0].RateLimit = c.rl
		if err := cfg.Validate(); (err == nil) != c.ok {
			t.Errorf("%s: Validate() = %v, want ok=%v", c.name, err, c.ok)
		}
	}
	if (VIPRateLimit{}).Set() {
		t.Error("the zero value must mean 'not set'")
	}
}

func TestLoadParsesPerVIPRateLimit(t *testing.T) {
	path := t.TempDir() + "/c.yaml"
	body := `interface: eth0
vips:
  - {address: 10.0.0.100, port: 80, protocol: tcp, mode: nat, backends: [{address: 10.0.0.11, port: 8080}],
     rateLimit: {perSourcePacketsPerSecond: 25, burst: 50}}
  - {address: 10.0.0.101, port: 80, protocol: tcp, mode: nat, backends: [{address: 10.0.0.12, port: 8080}]}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.VIPs[0].RateLimit; got.PerSourcePacketsPerSecond != 25 || got.Burst != 50 {
		t.Errorf("vip 0 rateLimit = %+v", got)
	}
	if cfg.VIPs[1].RateLimit.Set() {
		t.Error("a VIP without rateLimit must leave it unset")
	}
}

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	path := t.TempDir() + "/c.yaml"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadExpandsPortsAndPortRange(t *testing.T) {
	cfg, err := Load(writeCfg(t, `interface: eth0
vips:
  - {address: 10.0.0.100, ports: [80, 443], protocol: tcp, mode: nat, backends: [{address: 10.0.0.11, port: 8080}]}
  - {address: 10.0.0.101, portRange: "30000-30100", protocol: udp, mode: nat,
     healthCheck: {port: 9000}, backends: [{address: 10.0.0.12}]}
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.VIPs) != 3 {
		t.Fatalf("got %d VIPs, want 3 (two from ports, one range): %+v", len(cfg.VIPs), cfg.VIPs)
	}
	if cfg.VIPs[0].Port != 80 || cfg.VIPs[1].Port != 443 || cfg.VIPs[0].IsRange() {
		t.Errorf("ports not expanded to exact-port VIPs: %+v %+v", cfg.VIPs[0], cfg.VIPs[1])
	}
	if cfg.VIPs[0].Ports != nil || cfg.VIPs[2].PortRange != "" {
		t.Error("the file-only fields must be cleared after Load")
	}
	if r := cfg.VIPs[2]; !r.IsRange() || r.Port != 30000 || r.PortEnd != 30100 || r.PortLabel() != "30000-30100" {
		t.Errorf("portRange not parsed: %+v", r)
	}
	// Expanded VIPs must not alias one backend slice: editing one must not change the other.
	cfg.VIPs[0].Backends[0].Weight = 9
	if cfg.VIPs[1].Backends[0].Weight != 0 {
		t.Error("expanded VIPs share a Backends slice")
	}
}

func TestPortRangeConfigErrors(t *testing.T) {
	base := func(vip string) string {
		return "interface: eth0\nvips:\n  - {address: 10.0.0.100, protocol: tcp, mode: nat, " + vip + "}\n"
	}
	be := `backends: [{address: 10.0.0.11, port: 8080}]`
	rbe := `healthCheck: {port: 9000}, backends: [{address: 10.0.0.11}]`
	for _, c := range []struct{ name, body, want string }{
		{"ports and portRange", base(`ports: [80], portRange: "1-5", ` + be), "mutually exclusive"},
		{"ports with port", base(`port: 80, ports: [81], ` + be), "cannot be combined"},
		{"duplicate ports", base(`ports: [80, 80], ` + be), "listed twice"},
		{"zero port in ports", base(`ports: [0], ` + be), "1-65535"},
		{"bad range text", base(`portRange: "30000", ` + rbe), "want first-last"},
		{"reversed range", base(`portRange: "30100-30000", ` + rbe), "first port must be below"},
		{"range zero", base(`portRange: "0-10", ` + rbe), "1 to 65535"},
		{"range without probe port", base(`portRange: "1000-1010", backends: [{address: 10.0.0.11}]`), "healthCheck.port is required"},
		{"range backend with port", base(`portRange: "1000-1010", healthCheck: {port: 9000}, backends: [{address: 10.0.0.11, port: 80}]`), "must not set a port"},
		{"portEnd not above port", base(`port: 80, portEnd: 80, ` + rbe), "1 <= port < portEnd"},
	} {
		_, err := Load(writeCfg(t, c.body))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %v, want it to contain %q", c.name, err, c.want)
		}
	}
}

func TestPortRangesMustNotOverlapButExactPortsMayNest(t *testing.T) {
	mk := func(vips ...VIP) Config {
		cfg := Config{Interface: "eth0", VIPs: vips}
		return cfg
	}
	rng := func(lo, hi uint16, proto Protocol) VIP {
		return VIP{Address: "10.0.0.100", Port: lo, PortEnd: hi, Protocol: proto, Mode: ModeNAT,
			HealthCheck: ProbeSpec{Port: 9000}, Backends: []Backend{{Address: "10.0.0.11"}}}
	}
	exact := VIP{Address: "10.0.0.100", Port: 30050, Protocol: ProtoTCP, Mode: ModeNAT, Backends: []Backend{{Address: "10.0.0.12", Port: 22}}}

	if err := mk(rng(30000, 30100, ProtoTCP), rng(30100, 30200, ProtoTCP)).Validate(); err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Errorf("overlapping ranges: %v", err)
	}
	if err := mk(rng(30000, 30100, ProtoTCP), rng(30101, 30200, ProtoTCP)).Validate(); err != nil {
		t.Errorf("adjacent ranges must be fine: %v", err)
	}
	if err := mk(rng(30000, 30100, ProtoTCP), rng(30000, 30100, ProtoUDP)).Validate(); err != nil {
		t.Errorf("the same range on tcp and udp is two VIPs: %v", err)
	}
	if err := mk(rng(30000, 30100, ProtoTCP), exact).Validate(); err != nil {
		t.Errorf("an exact-port VIP inside a range must be allowed: %v", err)
	}
	if err := mk(rng(30000, 30100, ProtoTCP), rng(30000, 30100, ProtoTCP)).Validate(); err == nil {
		t.Error("the same range twice must be rejected")
	}
}

func TestVIPContainsAndLabel(t *testing.T) {
	r := VIP{Port: 100, PortEnd: 200}
	for p, want := range map[uint16]bool{99: false, 100: true, 150: true, 200: true, 201: false} {
		if r.Contains(p) != want {
			t.Errorf("range Contains(%d) = %v, want %v", p, !want, want)
		}
	}
	if s := (VIP{Port: 80}); !s.Contains(80) || s.Contains(81) || s.PortLabel() != "80" {
		t.Errorf("single-port VIP: %+v label %q", s, s.PortLabel())
	}
}

func TestSharedBackendMACsMustAgree(t *testing.T) {
	mk := func(macA, macB string) Config {
		be := func(mac string) []Backend { return []Backend{{Address: "10.0.0.11", Port: 80, MAC: mac}} }
		return Config{Interface: "eth0", VIPs: []VIP{
			{Address: "10.0.0.100", Port: 80, Protocol: ProtoTCP, Mode: ModeDSR, Backends: be(macA)},
			{Address: "10.0.0.101", Port: 80, Protocol: ProtoTCP, Mode: ModeNAT, Backends: be(macB)},
		}}
	}
	if err := mk("aa:bb:cc:dd:ee:01", "").Validate(); err != nil {
		t.Errorf("a NAT VIP sharing a DSR VIP's backend without naming a MAC must be fine: %v", err)
	}
	if err := mk("aa:bb:cc:dd:ee:01", "AA:BB:CC:DD:EE:01").Validate(); err != nil {
		t.Errorf("the same MAC written two ways must agree: %v", err)
	}
	if err := mk("aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02").Validate(); err == nil || !strings.Contains(err.Error(), "one MAC") {
		t.Errorf("two different MACs for one backend must be rejected, got %v", err)
	}
}

func TestTunnelModes(t *testing.T) {
	mk := func(mode Mode, mac string) Config {
		return Config{Interface: "eth0", VIPs: []VIP{{Address: "10.0.0.100", Port: 80, Protocol: ProtoTCP, Mode: mode,
			Backends: []Backend{{Address: "10.9.0.11", Port: 80, MAC: mac}}}}}
	}
	for _, m := range []Mode{ModeDSRIPIP, ModeDSRGRE} {
		if err := mk(m, "").Validate(); err != nil {
			t.Errorf("%s: a tunnel VIP needs no MAC: %v", m, err)
		}
		if err := mk(m, "aa:bb:cc:dd:ee:01").Validate(); err == nil || !strings.Contains(err.Error(), "only applies to mode dsr") {
			t.Errorf("%s: a MAC on a tunnel backend must be rejected, got %v", m, err)
		}
		if !m.IsTunnel() || !m.Valid() {
			t.Errorf("%s must be a valid tunnel mode", m)
		}
	}
	if err := mk("dsr-vxlan", "").Validate(); err == nil || !strings.Contains(err.Error(), "dsr-ipip") {
		t.Errorf("an unknown mode must be rejected and name the tunnel modes, got %v", err)
	}
	if ModeDSR.IsTunnel() || ModeNAT.IsTunnel() {
		t.Error("dsr and nat are not tunnel modes")
	}
	// A DSR (L2) backend still needs its MAC.
	if err := mk(ModeDSR, "").Validate(); err == nil {
		t.Error("mode dsr without a MAC must still be rejected")
	}
}

func TestTunnelSourceValidation(t *testing.T) {
	base := func() Config {
		c := validConfig()
		return c
	}
	c := base()
	c.TunnelSource, c.TunnelSource6 = "192.0.2.1", "2001:db8::1"
	if err := c.Validate(); err != nil {
		t.Errorf("valid sources rejected: %v", err)
	}
	for _, bad := range []struct{ v4, v6, want string }{
		{"2001:db8::1", "", "tunnelSource"},
		{"nonsense", "", "tunnelSource"},
		{"", "192.0.2.1", "tunnelSource6"},
		{"", "nonsense", "tunnelSource6"},
	} {
		c := base()
		c.TunnelSource, c.TunnelSource6 = bad.v4, bad.v6
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("v4=%q v6=%q: got %v, want an error naming %s", bad.v4, bad.v6, err, bad.want)
		}
	}
	// Changing a tunnel source needs a restart, like the interface.
	a, b := base(), base()
	b.TunnelSource = "192.0.2.9"
	if got := RestartRequired(a, b); len(got) != 1 || got[0] != "tunnelSource" {
		t.Errorf("RestartRequired = %v, want [tunnelSource]", got)
	}
}

func TestParseCommunities(t *testing.T) {
	got, err := ParseCommunities([]string{"65000:100", "no-export", " NO-ADVERTISE ", "0:0", "65535:65535"})
	if err != nil {
		t.Fatal(err)
	}
	want := []uint32{65000<<16 | 100, 0xFFFFFF01, 0xFFFFFF02, 0, 0xFFFFFFFF}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("community %d = %#x, want %#x", i, got[i], want[i])
		}
	}
	for _, bad := range []string{"", "65000", "65536:1", "1:65536", "a:b", "no-such-name", "1:2:3", "-1:1"} {
		if _, err := ParseCommunities([]string{bad}); err == nil {
			t.Errorf("community %q must be rejected", bad)
		}
	}
}

func TestBGPPeerOptionValidation(t *testing.T) {
	mk := func(p BGPPeer) BGP {
		p.Address, p.ASN = "192.0.2.1", 65100
		return BGP{Enabled: true, ASN: 65000, RouterID: "1.1.1.1", Peers: []BGPPeer{p}}
	}
	ok := func(name string, p BGPPeer) {
		if err := mk(p).Validate(); err != nil {
			t.Errorf("%s: unexpected error %v", name, err)
		}
	}
	bad := func(name string, p BGPPeer, want string) {
		if err := mk(p).Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %v, want it to contain %q", name, err, want)
		}
	}
	ok("password", BGPPeer{Password: "s3cret"})
	ok("password file", BGPPeer{PasswordFile: "/run/secrets/bgp"})
	ok("multihop", BGPPeer{Multihop: 4})
	ok("graceful restart", BGPPeer{GracefulRestart: &BGPGracefulRestart{Enabled: true, RestartTime: 300}})
	bad("both password forms", BGPPeer{Password: "a", PasswordFile: "/x"}, "not both")
	bad("password too long", BGPPeer{Password: strings.Repeat("x", 81)}, "80 bytes")
	bad("multihop 1", BGPPeer{Multihop: 1}, "2-255")
	bad("multihop 256", BGPPeer{Multihop: 256}, "2-255")
	bad("restart time too big", BGPPeer{GracefulRestart: &BGPGracefulRestart{Enabled: true, RestartTime: 5000}}, "4095")
	// Multihop is an eBGP setting: a peer in our own AS is iBGP.
	ibgp := mk(BGPPeer{Multihop: 3})
	ibgp.Peers[0].ASN = 65000
	if err := ibgp.Validate(); err == nil || !strings.Contains(err.Error(), "eBGP") {
		t.Errorf("multihop on an iBGP peer must be rejected, got %v", err)
	}
}

func TestBGPAggregatesAndCommunitiesValidate(t *testing.T) {
	base := func() BGP {
		return BGP{Enabled: true, ASN: 65000, RouterID: "1.1.1.1", Peers: []BGPPeer{{Address: "192.0.2.1", ASN: 65100}}}
	}
	b := base()
	b.Communities = []string{"65000:1", "no-export"}
	b.Aggregates = []BGPAggregate{{Prefix: "192.0.2.0/24", SuppressSpecifics: true, Communities: []string{"65000:2"}}, {Prefix: "2001:db8::/48"}}
	if err := b.Validate(); err != nil {
		t.Errorf("valid options rejected: %v", err)
	}
	b = base()
	b.Communities = []string{"nonsense"}
	if err := b.Validate(); err == nil {
		t.Error("a bad global community must be rejected")
	}
	b = base()
	b.Aggregates = []BGPAggregate{{Prefix: "192.0.2.0"}}
	if err := b.Validate(); err == nil || !strings.Contains(err.Error(), "aggregate") {
		t.Errorf("an aggregate that is not a prefix must be rejected, got %v", err)
	}
	// A VIP's own communities are validated with the config.
	c := validConfig()
	c.VIPs[0].BGPCommunities = []string{"65000:x"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "bgpCommunities") {
		t.Errorf("a bad VIP community must be rejected, got %v", err)
	}
}

func TestBGPForNodeFiltersPeers(t *testing.T) {
	b := BGP{Peers: []BGPPeer{
		{Address: "10.0.0.1", ASN: 1},                            // everywhere
		{Address: "10.0.1.1", ASN: 1, Nodes: []string{"a", "b"}}, // only a and b
		{Address: "10.0.2.1", ASN: 1, Nodes: []string{"c"}},      // only c
	}}
	names := func(x BGP) []string {
		var out []string
		for _, p := range x.Peers {
			out = append(out, p.Address)
		}
		return out
	}
	for node, want := range map[string]string{
		"a": "10.0.0.1 10.0.1.1", "c": "10.0.0.1 10.0.2.1", "z": "10.0.0.1", "": "10.0.0.1",
	} {
		if got := strings.Join(names(b.ForNode(node)), " "); got != want {
			t.Errorf("ForNode(%q) = %q, want %q", node, got, want)
		}
	}
	if len(b.Peers) != 3 {
		t.Error("ForNode must not modify the receiver")
	}
}

func TestParsePeerAddrs(t *testing.T) {
	got, err := ParsePeerAddrs([]string{"192.0.2.2", "2001:DB8::1", "192.0.2.2", "::ffff:192.0.2.1"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"192.0.2.1", "192.0.2.2", "2001:db8::1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ParsePeerAddrs = %v, want %v (canonical, sorted, no repeats, v4-mapped unmapped)", got, want)
	}
	if out, err := ParsePeerAddrs(nil); out != nil || err != nil {
		t.Errorf("no peers should mean no limit: %v %v", out, err)
	}
	if _, err := ParsePeerAddrs([]string{"192.0.2.1", "tor-1"}); err == nil {
		t.Error("a name that is not an address must be rejected")
	}
}

func TestVIPBGPPeersMustBeConfiguredPeers(t *testing.T) {
	c := validConfig()
	c.BGP = BGP{Enabled: true, ASN: 65000, RouterID: "1.1.1.1", Peers: []BGPPeer{{Address: "192.0.2.1", ASN: 65100}, {Address: "2001:db8::1", ASN: 65100}}}
	c.VIPs[0].BGPPeers = []string{"192.0.2.1", "2001:DB8::1"}
	if err := c.Validate(); err != nil {
		t.Errorf("configured peers (in any spelling) rejected: %v", err)
	}
	c.VIPs[0].BGPPeers = []string{"192.0.2.9"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "bgpPeers") || !strings.Contains(err.Error(), "192.0.2.9") {
		t.Errorf("a peer that is not configured must be named and rejected, got %v", err)
	}
	c.VIPs[0].BGPPeers = []string{"not-an-ip"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "bgpPeers") {
		t.Errorf("a non-address must be rejected, got %v", err)
	}
	// Peers that arrive at run time (BGPPeer resources) cannot be checked here.
	c.VIPs[0].BGPPeers = []string{"192.0.2.9"}
	c.BGP.PeersFromResources = true
	if err := c.Validate(); err != nil {
		t.Errorf("with peers coming from resources the list cannot be checked: %v", err)
	}
	// An aggregate's list is checked the same way.
	b := BGP{Enabled: true, ASN: 65000, RouterID: "1.1.1.1", Peers: []BGPPeer{{Address: "192.0.2.1", ASN: 65100}},
		Aggregates: []BGPAggregate{{Prefix: "10.0.0.0/24", Peers: []string{"192.0.2.9"}}}}
	if err := b.Validate(); err == nil || !strings.Contains(err.Error(), "aggregate") {
		t.Errorf("an aggregate limited to an unknown peer must be rejected, got %v", err)
	}
}

func TestBGPWithoutPeersNeedsResources(t *testing.T) {
	b := BGP{Enabled: true, ASN: 65000, RouterID: "1.1.1.1"}
	if err := b.Validate(); err == nil {
		t.Error("BGP with no peers at all must be rejected")
	}
	b.PeersFromResources = true
	if err := b.Validate(); err != nil {
		t.Errorf("BGP with peers to come from resources is valid: %v", err)
	}
}
