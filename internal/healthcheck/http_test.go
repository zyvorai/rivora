// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package healthcheck

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zyvorai/rivora/internal/config"
)

func httpTarget(t *testing.T, srv *httptest.Server, spec config.ProbeSpec) Target {
	t.Helper()
	host, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
	p, _ := strconv.Atoi(portStr)
	spec.Type = config.ProbeHTTP
	return Target{BackendID: 1, Address: host, Port: uint16(p), Probe: spec}
}

func statusServer(t *testing.T, code int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }))
	t.Cleanup(srv.Close)
	return srv
}

func TestHTTPProbeJudgesTheStatus(t *testing.T) {
	// Default expectation is 200-399.
	for code, want := range map[int]bool{200: true, 204: true, 301: true, 302: true, 399: true, 400: false, 404: false, 500: false, 503: false} {
		srv := statusServer(t, code)
		if got := probe(httpTarget(t, srv, config.ProbeSpec{}), time.Second); got != want {
			t.Errorf("status %d: probe = %v, want %v", code, got, want)
		}
	}
}

// The point of an HTTP probe: this backend's port is open and accepting, so a TCP
// probe passes it, but the app behind it is failing, so the HTTP probe must not.
func TestHTTPProbeCatchesWhatTCPMisses(t *testing.T) {
	srv := statusServer(t, http.StatusInternalServerError)
	tgt := httpTarget(t, srv, config.ProbeSpec{})

	tcp := tgt
	tcp.Probe = config.ProbeSpec{} // the legacy default
	if !probe(tcp, time.Second) {
		t.Fatal("a TCP probe should pass a backend that accepts connections")
	}
	if probe(tgt, time.Second) {
		t.Error("an HTTP probe passed a backend answering 500")
	}
}

func TestHTTPProbeExpectStatus(t *testing.T) {
	srv := statusServer(t, http.StatusNoContent)
	for expect, want := range map[string]bool{"204": true, "200-299": true, "200": false, "200-203": false, "400-599": false} {
		if got := probe(httpTarget(t, srv, config.ProbeSpec{ExpectStatus: expect}), time.Second); got != want {
			t.Errorf("expect %q vs a 204: probe = %v, want %v", expect, got, want)
		}
	}
	// A backend deliberately expected to answer 4xx (say an auth-protected endpoint).
	deny := statusServer(t, http.StatusUnauthorized)
	if !probe(httpTarget(t, deny, config.ProbeSpec{ExpectStatus: "401"}), time.Second) {
		t.Error("expectStatus 401 did not accept a 401")
	}
}

func TestHTTPProbeUsesPathAndHostHeader(t *testing.T) {
	var gotPath, gotHost, gotUA atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.RequestURI())
		gotHost.Store(r.Host)
		gotUA.Store(r.UserAgent())
		if r.URL.Path != "/ready" {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	if !probe(httpTarget(t, srv, config.ProbeSpec{Path: "/ready?deep=1", Host: "app.example"}), time.Second) {
		t.Fatal("probe of /ready failed")
	}
	if gotPath.Load() != "/ready?deep=1" {
		t.Errorf("server saw path %v", gotPath.Load())
	}
	if gotHost.Load() != "app.example" {
		t.Errorf("server saw Host %v, want the configured host", gotHost.Load())
	}
	if gotUA.Load() != "rivora-healthcheck" {
		t.Errorf("User-Agent = %v", gotUA.Load())
	}
	// The default path is /, which this server 404s: the probe must notice.
	if probe(httpTarget(t, srv, config.ProbeSpec{}), time.Second) {
		t.Error("default path / should have hit the 404")
	}
}

func TestHTTPProbeDoesNotFollowRedirects(t *testing.T) {
	var followed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/landed" {
			followed.Store(true)
			return
		}
		http.Redirect(w, r, "/landed", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	// 302 is inside the default range, so it passes without being followed.
	if !probe(httpTarget(t, srv, config.ProbeSpec{}), time.Second) {
		t.Error("a 302 should pass the default 200-399 expectation")
	}
	if followed.Load() {
		t.Error("the probe followed the redirect; it must judge the redirect itself")
	}
	// And a strict 200 expectation must reject it.
	if probe(httpTarget(t, srv, config.ProbeSpec{ExpectStatus: "200"}), time.Second) {
		t.Error("expectStatus 200 accepted a 302")
	}
}

func TestHTTPProbeTimesOutOnASlowBackend(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	t.Cleanup(func() { close(release); srv.Close() })

	start := time.Now()
	if probe(httpTarget(t, srv, config.ProbeSpec{}), 200*time.Millisecond) {
		t.Error("a backend that never answers passed")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("probe took %v; it must give up at its timeout, not hang", d)
	}
}

