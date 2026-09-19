// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestParseKeys(t *testing.T) {
	for in, want := range map[string]int{"": 0, "  ": 0, ",": 0, " , ,": 0, "a": 1, "a,b": 2, " a , b ,": 2, "a,,b": 2} {
		if got := parseKeys(in); len(got) != want {
			t.Errorf("parseKeys(%q) = %v, want %d keys", in, got, want)
		}
	}
	if got := parseKeys(" new-key , old-key "); got[0] != "new-key" || got[1] != "old-key" {
		t.Errorf("keys not trimmed: %q", got)
	}
}

func TestValidateKeys(t *testing.T) {
	ok := [][2]string{{"", ""}, {"admin", ""}, {"a1,a2", "r1,r2"}, {"admin", "reader"}}
	for _, c := range ok {
		if err := ValidateKeys(c[0], c[1]); err != nil {
			t.Errorf("ValidateKeys(%q,%q) = %v, want ok", c[0], c[1], err)
		}
	}
	bad := map[string][2]string{
		"read-only without admin (would protect nothing)": {"", "reader"},
		"key in both roles (ambiguous)":                   {"a,shared", "shared,r"},
		"admin present but no usable key":                 {" , ", ""},
		"read-only present but no usable key":             {"admin", " ,"},
	}
	for name, c := range bad {
		if err := ValidateKeys(c[0], c[1]); err == nil {
			t.Errorf("%s: ValidateKeys(%q,%q) accepted", name, c[0], c[1])
		}
	}
}

func TestWeakKeysCountsWithoutRevealingThem(t *testing.T) {
	if got := WeakKeys("short,a-sufficiently-long-key-1234"); got != 1 {
		t.Errorf("WeakKeys = %d, want 1", got)
	}
	if WeakKeys("") != 0 || WeakKeys("0123456789abcdef") != 0 {
		t.Error("empty setting or a 16-char key reported weak")
	}
}

func TestMatchAny(t *testing.T) {
	keys := []string{"first", "second", "third"}
	for _, k := range keys {
		if !matchAny(k, keys) {
			t.Errorf("%q not matched", k)
		}
	}
	for _, bad := range []string{"", "fir", "first ", "FIRST", "thirdd"} {
		if matchAny(bad, keys) {
			t.Errorf("%q wrongly matched", bad)
		}
	}
	if matchAny("x", nil) {
		t.Error("matched against no keys")
	}
}

func newAuthServer(admin, readOnly string, calls *int) (*Server, http.Handler) {
	failures := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "rivora_api_auth_failures_total", Help: "x"}, []string{"reason"})
	s := &Server{apiKey: admin, readOnlyKey: readOnly, authFailures: failures}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			*calls++
		}
		w.WriteHeader(http.StatusOK)
	})
	return s, s.auth(inner)
}

func do(h http.Handler, method, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestKeyRotationBothKeysWorkThenOnlyTheNewOne(t *testing.T) {
	// Phase 1 of a rotation: new and old are both accepted, so clients can move over
	// with no outage.
	_, h := newAuthServer("new-key,old-key", "", nil)
	for _, k := range []string{"new-key", "old-key"} {
		if rec := do(h, http.MethodPost, "/api/v1/backends/1/drain", k); rec.Code != http.StatusOK {
			t.Errorf("admin key %q during rotation: status %d, want 200", k, rec.Code)
		}
	}
	// Phase 2: the old key is dropped and stops working.
	_, h = newAuthServer("new-key", "", nil)
	if rec := do(h, http.MethodGet, "/api/v1/vips", "old-key"); rec.Code != http.StatusUnauthorized {
		t.Errorf("retired key: status %d, want 401", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/api/v1/vips", "new-key"); rec.Code != http.StatusOK {
		t.Errorf("current key: status %d, want 200", rec.Code)
	}
}

func TestReadOnlyKeyCanReadButNotChangeAnything(t *testing.T) {
	var calls int
	_, h := newAuthServer("admin-key", "reader-key", &calls)

	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		if rec := do(h, m, "/api/v1/vips", "reader-key"); rec.Code != http.StatusOK {
			t.Errorf("read-only %s: status %d, want 200", m, rec.Code)
		}
	}
	before := calls
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := do(h, m, "/api/v1/backends/1/drain", "reader-key")
		if rec.Code != http.StatusForbidden {
			t.Errorf("read-only %s: status %d, want 403", m, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "read-only") {
			t.Errorf("403 body %q should say the key is read-only", rec.Body.String())
		}
	}
	if calls != before {
		t.Errorf("a mutation reached the handler %d time(s) despite a read-only key", calls-before)
	}
	// Admin still can.
	if rec := do(h, http.MethodPost, "/api/v1/backends/1/drain", "admin-key"); rec.Code != http.StatusOK {
		t.Errorf("admin POST: status %d, want 200", rec.Code)
	}
}

func TestBadCredentialsAreRejected(t *testing.T) {
	_, h := newAuthServer("admin-key", "reader-key", nil)
	for name, tok := range map[string]string{"none": "", "wrong": "nope", "prefix of a key": "admin", "key plus junk": "admin-keyX"} {
		rec := do(h, http.MethodGet, "/api/v1/vips", tok)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, rec.Code)
		}
		if rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s: 401 without a WWW-Authenticate challenge", name)
		}
	}
	// A key sent without the Bearer scheme.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/vips", nil)
	req.Header.Set("Authorization", "admin-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no Bearer scheme: status %d, want 401", rec.Code)
	}
}

