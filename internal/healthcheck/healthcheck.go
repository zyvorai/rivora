// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package healthcheck runs active probes against backends (a TCP connect, or an
// HTTP GET whose status is checked) and reports up/down transitions once
// consecutive fail/success thresholds are crossed, so a single dropped probe
// doesn't flap a backend out of rotation.
package healthcheck

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/zyvorai/rivora/internal/config"
)

type Target struct {
	BackendID uint32
	Address   string
	Port      uint16
	// Probe says how to check this backend; the zero value is a TCP connect to
	// Port, which is what every target did before probes were configurable.
	Probe config.ProbeSpec
}

type state struct {
	healthy     bool
	consecutive int // consecutive results agreeing with the *opposite* of healthy
}

type Checker struct {
	Interval         time.Duration
	Timeout          time.Duration
	FailThreshold    int
	SuccessThreshold int
	OnChange         func(backendID uint32, healthy bool)

	mu      sync.Mutex
	states  map[uint32]*state
	targets []Target
	stop    chan struct{}
}

func New(interval, timeout time.Duration, failThreshold, successThreshold int, onChange func(uint32, bool)) *Checker {
	return &Checker{
		Interval:         interval,
		Timeout:          timeout,
		FailThreshold:    failThreshold,
		SuccessThreshold: successThreshold,
		OnChange:         onChange,
		states:           map[uint32]*state{},
		stop:             make(chan struct{}),
	}
}

// SetTargets replaces the checked backend set. Backends start healthy so
// v0.1 doesn't have to wait a full check interval before a fresh config
// serves any traffic; the first failed probe will take them out.
func (c *Checker) SetTargets(targets []Target) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.targets = targets
	seen := map[uint32]bool{}
	for _, t := range targets {
		seen[t.BackendID] = true
		if _, ok := c.states[t.BackendID]; !ok {
			c.states[t.BackendID] = &state{healthy: true}
		}
	}
	for id := range c.states {
		if !seen[id] {
			delete(c.states, id)
		}
	}
}

func (c *Checker) IsHealthy(backendID uint32) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.states[backendID]; ok {
		return s.healthy
	}
	return false
}

func (c *Checker) Run() {
	ticker := time.NewTicker(c.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
			c.probeAll()
		}
	}
}

func (c *Checker) Stop() { close(c.stop) }

func (c *Checker) probeAll() {
	c.mu.Lock()
	targets := append([]Target(nil), c.targets...)
	c.mu.Unlock()

	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.probeOne(t)
		}()
	}
	wg.Wait()
}

func (c *Checker) probeOne(t Target) {
	ok := probe(t, c.Timeout)

	c.mu.Lock()
	s, exists := c.states[t.BackendID]
	if !exists {
		c.mu.Unlock()
		return
	}
	prevHealthy := s.healthy
	if ok == s.healthy {
		s.consecutive = 0
	} else {
		s.consecutive++
		threshold := c.FailThreshold
		if ok {
			threshold = c.SuccessThreshold
		}
		if s.consecutive >= threshold {
			s.healthy = ok
			s.consecutive = 0
		}
	}
	changed := s.healthy != prevHealthy
	newHealthy := s.healthy
	c.mu.Unlock()

	if changed && c.OnChange != nil {
		c.OnChange(t.BackendID, newHealthy)
	}
}

// probe runs t's configured probe once and reports whether the backend passed.
func probe(t Target, timeout time.Duration) bool {
	spec := t.Probe.Effective()
	port := t.Port
	if spec.Port != 0 {
		port = spec.Port // health endpoint on its own port
	}
	if spec.Type == config.ProbeHTTP {
		return httpProbe(t.Address, port, spec, timeout)
	}
	return tcpProbe(t.Address, port, timeout)
}

// probeClient is shared by every HTTP probe. It never follows redirects (a 3xx
// is judged by expectStatus, like any other status), never reuses a connection
// (each probe is a fresh connect, so a backend that has stopped accepting new
// connections is noticed), and ignores proxy environment variables (a health
// probe must reach the backend itself, not something in front of it).
var probeClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport:     &http.Transport{DisableKeepAlives: true, Proxy: nil},
}

// httpProbe GETs spec.Path on addr:port and passes iff the status is within
// spec's expected range. The whole exchange, connect to last byte read, is
// bounded by timeout, so a backend that accepts and then never answers fails
// the probe instead of hanging it.
func httpProbe(addr string, port uint16, spec config.ProbeSpec, timeout time.Duration) bool {
	lo, hi, err := spec.StatusRange()
	if err != nil {
		return false // config validation rejects this; never treat a broken spec as healthy
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// JoinHostPort brackets an IPv6 literal, as in tcpProbe.
	url := "http://" + net.JoinHostPort(addr, fmt.Sprintf("%d", port)) + spec.Path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	if spec.Host != "" {
		req.Host = spec.Host
	}
	req.Header.Set("User-Agent", "rivora-healthcheck")
	resp, err := probeClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	// Drain a little so the server sees a clean exchange; the body is irrelevant.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode >= lo && resp.StatusCode <= hi
}

func tcpProbe(addr string, port uint16, timeout time.Duration) bool {
	// net.JoinHostPort, not a raw fmt.Sprintf("%s:%d", ...) — an IPv6
	// literal needs bracketing ("[fd00::1]:8080") or it's ambiguous with
	// the port separator; JoinHostPort handles both families correctly.
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(addr, fmt.Sprintf("%d", port)), timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
