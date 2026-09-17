// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package metrics exposes rivorad's dataplane state as Prometheus metrics.
// It reads on every scrape (the same on-demand pattern internal/api already
// uses for /api/v1/status) rather than maintaining its own counters, since
// internal/dataplane.Statuses already aggregates the BPF maps' per-CPU
// packet/byte/health state — there's no separate value to keep in sync.
package metrics

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/zyvorai/rivora/internal/dataplane"
)

// StatusSource is the subset of *dataplane.Dataplane the collector needs —
// narrowed to an interface so it can be exercised with a fake in tests
// without loading real BPF maps.
type StatusSource interface {
	Statuses() ([]dataplane.Status, error)
}

// DataplaneCollector implements prometheus.Collector over a StatusSource.
type DataplaneCollector struct {
	src StatusSource

	vipInfo        *prometheus.Desc
	vipBackends    *prometheus.Desc
	vipPackets     *prometheus.Desc
	vipBytes       *prometheus.Desc
	droppedPackets *prometheus.Desc
	backendPackets *prometheus.Desc
	backendBytes   *prometheus.Desc
	backendHealthy *prometheus.Desc
	backendWeight  *prometheus.Desc
	scrapeErrors   *prometheus.Desc
}

// NewDataplaneCollector wraps src for registration on a prometheus.Registry.
func NewDataplaneCollector(src StatusSource) *DataplaneCollector {
	const ns = "rivora"
	vipLabels := []string{"vip", "protocol", "mode"}
	backendLabels := []string{"vip", "backend"}
	return &DataplaneCollector{
		src: src,
		vipInfo: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "vip", "info"),
			"Static info for a configured VIP; always 1.",
			vipLabels, nil,
		),
		vipBackends: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "vip", "backends"),
			"Number of backends currently configured behind a VIP.",
			vipLabels, nil,
		),
		vipPackets: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "vip", "packets_total"),
			"Packets forwarded for a VIP since its BPF maps were pinned.",
			vipLabels, nil,
		),
		vipBytes: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "vip", "bytes_total"),
			"Bytes forwarded for a VIP since its BPF maps were pinned.",
			vipLabels, nil,
		),
		droppedPackets: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "", "dropped_packets_total"),
			"Node-wide packets dropped by the dataplane (not attributable to a single VIP).",
			nil, nil,
		),
		backendPackets: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "backend", "packets_total"),
			"Packets forwarded to a backend since its BPF maps were pinned.",
			backendLabels, nil,
		),
		backendBytes: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "backend", "bytes_total"),
			"Bytes forwarded to a backend since its BPF maps were pinned.",
			backendLabels, nil,
		),
		backendHealthy: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "backend", "healthy"),
			"Whether the active health checker currently considers a backend healthy (1) or not (0).",
			backendLabels, nil,
		),
		backendWeight: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "backend", "weight"),
			"Configured Maglev traffic-share weight for a backend.",
			backendLabels, nil,
		),
		scrapeErrors: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "", "dataplane_scrape_errors_total"),
			"Whether the last scrape of the dataplane's BPF maps failed (1) or succeeded (0).",
			nil, nil,
		),
	}
}

func (c *DataplaneCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.vipInfo
	ch <- c.vipBackends
	ch <- c.vipPackets
	ch <- c.vipBytes
	ch <- c.droppedPackets
	ch <- c.backendPackets
	ch <- c.backendBytes
	ch <- c.backendHealthy
	ch <- c.backendWeight
	ch <- c.scrapeErrors
}

func (c *DataplaneCollector) Collect(ch chan<- prometheus.Metric) {
	statuses, err := c.src.Statuses()
	if err != nil {
		ch <- prometheus.MustNewConstMetric(c.scrapeErrors, prometheus.GaugeValue, 1)
		return
	}
	ch <- prometheus.MustNewConstMetric(c.scrapeErrors, prometheus.GaugeValue, 0)

	for i, st := range statuses {
		vip := vipLabel(st.VIPAddress, st.VIPPort)
		ch <- prometheus.MustNewConstMetric(c.vipInfo, prometheus.GaugeValue, 1, vip, st.Protocol, st.Mode)
		ch <- prometheus.MustNewConstMetric(c.vipBackends, prometheus.GaugeValue, float64(len(st.Backends)), vip, st.Protocol, st.Mode)
		ch <- prometheus.MustNewConstMetric(c.vipPackets, prometheus.CounterValue, float64(st.Packets), vip, st.Protocol, st.Mode)
		ch <- prometheus.MustNewConstMetric(c.vipBytes, prometheus.CounterValue, float64(st.Bytes), vip, st.Protocol, st.Mode)

		// Dropped is the same node-wide counter on every Status (see
		// dataplane.Status's Dropped field doc) — report it once, not once
		// per VIP, so summing across VIPs in PromQL doesn't multiply it.
		if i == 0 {
			ch <- prometheus.MustNewConstMetric(c.droppedPackets, prometheus.CounterValue, float64(st.Dropped))
		}

		for _, b := range st.Backends {
			backend := vipLabel(b.Address, b.Port)
			healthy := 0.0
			if b.Healthy {
				healthy = 1.0
			}
			ch <- prometheus.MustNewConstMetric(c.backendPackets, prometheus.CounterValue, float64(b.Packets), vip, backend)
			ch <- prometheus.MustNewConstMetric(c.backendBytes, prometheus.CounterValue, float64(b.Bytes), vip, backend)
			ch <- prometheus.MustNewConstMetric(c.backendHealthy, prometheus.GaugeValue, healthy, vip, backend)
			ch <- prometheus.MustNewConstMetric(c.backendWeight, prometheus.GaugeValue, float64(b.Weight), vip, backend)
		}
	}
}

func vipLabel(address string, port uint16) string {
	return fmt.Sprintf("%s:%d", address, port)
}
