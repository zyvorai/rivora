// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package healthcheck runs active TCP connect probes against backends and
// reports up/down transitions once consecutive fail/success thresholds are
// crossed, so a single dropped probe doesn't flap a backend out of rotation.
package healthcheck

import (
	"fmt"
	"net"
	"sync"
	"time"
)

type Target struct {
	BackendID uint32
	Address   string
	Port      uint16
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
	ok := tcpProbe(t.Address, t.Port, c.Timeout)

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

func tcpProbe(addr string, port uint16, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", addr, port), timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
