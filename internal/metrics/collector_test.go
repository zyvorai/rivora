// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package metrics

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/zyvorai/rivora/internal/dataplane"
)

type fakeSource struct {
	statuses []dataplane.Status
	err      error
}

func (f fakeSource) Statuses() ([]dataplane.Status, error) { return f.statuses, f.err }

func TestDataplaneCollectorEmitsExpectedMetrics(t *testing.T) {
	src := fakeSource{statuses: []dataplane.Status{
		{
			VIPAddress: "10.0.0.1", VIPPort: 80, Protocol: "tcp", Mode: "nat",
			Packets: 100, Bytes: 5000, Dropped: 3,
			Backends: []dataplane.BackendStatus{
				{Address: "10.0.1.1", Port: 8080, Weight: 1, Healthy: true, Packets: 60, Bytes: 3000},
				{Address: "10.0.1.2", Port: 8080, Weight: 2, Healthy: false, Packets: 40, Bytes: 2000},
			},
		},
	}}

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewDataplaneCollector(src))

	want := `
# HELP rivora_vip_backends Number of backends currently configured behind a VIP.
# TYPE rivora_vip_backends gauge
rivora_vip_backends{mode="nat",protocol="tcp",vip="10.0.0.1:80"} 2
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "rivora_vip_backends"); err != nil {
		t.Errorf("rivora_vip_backends: %v", err)
	}

	wantHealthy := `
# HELP rivora_backend_healthy Whether the active health checker currently considers a backend healthy (1) or not (0).
# TYPE rivora_backend_healthy gauge
rivora_backend_healthy{backend="10.0.1.1:8080",vip="10.0.0.1:80"} 1
rivora_backend_healthy{backend="10.0.1.2:8080",vip="10.0.0.1:80"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(wantHealthy), "rivora_backend_healthy"); err != nil {
		t.Errorf("rivora_backend_healthy: %v", err)
	}
}

func TestDataplaneCollectorDropsCountedOnce(t *testing.T) {
	src := fakeSource{statuses: []dataplane.Status{
		{VIPAddress: "10.0.0.1", VIPPort: 80, Protocol: "tcp", Mode: "dsr", Dropped: 7},
		{VIPAddress: "10.0.0.2", VIPPort: 443, Protocol: "tcp", Mode: "dsr", Dropped: 7},
	}}

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewDataplaneCollector(src))

	want := `
# HELP rivora_dropped_packets_total Node-wide packets dropped by the dataplane (not attributable to a single VIP).
# TYPE rivora_dropped_packets_total counter
rivora_dropped_packets_total 7
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "rivora_dropped_packets_total"); err != nil {
		t.Errorf("rivora_dropped_packets_total should be reported once despite two VIPs: %v", err)
	}
}

func TestDataplaneCollectorReportsScrapeErrors(t *testing.T) {
	src := fakeSource{err: errors.New("read bpf map: boom")}

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewDataplaneCollector(src))

	want := `
# HELP rivora_dataplane_scrape_errors_total Whether the last scrape of the dataplane's BPF maps failed (1) or succeeded (0).
# TYPE rivora_dataplane_scrape_errors_total gauge
rivora_dataplane_scrape_errors_total 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "rivora_dataplane_scrape_errors_total"); err != nil {
		t.Errorf("rivora_dataplane_scrape_errors_total: %v", err)
	}
}

// ctSource is a fakeSource that also reports flow-table usage and counts how
// often it was asked, so the cache can be observed.
type ctSource struct {
	fakeSource
	calls int
	usage []dataplane.MapUsage
}

func (c *ctSource) ConntrackUsage() []dataplane.MapUsage {
	c.calls++
	return c.usage
}

func TestBackendDrainingMetric(t *testing.T) {
	src := fakeSource{statuses: []dataplane.Status{{
		VIPAddress: "10.0.0.1", VIPPort: 80, Protocol: "tcp", Mode: "nat",
		Backends: []dataplane.BackendStatus{
			{Address: "10.0.1.1", Port: 8080, State: "healthy", Healthy: true},
			{Address: "10.0.1.2", Port: 8080, State: "draining"},
			{Address: "10.0.1.3", Port: 8080, State: "down"},
		},
	}}}
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewDataplaneCollector(src))

	want := `
# HELP rivora_backend_draining Whether a backend is draining (1): it takes no new flows but established ones continue. Set by an operator drain or a terminating Kubernetes endpoint.
# TYPE rivora_backend_draining gauge
rivora_backend_draining{backend="10.0.1.1:8080",vip="10.0.0.1:80"} 0
rivora_backend_draining{backend="10.0.1.2:8080",vip="10.0.0.1:80"} 1
rivora_backend_draining{backend="10.0.1.3:8080",vip="10.0.0.1:80"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "rivora_backend_draining"); err != nil {
		t.Errorf("rivora_backend_draining: %v", err)
	}
}

func TestConntrackMetrics(t *testing.T) {
	src := &ctSource{usage: []dataplane.MapUsage{
		{Table: "affinity", Entries: 1200, Capacity: 65536},
		{Table: "nat_reverse", Entries: 64000, Capacity: 65536},
	}}
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewDataplaneCollector(src))

	wantEntries := `
# HELP rivora_conntrack_entries Live entries in a flow table (affinity, nat_reverse, and their IPv6 siblings). Sampled at most every 10s.
# TYPE rivora_conntrack_entries gauge
rivora_conntrack_entries{table="affinity"} 1200
rivora_conntrack_entries{table="nat_reverse"} 64000
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(wantEntries), "rivora_conntrack_entries"); err != nil {
		t.Errorf("rivora_conntrack_entries: %v", err)
	}
	wantCap := `
# HELP rivora_conntrack_capacity Maximum entries in a flow table. These are LRU maps: once entries reaches capacity, live flows are evicted and may be re-hashed onto a different backend.
# TYPE rivora_conntrack_capacity gauge
rivora_conntrack_capacity{table="affinity"} 65536
rivora_conntrack_capacity{table="nat_reverse"} 65536
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(wantCap), "rivora_conntrack_capacity"); err != nil {
		t.Errorf("rivora_conntrack_capacity: %v", err)
	}
}

func TestConntrackAbsentWhenSourceCannotReportIt(t *testing.T) {
	// A plain StatusSource (no ConntrackUsage) must not panic and must not
	// emit half-populated conntrack series.
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewDataplaneCollector(fakeSource{}))
	n, err := testutil.GatherAndCount(reg, "rivora_conntrack_entries", "rivora_conntrack_capacity")
	if err != nil || n != 0 {
		t.Errorf("GatherAndCount = %d, %v; want 0 series, nil", n, err)
	}
}

func TestConntrackWalkIsCached(t *testing.T) {
	src := &ctSource{usage: []dataplane.MapUsage{{Table: "affinity", Entries: 1, Capacity: 10}}}
	c := NewDataplaneCollector(src)
	clock := time.Unix(1_000_000, 0)
	c.now = func() time.Time { return clock }

	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	scrape := func() {
		t.Helper()
		if _, err := reg.Gather(); err != nil {
			t.Fatal(err)
		}
	}

	scrape()
	scrape()
	clock = clock.Add(conntrackCacheTTL - time.Second)
	scrape()
	if src.calls != 1 {
		t.Fatalf("flow tables walked %d times within the TTL, want 1 (walk is O(entries))", src.calls)
	}

	clock = clock.Add(2 * time.Second) // now past the TTL
	scrape()
	if src.calls != 2 {
		t.Errorf("flow tables walked %d times after TTL expiry, want 2", src.calls)
	}
}
