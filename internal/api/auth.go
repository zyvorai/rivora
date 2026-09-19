// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package api

import (
	"crypto/subtle"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// MinKeyLength is the length below which a key is worth a warning. It is not
// enforced: existing deployments use whatever they chose and must keep starting.
const MinKeyLength = 16

// parseKeys splits a comma-separated key list, trimming whitespace and dropping
// empty entries. A list is how a key is rotated with no outage: add the new key,
// move clients to it, then remove the old one.
func parseKeys(spec string) []string {
	var keys []string
	for _, k := range strings.Split(spec, ",") {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}
	return keys
}

// ValidateKeys checks a deployment's key configuration. admin and readOnly are
// the raw settings (each may be a comma-separated list). It refuses the two
// combinations that would silently weaken the setup:
//
//   - a read-only key with no admin key: auth is switched on by the admin key, so
//     this would leave the API (mutations included) wide open while looking
//     secured;
//   - a key that is in both lists: which role it gets would be a matter of
//     lookup order.
func ValidateKeys(admin, readOnly string) error {
	a, r := parseKeys(admin), parseKeys(readOnly)
	// A setting that is present but yields no key (say " , ") must not quietly
	// mean "auth off": that is exactly how a stray character disables security.
	if strings.TrimSpace(admin) != "" && len(a) == 0 {
		return errors.New("RIVORA_API_KEY is set but contains no usable key")
	}
	if strings.TrimSpace(readOnly) != "" && len(r) == 0 {
		return errors.New("RIVORA_API_READONLY_KEY is set but contains no usable key")
	}
	if len(r) > 0 && len(a) == 0 {
		return errors.New("RIVORA_API_READONLY_KEY is set but RIVORA_API_KEY is not: an admin key is what turns authentication on, so without one the read-only key would protect nothing")
	}
	inAdmin := make(map[string]bool, len(a))
	for _, k := range a {
		inAdmin[k] = true
	}
	for _, k := range r {
		if inAdmin[k] {
			return errors.New("a key appears in both RIVORA_API_KEY and RIVORA_API_READONLY_KEY: it would be ambiguous which role it has")
		}
	}
	return nil
}

// CountKeys reports how many usable keys a raw setting holds, for logging how
// auth is configured without revealing any key.
func CountKeys(spec string) int { return len(parseKeys(spec)) }

// WeakKeys reports how many keys in the raw setting are shorter than
// MinKeyLength, for a start-up warning. It never reports the keys themselves.
func WeakKeys(spec string) int {
	n := 0
	for _, k := range parseKeys(spec) {
		if len(k) < MinKeyLength {
			n++
		}
	}
	return n
}

// matchAny reports whether token equals any key. It compares against every key
// rather than stopping at the first hit, so how long a check takes doesn't tell
// an attacker which position in the list matched.
func matchAny(token string, keys []string) bool {
	match := 0
	for _, k := range keys {
		match |= subtle.ConstantTimeCompare([]byte(token), []byte(k))
	}
	return match == 1
}

func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	return strings.TrimPrefix(header, prefix), true
}

// safeMethod is what a read-only key may do: requests that cannot change state.
func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// Reasons for an authentication failure, the values of the reason label on
// rivora_api_auth_failures_total.
const (
	failUnauthenticated = "unauthenticated" // missing, malformed or wrong key (401)
	failForbidden       = "forbidden"       // a valid read-only key attempting a mutation (403)
)

// throttle lets a log line through at most once per interval and counts what it
// swallowed, so an internet scanner hammering the API can't flood the journal
// but a burst is still visible as a count.
type throttle struct {
	mu         sync.Mutex
	interval   time.Duration
	last       time.Time
	suppressed int
	now        func() time.Time
}

func newThrottle(interval time.Duration) *throttle {
	return &throttle{interval: interval, now: time.Now}
}

// allow reports whether to emit now, and how many were swallowed since the last
// one emitted.
func (t *throttle) allow() (bool, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if !t.last.IsZero() && now.Sub(t.last) < t.interval {
		t.suppressed++
		return false, 0
	}
	n := t.suppressed
	t.suppressed, t.last = 0, now
	return true, n
}

// recordAuthFailure counts a rejected request and, throttled, logs it. The
// credential is never logged: only where the request came from and what it tried.
func (s *Server) recordAuthFailure(r *http.Request, reason string) {
	if s.authFailures != nil {
		s.authFailures.WithLabelValues(reason).Inc()
	}
	if s.logger == nil {
		return
	}
	if s.failLog == nil {
		return
	}
	if ok, suppressed := s.failLog.allow(); ok {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		s.logger.Warn("api request rejected",
			"reason", reason, "remote", host, "method", r.Method, "path", r.URL.Path,
			"similar_suppressed", suppressed)
	}
}

// SetLogger turns on the throttled warning for rejected requests.
func (s *Server) SetLogger(l *slog.Logger) {
	s.logger = l
	s.failLog = newThrottle(10 * time.Second)
}

// SetReadOnlyKeys sets the keys that may read but not change anything (a
// comma-separated list). Call before serving; use ValidateKeys first.
func (s *Server) SetReadOnlyKeys(spec string) { s.readOnlyKey = spec }
