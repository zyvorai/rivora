// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zyvorai/rivora/internal/config"
	"github.com/zyvorai/rivora/internal/dataplane"
	"github.com/zyvorai/rivora/internal/logging"
)

type fakePlane struct {
	calls   int
	got     []config.VIP
	result  dataplane.ReloadResult
	err     error
	applied int // how many times afterApply ran, checked by the tests below
}

func (f *fakePlane) ReloadVIPs(desired []config.VIP) (dataplane.ReloadResult, error) {
	f.calls++
	f.got = desired
	return f.result, f.err
}

const dsrYAML = `
interface: eth0
apiListen: 127.0.0.1:9870
vips:
  - address: 10.0.0.100
    port: 80
    protocol: tcp
    mode: dsr
    backends:
      - {address: 10.0.0.11, port: 80, mac: "aa:bb:cc:dd:ee:01"}
`

const natYAML = `
interface: eth0
apiListen: 127.0.0.1:9870
vips:
  - address: 10.0.0.100
    port: 80
    protocol: tcp
    mode: nat
    backends:
      - {address: 10.0.1.11, port: 8080}
`

func writeCfg(t *testing.T, yaml string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func runReload(t *testing.T, yaml string, natLoaded bool, plane *fakePlane, running config.Config) (reloadOutcome, string) {
	t.Helper()
	var buf bytes.Buffer
	logger, err := logging.New(&buf, "debug", "text")
	if err != nil {
		t.Fatal(err)
	}
	out := reloadStaticConfig(logger, writeCfg(t, yaml), running, natLoaded, plane, func() { plane.applied++ })
	return out, buf.String()
}

func startupCfg(t *testing.T, yaml string) config.Config {
	t.Helper()
	cfg, err := config.Load(writeCfg(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestReloadRejectsInvalidFileWithoutTouchingTheDataplane(t *testing.T) {
	// The whole point of validating first: a typo'd edit must never reach a
	// running load balancer.
	for name, yaml := range map[string]string{
		"not yaml":        "interface: [unclosed",
		"no vips":         "interface: eth0\nvips: []\n",
		"bad protocol":    strings.Replace(dsrYAML, "protocol: tcp", "protocol: sctp", 1),
		"no interface":    strings.Replace(dsrYAML, "interface: eth0", "interface: \"\"", 1),
		"dsr without mac": strings.Replace(dsrYAML, `, mac: "aa:bb:cc:dd:ee:01"`, "", 1),
	} {
		plane := &fakePlane{}
		out, logs := runReload(t, yaml, false, plane, startupCfg(t, dsrYAML))
		if out != reloadRejected {
			t.Errorf("%s: outcome %v, want rejected", name, out)
		}
		if plane.calls != 0 || plane.applied != 0 {
			t.Errorf("%s: dataplane touched (calls=%d applied=%d) despite a rejected reload", name, plane.calls, plane.applied)
		}
		if !strings.Contains(logs, "keeping the running config") {
			t.Errorf("%s: no operator-facing rejection message in logs: %q", name, logs)
		}
	}
}

func TestReloadRejectsIntroducingNATWithoutTheEgressProgram(t *testing.T) {
	// Started DSR-only => tc_nat was never loaded. Programming a NAT VIP anyway
	// would rewrite the forward path with no un-NAT on replies.
	plane := &fakePlane{}
	out, logs := runReload(t, natYAML, false, plane, startupCfg(t, dsrYAML))
	if out != reloadRejected || plane.calls != 0 {
		t.Fatalf("outcome %v, calls %d; want rejected without touching the dataplane", out, plane.calls)
	}
	if !strings.Contains(logs, "restart rivorad") {
		t.Errorf("rejection should tell the operator to restart, got %q", logs)
	}
}

func TestReloadAllowsNATWhenEgressProgramIsLoaded(t *testing.T) {
	plane := &fakePlane{}
	out, _ := runReload(t, natYAML, true, plane, startupCfg(t, natYAML))
	if out != reloadApplied || plane.calls != 1 {
		t.Errorf("outcome %v, calls %d; want applied with one ReloadVIPs call", out, plane.calls)
	}
}

func TestReloadAppliesVIPsAndRunsAfterApply(t *testing.T) {
	plane := &fakePlane{result: dataplane.ReloadResult{Added: 1, Unchanged: 2}}
	out, logs := runReload(t, dsrYAML, false, plane, startupCfg(t, dsrYAML))
	if out != reloadApplied {
		t.Fatalf("outcome %v, want applied", out)
	}
	if plane.calls != 1 || len(plane.got) != 1 || plane.got[0].Address != "10.0.0.100" {
		t.Errorf("ReloadVIPs got %+v (calls %d)", plane.got, plane.calls)
	}
	if plane.applied != 1 {
		t.Errorf("afterApply ran %d times, want 1 (health targets must be refreshed)", plane.applied)
	}
	if !strings.Contains(logs, "config reloaded") || !strings.Contains(logs, "added=1") {
		t.Errorf("summary missing from logs: %q", logs)
	}
}

func TestReloadPartialFailureStillRefreshesAndIsReported(t *testing.T) {
	// Some VIPs did change, so health targets must still be refreshed, but the
	// operator must be told it wasn't clean.
	plane := &fakePlane{result: dataplane.ReloadResult{Added: 1}, err: errors.New("vip 10.0.0.9:80: no free maglev extent")}
	out, logs := runReload(t, dsrYAML, false, plane, startupCfg(t, dsrYAML))
	if out != reloadPartial {
		t.Fatalf("outcome %v, want partial", out)
	}
	if plane.applied != 1 {
		t.Errorf("afterApply ran %d times, want 1 even after a partial failure", plane.applied)
	}
	if !strings.Contains(logs, "partly applied") || !strings.Contains(logs, "no free maglev extent") {
		t.Errorf("failure detail missing from logs: %q", logs)
	}
}

func TestReloadWarnsAboutStartupOnlySettingsButStillAppliesVIPs(t *testing.T) {
	changed := strings.Replace(dsrYAML, "interface: eth0", "interface: eth9", 1)
	plane := &fakePlane{}
	out, logs := runReload(t, changed, false, plane, startupCfg(t, dsrYAML))
	if out != reloadApplied || plane.calls != 1 {
		t.Fatalf("a startup-only change must not block the VIP reload: outcome %v calls %d", out, plane.calls)
	}
	if !strings.Contains(logs, "NOT applied") || !strings.Contains(logs, "interface") {
		t.Errorf("operator not warned that the interface change was ignored: %q", logs)
	}
}

func TestLocalPolicyOnlyHonouredWhereItIsSafe(t *testing.T) {
	cases := []struct {
		name         string
		bgp, l2      bool
		node         string
		wantHonoured bool
		wantReason   string
	}{
		{"BGP with the L2 speaker off and a node name: safe", true, false, "node-a", true, ""},
		{"whitespace around the node name is trimmed", true, false, "  node-a ", true, ""},
		{"L2 speaker alone: unsafe, one node answers for every VIP", false, true, "node-a", false, "L2 speaker"},
		{"BGP and the L2 speaker together: unsafe, and says how to fix it", true, true, "node-a", false, "-speaker=false"},
		{"neither BGP nor L2: nothing stops a node without pods attracting traffic", false, false, "node-a", false, "needs BGP"},
		{"BGP but no node name: cannot filter", true, false, "", false, "NODE_NAME"},
		{"BGP but a blank node name", true, false, "   ", false, "NODE_NAME"},
	}
	for _, c := range cases {
		got := localPolicyFor(c.bgp, c.l2, c.node)
		if (got.Node != "") != c.wantHonoured {
			t.Errorf("%s: honoured=%v, want %v (%+v)", c.name, got.Node != "", c.wantHonoured, got)
		}
		if c.wantHonoured && got.Node != "node-a" {
			t.Errorf("%s: node = %q, want it trimmed to node-a", c.name, got.Node)
		}
		if !c.wantHonoured && !strings.Contains(got.NotHonouredReason, c.wantReason) {
			t.Errorf("%s: reason %q should mention %q", c.name, got.NotHonouredReason, c.wantReason)
		}
		if c.wantHonoured && got.NotHonouredReason != "" {
			t.Errorf("%s: honoured but carries a reason: %q", c.name, got.NotHonouredReason)
		}
	}
}
