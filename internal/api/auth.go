// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package api

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MinKeyLength is the length below which a key is worth a warning. It is not
// enforced: existing deployments use whatever they chose and must keep starting.
const MinKeyLength = 16

// credential is one API key and the name it is audited under. A key is written either bare
// ("s3cret") or named ("id:alice=s3cret"); a bare key is named after its position ("key-1"), so the
// audit trail can always say which of several keys acted, without the key itself.
type credential struct {
	name, key string
}

// namedPrefix marks a named key. It is an explicit prefix, not a bare "name=key", because keys are
// often base64 and end in "=".
const namedPrefix = "id:"

// parseCredentials splits a comma-separated list into credentials, trimming whitespace and dropping empty
// entries. A list is how a key is rotated with no outage: add the new key, move clients to it, then
// remove the old one. err reports a malformed named entry (its text is never echoed: it holds a secret).
func parseCredentials(spec string) ([]credential, error) {
	var out []credential
	for i, e := range strings.Split(spec, ",") {
		if e = strings.TrimSpace(e); e == "" {
			continue
		}
		if !strings.HasPrefix(e, namedPrefix) {
			out = append(out, credential{name: "key-" + strconv.Itoa(len(out)+1), key: e})
			continue
		}
		name, key, ok := strings.Cut(strings.TrimPrefix(e, namedPrefix), "=")
		if !ok || key == "" || !validKeyName(name) {
			return nil, fmt.Errorf("entry %d of the key list is malformed: a named key is id:NAME=KEY, with NAME of letters, digits, '.', '_' or '-' (up to 64) and a non-empty KEY", i+1)
		}
		out = append(out, credential{name: name, key: key})
	}
	return out, nil
}

func validKeyName(n string) bool {
	if n == "" || len(n) > 64 {
		return false
	}
	for _, r := range n {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// parseKeys returns just the secrets of a key list. Malformed entries are dropped here; ValidateKeys is
// what reports them.
func parseKeys(spec string) []string {
	creds, _ := parseCredentials(spec)
	keys := make([]string, len(creds))
	for i, c := range creds {
		keys[i] = c.key
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
	ac, aerr := parseCredentials(admin)
	if aerr != nil {
		return fmt.Errorf("RIVORA_API_KEY: %w", aerr)
	}
	rc, rerr := parseCredentials(readOnly)
	if rerr != nil {
		return fmt.Errorf("RIVORA_API_READONLY_KEY: %w", rerr)
	}
	names := map[string]bool{}
	for _, c := range append(append([]credential(nil), ac...), rc...) {
		if !strings.HasPrefix(c.name, "key-") && names[c.name] {
			return fmt.Errorf("the key name %q is used twice: audit entries would be ambiguous", c.name)
		}
		names[c.name] = true
	}
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

// matchCred is matchAny that also says which credential matched (by name). It too compares against every
// key, so timing does not reveal the position.
func matchCred(token string, creds []credential) (string, bool) {
	name, hit := "", 0
	for _, c := range creds {
		eq := subtle.ConstantTimeCompare([]byte(token), []byte(c.key))
		if eq == 1 {
			name = c.name
		}
		hit |= eq
	}
	return name, hit == 1
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
func (s *Server) recordAuthFailure(r *http.Request, reason, user string) {
	if s.authFailures != nil {
		s.authFailures.WithLabelValues(reason).Inc()
	}
	if s.logger == nil {
		return
	}
	// An anonymous failure may be a scanner hammering the port, so it is throttled. One by an authenticated
	// caller (a read-only key or certificate trying a change) is rare, meaningful, and always worth a line.
	ok, suppressed := true, 0
	if user == "" {
		if s.failLog == nil {
			return
		}
		ok, suppressed = s.failLog.allow()
	}
	if ok {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		s.logger.Warn("api request rejected",
			"reason", reason, "user", user, "remote", host, "method", r.Method, "path", r.URL.Path,
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
