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
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/zyvorai/rivora/internal/dataplane"
)

// StatusSource is the subset of *dataplane.Dataplane the collector needs —
// narrowed to an interface so it can be exercised with a fake in tests
// without loading real BPF maps.
type StatusSource interface {
	Statuses() ([]dataplane.Status, error)
}

// ConntrackSource is optionally implemented by a StatusSource (the real
// *dataplane.Dataplane does) to report flow-table occupancy. Kept separate so
// a source that can't — or a test fake that doesn't care — needn't provide it.
type ConntrackSource interface {
	ConntrackUsage() []dataplane.MapUsage
}

// conntrackCacheTTL bounds how often the flow tables are walked. Counting
// entries is O(table size) syscalls, so scraping it every few seconds from
// several Prometheus replicas would be wasteful; occupancy moves slowly enough
// that a 10s-old value is as good for alerting.
const conntrackCacheTTL = 10 * time.Second

// DataplaneCollector implements prometheus.Collector over a StatusSource.
type DataplaneCollector struct {
	src StatusSource

	ct       ConntrackSource // nil when src doesn't provide flow-table usage
	now      func() time.Time
	ctMu     sync.Mutex
	ctAt     time.Time
	ctCached []dataplane.MapUsage

	backendDraining *prometheus.Desc
	vipDropped      *prometheus.Desc
	vipUnserved     *prometheus.Desc
	ctEntries       *prometheus.Desc
	ctCapacity      *prometheus.Desc

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
	c := &DataplaneCollector{
		src: src,
		now: time.Now,
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
		vipDropped: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "vip", "dropped_packets_total"),
			"Packets dropped for a VIP, by reason: rate_limited (a SYN over the per-source limit) or no_healthy_backend (nothing to send it to). Their sum across VIPs equals rivora_dropped_packets_total.",
			append(append([]string{}, vipLabels...), "reason"), nil,
		),
		vipUnserved: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "vip", "unserved_packets_total"),
			"Packets that matched a VIP but could not be served (no service configuration or backend entry), so they passed to the kernel stack instead of being load-balanced. Not drops.",
			vipLabels, nil,
		),
		backendDraining: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "backend", "draining"),
			"Whether a backend is draining (1): it takes no new flows but established ones continue. Set by an operator drain or a terminating Kubernetes endpoint.",
			backendLabels, nil,
		),
		ctEntries: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "conntrack", "entries"),
			"Live entries in a flow table (affinity, nat_reverse, and their IPv6 siblings). Sampled at most every 10s.",
			[]string{"table"}, nil,
		),
		ctCapacity: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "conntrack", "capacity"),
			"Maximum entries in a flow table. These are LRU maps: once entries reaches capacity, live flows are evicted and may be re-hashed onto a different backend.",
			[]string{"table"}, nil,
		),
	}
	if cs, ok := src.(ConntrackSource); ok {
		c.ct = cs
	}
	return c
}

// conntrackUsage returns the flow-table usage, walking the tables at most once
// per conntrackCacheTTL. The lock is held across the walk on purpose: two
// concurrent scrapes should share one walk, not each start their own.
func (c *DataplaneCollector) conntrackUsage() []dataplane.MapUsage {
	c.ctMu.Lock()
	defer c.ctMu.Unlock()
	if c.ctCached != nil && c.now().Sub(c.ctAt) < conntrackCacheTTL {
		return c.ctCached
	}
	c.ctCached = c.ct.ConntrackUsage()
	if c.ctCached == nil {
		c.ctCached = []dataplane.MapUsage{} // cache "no tables" too
	}
	c.ctAt = c.now()
	return c.ctCached
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
	ch <- c.backendDraining
	ch <- c.vipDropped
	ch <- c.vipUnserved
	ch <- c.ctEntries
	ch <- c.ctCapacity
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
		ch <- prometheus.MustNewConstMetric(c.vipDropped, prometheus.CounterValue, float64(st.DroppedRateLimited), vip, st.Protocol, st.Mode, "rate_limited")
		ch <- prometheus.MustNewConstMetric(c.vipDropped, prometheus.CounterValue, float64(st.DroppedNoBackend), vip, st.Protocol, st.Mode, "no_healthy_backend")
		ch <- prometheus.MustNewConstMetric(c.vipUnserved, prometheus.CounterValue, float64(st.Unserved), vip, st.Protocol, st.Mode)

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
			draining := 0.0
			if b.State == "draining" {
				draining = 1.0
			}
			ch <- prometheus.MustNewConstMetric(c.backendDraining, prometheus.GaugeValue, draining, vip, backend)
		}
	}

	if c.ct != nil {
		for _, u := range c.conntrackUsage() {
			ch <- prometheus.MustNewConstMetric(c.ctEntries, prometheus.GaugeValue, float64(u.Entries), u.Table)
			ch <- prometheus.MustNewConstMetric(c.ctCapacity, prometheus.GaugeValue, float64(u.Capacity), u.Table)
		}
	}
}

func vipLabel(address string, port uint16) string {
	return fmt.Sprintf("%s:%d", address, port)
}
