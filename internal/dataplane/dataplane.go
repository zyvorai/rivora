// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package dataplane wires rivorad's static config into the loaded BPF maps:
// VIP/service/backend/maglev population at startup, and backend health
// updates as the health checker reports transitions. It's also the source
// of truth the local HTTP API (internal/api) reads status from.
package dataplane

import (
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"github.com/cilium/ebpf"

	"github.com/zyvorai/rivora/internal/bpfmaps"
	"github.com/zyvorai/rivora/internal/config"
	"github.com/zyvorai/rivora/internal/loader"
	"github.com/zyvorai/rivora/internal/maglev"
)

const serviceID = 0 // v0.1: exactly one VIP per node

type BackendStatus struct {
	ID      uint32 `json:"id"`
	Address string `json:"address"`
	Port    uint16 `json:"port"`
	Healthy bool   `json:"healthy"`
	Packets uint64 `json:"packets"`
	Bytes   uint64 `json:"bytes"`
}

type Status struct {
	VIPAddress string          `json:"vipAddress"`
	VIPPort    uint16          `json:"vipPort"`
	Protocol   string          `json:"protocol"`
	Mode       string          `json:"mode"`
	Interface  string          `json:"interface"`
	StartedAt  time.Time       `json:"startedAt"`
	Backends   []BackendStatus `json:"backends"`
	Packets    uint64          `json:"packets"`
	Bytes      uint64          `json:"bytes"`
	Dropped    uint64          `json:"dropped"`
}

type Dataplane struct {
	cfg       config.Config
	dp        *loader.Datapath
	startedAt time.Time

	backendIDs map[string]uint32 // "addr:port" -> backend_id
}

func New(cfg config.Config, dp *loader.Datapath) *Dataplane {
	return &Dataplane{cfg: cfg, dp: dp, startedAt: time.Now(), backendIDs: map[string]uint32{}}
}

// Apply writes the full config into the BPF maps: VIP, service config,
// backends, maglev table and (for DSR) the attached interface's own MAC.
func (d *Dataplane) Apply(iface *net.Interface) error {
	vip := d.cfg.VIPs[0]

	vipIP := net.ParseIP(vip.Address).To4()
	if vipIP == nil {
		return fmt.Errorf("vip %s is not IPv4 — v0.1 is IPv4-only", vip.Address)
	}
	proto := uint8(6)
	if vip.Protocol == config.ProtoUDP {
		proto = 17
	}
	mode := uint8(bpfmaps.ModeDSR)
	if vip.Mode == config.ModeNAT {
		mode = bpfmaps.ModeNAT
	}

	names := make([]string, len(vip.Backends))
	for i, b := range vip.Backends {
		names[i] = fmt.Sprintf("%s:%d", b.Address, b.Port)
	}
	table, err := maglev.BuildTable(bpfmaps.MaglevM, names)
	if err != nil {
		return fmt.Errorf("build maglev table: %w", err)
	}

	vipMap := d.dp.Maps[bpfmaps.MapVIP]
	svcMap := d.dp.Maps[bpfmaps.MapServiceConfig]
	backendMap := d.dp.Maps[bpfmaps.MapBackend]
	healthMap := d.dp.Maps[bpfmaps.MapBackendHealth]
	maglevMap := d.dp.Maps[bpfmaps.MapMaglevTable]
	ifaceMACMap := d.dp.Maps[bpfmaps.MapIfaceMAC]

	vk := bpfmaps.VipKey{Addr: ip4ToBE32(vipIP), Port: htons(vip.Port), Proto: proto}
	if err := vipMap.Update(&vk, uint32(serviceID), ebpf.UpdateAny); err != nil {
		return fmt.Errorf("update vip_map: %w", err)
	}

	sc := bpfmaps.ServiceConfig{BackendCount: uint32(len(vip.Backends)), MaglevOffset: 0, Mode: mode}
	if err := svcMap.Update(uint32(serviceID), &sc, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("update service_config_map: %w", err)
	}

	for i, b := range vip.Backends {
		id := uint32(i)
		beIP := net.ParseIP(b.Address).To4()
		if beIP == nil {
			return fmt.Errorf("backend %s is not IPv4", b.Address)
		}
		var mac [6]byte
		if b.MAC != "" {
			hw, err := net.ParseMAC(b.MAC)
			if err != nil {
				return fmt.Errorf("backend %s: %w", b.Address, err)
			}
			copy(mac[:], hw)
		}
		bi := bpfmaps.BackendInfo{Addr: ip4ToBE32(beIP), Port: htons(b.Port), Mac: mac}
		if err := backendMap.Update(&id, &bi, ebpf.UpdateAny); err != nil {
			return fmt.Errorf("update backend_map[%d]: %w", id, err)
		}
		healthy := uint8(1) // start healthy; first failed probe removes it
		if err := healthMap.Update(&id, &healthy, ebpf.UpdateAny); err != nil {
			return fmt.Errorf("update backend_health_map[%d]: %w", id, err)
		}
		d.backendIDs[names[i]] = id
	}

	for slot, backendIdx := range table {
		s := uint32(slot)
		b := uint32(backendIdx)
		if err := maglevMap.Update(&s, &b, ebpf.UpdateAny); err != nil {
			return fmt.Errorf("update maglev_table[%d]: %w", slot, err)
		}
	}

	if iface != nil && len(iface.HardwareAddr) == 6 {
		var mac [6]byte
		copy(mac[:], iface.HardwareAddr)
		ifidx := uint32(iface.Index)
		if err := ifaceMACMap.Update(&ifidx, &mac, ebpf.UpdateAny); err != nil {
			return fmt.Errorf("update iface_mac_map: %w", err)
		}
	}

	return nil
}

