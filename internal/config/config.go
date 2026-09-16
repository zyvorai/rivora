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

type Config struct {
	Interface   string      `yaml:"interface"`
	APIListen   string      `yaml:"apiListen"`
	HealthCheck HealthCheck `yaml:"healthCheck"`
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
	if len(c.VIPs) == 0 {
		return fmt.Errorf("at least one VIP is required")
	}
	if len(c.VIPs) > 1 {
		return fmt.Errorf("v0.1 supports exactly one VIP per node (got %d)", len(c.VIPs))
	}
	for _, v := range c.VIPs {
		if net.ParseIP(v.Address) == nil {
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
		for _, b := range v.Backends {
			if net.ParseIP(b.Address) == nil {
				return fmt.Errorf("backend %q: invalid address", b.Address)
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
