// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package initsync tells a caller when a reconciler has processed every object that existed when
// it started, so work that must wait for that (removing what a previous run left behind and nothing
// current claims) does not race the first pass.
package initsync

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Tracker is armed with the keys present at start-up and reports done once each has been reported
// finished. A key that is never finished (a persistently failing object) keeps it from completing, so
// callers pair Wait with a timeout.
type Tracker struct {
	mu      sync.Mutex
	pending map[string]bool
	armed   bool
	done    chan struct{}
	once    sync.Once
}

// New returns a Tracker that has not been armed.
func New() *Tracker { return &Tracker{done: make(chan struct{})} }

// Arm records the keys to wait for. Arm before queueing them, or a fast worker could finish a key
// before it is pending. An empty set completes at once. Arm once.
func (t *Tracker) Arm(keys []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.armed {
		return
	}
	t.armed = true
	t.pending = make(map[string]bool, len(keys))
	for _, k := range keys {
		t.pending[k] = true
	}
	t.completeLocked()
}

// Finished reports that key was reconciled. Keys that were not pending, and calls before Arm, are ignored.
func (t *Tracker) Finished(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.armed || !t.pending[key] {
		return
	}
	delete(t.pending, key)
	t.completeLocked()
}

func (t *Tracker) completeLocked() {
	if len(t.pending) == 0 {
		t.once.Do(func() { close(t.done) })
	}
}

// Done is closed when every armed key has finished.
func (t *Tracker) Done() <-chan struct{} { return t.done }

// Pending returns how many keys are still outstanding, for logging.
func (t *Tracker) Pending() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.pending)
}

// WaitAll blocks until every tracker is done, ctx is cancelled, or timeout elapses. A timeout
// (some object keeps failing to reconcile, or a reconciler never started) is an error, so the
// caller does not act as if the first pass had completed.
func WaitAll(ctx context.Context, timeout time.Duration, trackers ...*Tracker) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for i, t := range trackers {
		select {
		case <-t.Done():
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return fmt.Errorf("timed out after %s: tracker %d still has %d object(s) unreconciled", timeout, i, t.Pending())
		}
	}
	return nil
}
