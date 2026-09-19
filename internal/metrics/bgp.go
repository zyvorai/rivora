// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package metrics

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/zyvorai/rivora/internal/bgp"
)

// BGPSource is the subset of *bgp.Speaker the collector needs, narrowed so it
// can be exercised with a fake.
type BGPSource interface {
	Snapshot() bgp.Snapshot
}

// BGPCollector exposes the BGP speaker's health. It is registered only when BGP
// is enabled, so a node without BGP has no rivora_bgp_* series at all rather than
// a misleading set of zeros.
type BGPCollector struct {
	src BGPSource

	peerUp       *prometheus.Desc
	stateChanges *prometheus.Desc
	advertised   *prometheus.Desc
	updateErrors *prometheus.Desc
}

func NewBGPCollector(src BGPSource) *BGPCollector {
	const ns = "rivora"
	peer := []string{"peer", "asn"}
	return &BGPCollector{
		src: src,
		peerUp: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "bgp", "peer_up"),
			"Whether the BGP session to a configured peer is ESTABLISHED (1) or not (0). A down session means that peer is not receiving this node's VIP routes.",
			peer, nil,
		),
		stateChanges: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "bgp", "peer_state_changes_total"),
			"BGP session-state transitions observed for a peer since rivorad started. A rising value on a peer that is mostly up is a flapping session.",
			[]string{"peer"}, nil,
		),
		advertised: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "bgp", "advertised_routes"),
			"VIP host routes currently advertised (a VIP is advertised only while it has a healthy backend), by address family.",
			[]string{"family"}, nil,
		),
		updateErrors: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "bgp", "route_update_errors_total"),
			"Failed attempts to advertise or withdraw a VIP route since rivorad started. A value that keeps rising means routes are not reaching the peers.",
			[]string{"op"}, nil,
		),
	}
}

func (c *BGPCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.peerUp
	ch <- c.stateChanges
	ch <- c.advertised
	ch <- c.updateErrors
}

func (c *BGPCollector) Collect(ch chan<- prometheus.Metric) {
	snap := c.src.Snapshot()
	for _, p := range snap.Peers {
		up := 0.0
		if p.Established {
			up = 1.0
		}
		ch <- prometheus.MustNewConstMetric(c.peerUp, prometheus.GaugeValue, up, p.Address, strconv.FormatUint(uint64(p.ASN), 10))
		ch <- prometheus.MustNewConstMetric(c.stateChanges, prometheus.CounterValue, float64(p.StateChanges), p.Address)
	}
	ch <- prometheus.MustNewConstMetric(c.advertised, prometheus.GaugeValue, float64(snap.AdvertisedIPv4), "ipv4")
	ch <- prometheus.MustNewConstMetric(c.advertised, prometheus.GaugeValue, float64(snap.AdvertisedIPv6), "ipv6")
	ch <- prometheus.MustNewConstMetric(c.updateErrors, prometheus.CounterValue, float64(snap.AdvertiseErrors), "advertise")
	ch <- prometheus.MustNewConstMetric(c.updateErrors, prometheus.CounterValue, float64(snap.WithdrawErrors), "withdraw")
}
