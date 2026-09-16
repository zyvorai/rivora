// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package config

import "testing"

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