// SetBackendHealth updates backend_health_map for a single backend.
func (d *Dataplane) SetBackendHealth(backendID uint32, healthy bool) error {
	healthMap := d.dp.Maps[bpfmaps.MapBackendHealth]
	v := uint8(0)
	if healthy {
		v = 1
	}
	return healthMap.Update(&backendID, &v, ebpf.UpdateAny)
}

// Targets returns the backend set for the health checker.
func (d *Dataplane) Targets() []struct {
	ID      uint32
	Address string
	Port    uint16
} {
	vip := d.cfg.VIPs[0]
	out := make([]struct {
		ID      uint32
		Address string
		Port    uint16
	}, len(vip.Backends))
	for i, b := range vip.Backends {
		out[i] = struct {
			ID      uint32
			Address string
			Port    uint16
		}{ID: uint32(i), Address: b.Address, Port: b.Port}
	}
	return out
}

func (d *Dataplane) Status() (Status, error) {
	vip := d.cfg.VIPs[0]
	healthMap := d.dp.Maps[bpfmaps.MapBackendHealth]
	statsMap := d.dp.Maps[bpfmaps.MapStats]

	st := Status{
		VIPAddress: vip.Address,
		VIPPort:    vip.Port,
		Protocol:   string(vip.Protocol),
		Mode:       string(vip.Mode),
		Interface:  d.cfg.Interface,
		StartedAt:  d.startedAt,
	}

	if s, err := sumStats(statsMap, bpfmaps.StatsGlobalIdx); err == nil {
		st.Packets, st.Bytes, st.Dropped = s.Packets, s.Bytes, s.Dropped
	}

	for i, b := range vip.Backends {
		id := uint32(i)
		var healthy uint8
		_ = healthMap.Lookup(&id, &healthy)
		bs := BackendStatus{ID: id, Address: b.Address, Port: b.Port, Healthy: healthy == 1}
		if s, err := sumStats(statsMap, 1+id); err == nil {
			bs.Packets, bs.Bytes = s.Packets, s.Bytes
		}
		st.Backends = append(st.Backends, bs)
	}

	return st, nil
}

func sumStats(m *ebpf.Map, idx uint32) (bpfmaps.LBStats, error) {
	var perCPU []bpfmaps.LBStats
	if err := m.Lookup(&idx, &perCPU); err != nil {
		return bpfmaps.LBStats{}, err
	}
	var total bpfmaps.LBStats
	for _, s := range perCPU {
		total.Packets += s.Packets
		total.Bytes += s.Bytes
		total.Dropped += s.Dropped
	}
	return total, nil
}

// ip4ToBE32 converts an IPv4 address into the uint32 that, once cilium/ebpf
// serializes it in the host's native (little-endian) byte order for the BPF
// map, reproduces the address's raw network-order bytes — the same bytes
// iphdr->daddr holds on the wire. This is binary.LittleEndian, not
// BigEndian, precisely because the map encoding is little-endian: reading
// network-order bytes back with LittleEndian.Uint32 is what undoes it.
func ip4ToBE32(ip net.IP) uint32 { return binary.LittleEndian.Uint32(ip) }

// htons mirrors C's htons on a little-endian host: the byte-swapped value,
// once serialized little-endian by cilium/ebpf, reproduces the port's
// network-order bytes — matching what tcphdr/udphdr's source/dest hold.
func htons(port uint16) uint16 { return (port << 8) | (port >> 8) }
