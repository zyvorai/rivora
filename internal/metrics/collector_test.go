// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package metrics

import (
	"errors"
	"strings"
	"testing"

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