func TestReadOnlyKeyDoesNotTurnAuthOnWithoutAnAdminKey(t *testing.T) {
	// Defence in depth behind ValidateKeys: a Server that somehow has only a
	// read-only key must not treat that as "auth enabled with nobody able to
	// write" — the admin key is what enables auth, so this is auth off.
	_, h := newAuthServer("", "reader-key", nil)
	if rec := do(h, http.MethodPost, "/api/v1/backends/1/drain", ""); rec.Code != http.StatusOK {
		t.Errorf("with no admin key auth is off (main refuses this config); got %d", rec.Code)
	}
}

func TestAuthFailureMetricSeparatesTheTwoReasons(t *testing.T) {
	s, h := newAuthServer("admin-key", "reader-key", nil)
	do(h, http.MethodGet, "/api/v1/vips", "wrong")
	do(h, http.MethodGet, "/api/v1/vips", "")
	do(h, http.MethodPost, "/api/v1/backends/1/drain", "reader-key")
	do(h, http.MethodGet, "/api/v1/vips", "reader-key") // a success: must not count
	do(h, http.MethodGet, "/api/v1/vips", "admin-key")  // a success: must not count

	if got := testutil.ToFloat64(s.authFailures.WithLabelValues(failUnauthenticated)); got != 2 {
		t.Errorf("unauthenticated = %v, want 2", got)
	}
	if got := testutil.ToFloat64(s.authFailures.WithLabelValues(failForbidden)); got != 1 {
		t.Errorf("forbidden = %v, want 1", got)
	}
}

func TestUIStaysReachableWithoutAKey(t *testing.T) {
	// The console shell must load so the browser can collect the token; only
	// /api/* is protected.
	_, h := newAuthServer("admin-key", "reader-key", nil)
	if rec := do(h, http.MethodGet, "/", ""); rec.Code != http.StatusOK {
		t.Errorf("GET /: status %d, want 200", rec.Code)
	}
}

func TestThrottleLimitsLogsButCountsWhatItSwallowed(t *testing.T) {
	now := time.Unix(1000, 0)
	th := newThrottle(10 * time.Second)
	th.now = func() time.Time { return now }

	if ok, n := th.allow(); !ok || n != 0 {
		t.Fatalf("first call: ok=%v suppressed=%d, want emit with 0 suppressed", ok, n)
	}
	for i := 0; i < 5; i++ {
		now = now.Add(time.Second)
		if ok, _ := th.allow(); ok {
			t.Fatal("a call inside the interval was allowed through")
		}
	}
	now = now.Add(10 * time.Second)
	if ok, n := th.allow(); !ok || n != 5 {
		t.Errorf("after the interval: ok=%v suppressed=%d, want emit reporting 5 swallowed", ok, n)
	}
}

func TestAuthFailureLogNeverContainsTheCredential(t *testing.T) {
	var buf bytes.Buffer
	s, h := newAuthServer("admin-key", "reader-key", nil)
	s.SetLogger(slog.New(slog.NewTextHandler(&buf, nil)))
	// auth() captured the Server pointer, so the logger set above is in effect.

	req := httptest.NewRequest(http.MethodGet, "/api/v1/vips", nil)
	req.RemoteAddr = "203.0.113.7:51234"
	req.Header.Set("Authorization", "Bearer super-secret-guess-123")
	h.ServeHTTP(httptest.NewRecorder(), req)

	out := buf.String()
	if strings.Contains(out, "super-secret-guess-123") || strings.Contains(out, "admin-key") || strings.Contains(out, "reader-key") {
		t.Errorf("a credential appears in the log: %s", out)
	}
	if !strings.Contains(out, "203.0.113.7") || !strings.Contains(out, "unauthenticated") {
		t.Errorf("log should say who and why: %s", out)
	}
	if strings.Contains(out, "51234") {
		t.Errorf("the ephemeral source port should not be logged: %s", out)
	}
}

func TestCountKeys(t *testing.T) {
	for in, want := range map[string]int{"": 0, " , ": 0, "a": 1, "a, b ,c": 3} {
		if got := CountKeys(in); got != want {
			t.Errorf("CountKeys(%q) = %d, want %d", in, got, want)
		}
	}
}
