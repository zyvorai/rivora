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