func TestHTTPProbeFailsWhenTheBackendAcceptsButNeverSpeaks(t *testing.T) {
	ln, port := listenOn(t) // accepts TCP, never reads or writes
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	tgt := Target{Address: "127.0.0.1", Port: port, Probe: config.ProbeSpec{Type: config.ProbeHTTP}}
	start := time.Now()
	if probe(tgt, 200*time.Millisecond) {
		t.Error("a silent TCP listener passed an HTTP probe")
	}
	if time.Since(start) > 2*time.Second {
		t.Error("the probe did not honour its timeout")
	}
}

func TestHTTPProbeFailsAgainstANonHTTPService(t *testing.T) {
	ln, port := listenOn(t)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("SSH-2.0-not-http\r\n"))
			_ = c.Close()
		}
	}()
	if probe(Target{Address: "127.0.0.1", Port: port, Probe: config.ProbeSpec{Type: config.ProbeHTTP}}, time.Second) {
		t.Error("a non-HTTP service passed an HTTP probe")
	}
}

func TestProbeFailsOnAClosedPort(t *testing.T) {
	tgt := Target{Address: "127.0.0.1", Port: closedPort(t), Probe: config.ProbeSpec{Type: config.ProbeHTTP}}
	if probe(tgt, time.Second) {
		t.Error("connection refused passed an HTTP probe")
	}
}

func TestProbePortOverride(t *testing.T) {
	// The service port speaks something that isn't HTTP; health is served on its
	// own port. The probe must go to the override, or this would fail.
	svc, svcPort := listenOn(t)
	go func() {
		for {
			c, err := svc.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	health := statusServer(t, http.StatusOK)
	_, hp, _ := net.SplitHostPort(health.Listener.Addr().String())
	hpn, _ := strconv.Atoi(hp)

	tgt := Target{Address: "127.0.0.1", Port: svcPort, Probe: config.ProbeSpec{Type: config.ProbeHTTP, Port: uint16(hpn)}}
	if !probe(tgt, time.Second) {
		t.Error("probe ignored the port override")
	}
	// TCP probes honour it too: nothing listens on the service port here, but the
	// override port accepts.
	dead := Target{Address: "127.0.0.1", Port: closedPort(t), Probe: config.ProbeSpec{Port: uint16(hpn)}}
	if !probe(dead, time.Second) {
		t.Error("a tcp probe ignored the port override")
	}
}

func TestLegacyTargetStillProbesWithTCP(t *testing.T) {
	// A Target with no Probe set — every caller before probes were configurable —
	// must keep doing a plain TCP connect, even against a port that isn't HTTP.
	ln, port := listenOn(t)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	if !probe(Target{Address: "127.0.0.1", Port: port}, time.Second) {
		t.Error("the zero-value Probe no longer means a TCP connect")
	}
}

func TestHTTPProbeUsesAFreshConnectionEachTime(t *testing.T) {
	// A reused connection would keep passing after a backend stops accepting new
	// ones; every probe must connect afresh.
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	tgt := httpTarget(t, srv, config.ProbeSpec{})
	for i := 0; i < 5; i++ {
		if !probe(tgt, time.Second) {
			t.Fatal("probe failed")
		}
	}
	if got := conns.Load(); got != 5 {
		t.Errorf("5 probes opened %d connections, want 5 (no reuse)", got)
	}
}

func TestHTTPProbeIPv6Literal(t *testing.T) {
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skip("no IPv6 loopback here")
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	port := uint16(ln.Addr().(*net.TCPAddr).Port)
	if !probe(Target{Address: "::1", Port: port, Probe: config.ProbeSpec{Type: config.ProbeHTTP}}, time.Second) {
		t.Error("an IPv6 backend was not probed correctly (address needs bracketing in the URL)")
	}
}

// End to end through the Checker's own thresholds: a backend whose port stays
// open but whose app starts failing is marked down after FailThreshold probes,
// and back up once it recovers — the transition a TCP probe could never report.
func TestCheckerMarksAnOpenButFailingBackendDown(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(srv.Close)

	var flips []bool
	c := newChecker(2, 2, func(_ uint32, h bool) { flips = append(flips, h) })
	tgt := httpTarget(t, srv, config.ProbeSpec{})
	c.SetTargets([]Target{tgt})

	c.probeOne(tgt)
	if !c.IsHealthy(1) {
		t.Fatal("a passing probe flipped the backend")
	}
	healthy.Store(false)
	c.probeOne(tgt) // 1st failure: below the threshold of 2
	if !c.IsHealthy(1) {
		t.Fatal("one failed probe should not flip a backend (thresholds exist to stop flapping)")
	}
	c.probeOne(tgt) // 2nd failure
	if c.IsHealthy(1) {
		t.Fatal("two failed probes should have marked the backend down, though its TCP port is open")
	}
	healthy.Store(true)
	c.probeOne(tgt)
	c.probeOne(tgt)
	if !c.IsHealthy(1) {
		t.Fatal("two passing probes should have restored the backend")
	}
	if len(flips) != 2 || flips[0] != false || flips[1] != true {
		t.Errorf("OnChange calls = %v, want [false true]", flips)
	}
}
