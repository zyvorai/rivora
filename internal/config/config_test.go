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
