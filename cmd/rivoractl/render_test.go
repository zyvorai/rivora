// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/zyvorai/rivora/internal/dataplane"
)

func vip(addr string, port uint16, backends ...dataplane.BackendStatus) dataplane.Status {
	return dataplane.Status{VIPAddress: addr, VIPPort: port, Protocol: "tcp", Mode: "nat", Interface: "eth0", Backends: backends}
}

func be(id uint32, healthy bool) dataplane.BackendStatus {
	state := "down"
	if healthy {
		state = "healthy"
	}
	return dataplane.BackendStatus{ID: id, Address: "10.1.0.1", Port: 8080, Weight: 1, Healthy: healthy, State: state}
}

// A node with several VIPs used to get "N VIPs configured; use the vips list" from status. It now gets a
// line per VIP.
func TestStatusSummarisesSeveralVIPs(t *testing.T) {
	var out bytes.Buffer
	vips := []dataplane.Status{vip("10.0.0.1", 80, be(0, true), be(1, false)), vip("10.0.0.2", 443, be(2, true))}
	if err := renderStatus(&out, vips, ""); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"10.0.0.1", "10.0.0.2", "1/2", "1/1"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary is missing %q:\n%s", want, got)
		}
	}
	out.Reset()
	if err := renderStatus(&out, vips, "json"); err != nil || !strings.HasPrefix(strings.TrimSpace(out.String()), "[") {
		t.Errorf("json for several VIPs should be the list: %v %q", err, out.String())
	}
}

func TestStatusForOneVIPIsUnchanged(t *testing.T) {
	var out bytes.Buffer
	if err := renderStatus(&out, []dataplane.Status{vip("10.0.0.1", 80, be(0, true))}, ""); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"VIP        10.0.0.1:80/tcp", "backends   1/1 healthy"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("single-VIP output lost %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	if err := renderStatus(&out, []dataplane.Status{vip("10.0.0.1", 80)}, "json"); err != nil || !strings.HasPrefix(strings.TrimSpace(out.String()), "{") {
		t.Errorf("json for one VIP should stay an object: %v %q", err, out.String())
	}
}

func TestStatusWithNoVIPsSaysSo(t *testing.T) {
	if err := renderStatus(&bytes.Buffer{}, nil, ""); err == nil || !strings.Contains(err.Error(), "no VIPs") {
		t.Errorf("want a 'no VIPs' error, got %v", err)
	}
}

func TestBackendsShowTheirVIPWhenLabelled(t *testing.T) {
	var out bytes.Buffer
	rows := []dataplane.BackendStatus{
		{VIP: "10.0.0.1:80:tcp", ID: 0, Address: "10.1.0.1", Port: 8080, Weight: 1, State: "healthy", Healthy: true},
		{VIP: "10.0.0.2:443:tcp", ID: 0, Address: "10.1.0.1", Port: 8443, Weight: 3, State: "draining", AdminDraining: true},
	}
	if err := renderBackends(&out, rows, ""); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.HasPrefix(got, "VIP") || !strings.Contains(got, "10.0.0.2:443:tcp") || !strings.Contains(got, "draining (operator)") {
		t.Errorf("a backend serving two VIPs must show both rows with their VIP:\n%s", got)
	}
}

// An older rivorad does not label rows; the table must still print, without the column.
func TestBackendsWithoutVIPLabelKeepTheOldLayout(t *testing.T) {
	var out bytes.Buffer
	if err := renderBackends(&out, []dataplane.BackendStatus{{ID: 7, Address: "10.1.0.1", Port: 80, State: "healthy", Healthy: true}}, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "ID") {
		t.Errorf("old layout expected:\n%s", out.String())
	}
}

func TestZeroBackendsStillPrintsAHeader(t *testing.T) {
	var out bytes.Buffer
	if err := renderBackends(&out, nil, ""); err != nil || out.Len() == 0 {
		t.Errorf("an empty table should still print a header: %v %q", err, out.String())
	}
}
