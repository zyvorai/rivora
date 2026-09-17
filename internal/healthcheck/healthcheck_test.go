// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package healthcheck

import (
	"net"
	"sync"
	"testing"
	"time"
)

// listenOn returns a live TCP listener and a closer that shuts it down —
// connecting to it always succeeds (a "healthy" backend).
func listenOn(t *testing.T) (net.Listener, uint16) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln, uint16(ln.Addr().(*net.TCPAddr).Port)
}

// closedPort returns a port nothing is listening on — connecting to it
// fails fast with connection-refused (a "down" backend), without needing to
// wait out a dial timeout.
func closedPort(t *testing.T) uint16 {
	t.Helper()
	ln, port := listenOn(t)
	_ = ln.Close()
	return port
}

func newChecker(failThreshold, successThreshold int, onChange func(uint32, bool)) *Checker {
	return New(time.Hour /* Run() isn't exercised in these tests */, 200*time.Millisecond, failThreshold, successThreshold, onChange)
}

func TestSetTargetsStartsHealthy(t *testing.T) {
	c := newChecker(1, 1, nil)
	c.SetTargets([]Target{{BackendID: 1, Address: "127.0.0.1", Port: 80}})
	if !c.IsHealthy(1) {
		t.Error("a freshly added target should start healthy, so it can serve immediately")
	}
}

func TestIsHealthyUnknownBackend(t *testing.T) {
	c := newChecker(1, 1, nil)
	if c.IsHealthy(99) {
		t.Error("an unknown backend ID should report unhealthy, not panic or default true")
	}
}

func TestSetTargetsRemovesStaleBackends(t *testing.T) {
	c := newChecker(1, 1, nil)
	c.SetTargets([]Target{{BackendID: 1, Address: "127.0.0.1", Port: 80}})
	c.SetTargets([]Target{{BackendID: 2, Address: "127.0.0.1", Port: 81}})
	if c.IsHealthy(1) {
		t.Error("backend 1 was dropped from the target set and should no longer report healthy")
	}
}

func TestProbeOneFlipsUnhealthyAtFailThreshold(t *testing.T) {
	port := closedPort(t)
	var mu sync.Mutex
	var changes []bool
	c := newChecker(2, 2, func(id uint32, healthy bool) {
		mu.Lock()
		changes = append(changes, healthy)
		mu.Unlock()
	})
	c.SetTargets([]Target{{BackendID: 1, Address: "127.0.0.1", Port: port}})
	target := Target{BackendID: 1, Address: "127.0.0.1", Port: port}

	c.probeOne(target)
	if !c.IsHealthy(1) {
		t.Fatal("a single failed probe should not flip health below FailThreshold=2")
	}
	c.probeOne(target)
	if c.IsHealthy(1) {
		t.Fatal("two consecutive failed probes should flip health at FailThreshold=2")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(changes) != 1 || changes[0] != false {
		t.Errorf("OnChange calls = %v, want exactly one call with healthy=false", changes)
	}
}

func TestProbeOneRecoversAtSuccessThreshold(t *testing.T) {
	ln, port := listenOn(t)
	defer ln.Close()

	c := newChecker(1, 2, nil)
	c.SetTargets([]Target{{BackendID: 1, Address: "127.0.0.1", Port: port}})
	// Force the backend down first so recovery has something to climb out of.
	c.mu.Lock()
	c.states[1].healthy = false
	c.mu.Unlock()

	target := Target{BackendID: 1, Address: "127.0.0.1", Port: port}
	c.probeOne(target)
	if c.IsHealthy(1) {
		t.Fatal("one successful probe should not recover health below SuccessThreshold=2")
	}
	c.probeOne(target)
	if !c.IsHealthy(1) {
		t.Fatal("two consecutive successful probes should recover health at SuccessThreshold=2")
	}
}

func TestProbeOneIgnoresRemovedBackend(t *testing.T) {
	port := closedPort(t)
	called := false
	c := newChecker(1, 1, func(uint32, bool) { called = true })
	c.SetTargets([]Target{{BackendID: 1, Address: "127.0.0.1", Port: port}})
	c.SetTargets(nil) // backend 1 no longer tracked

	c.probeOne(Target{BackendID: 1, Address: "127.0.0.1", Port: port})
	if called {
		t.Error("probing a backend no longer in the target set should not invoke OnChange")
	}
}

func TestTCPProbe(t *testing.T) {
	ln, port := listenOn(t)
	defer ln.Close()
	if !tcpProbe("127.0.0.1", port, time.Second) {
		t.Error("tcpProbe against a live listener should succeed")
	}

	closed := closedPort(t)
	if tcpProbe("127.0.0.1", closed, time.Second) {
		t.Error("tcpProbe against a closed port should fail")
	}
}

func TestTCPProbeIPv6Literal(t *testing.T) {
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	defer ln.Close()
	port := uint16(ln.Addr().(*net.TCPAddr).Port)
	if !tcpProbe("::1", port, time.Second) {
		t.Error("tcpProbe should bracket an IPv6 literal address correctly, not treat it as ambiguous with the port")
	}
}
