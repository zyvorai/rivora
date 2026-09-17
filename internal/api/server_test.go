// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidBearer(t *testing.T) {
	s := &Server{apiKey: "secret-token"}

	cases := []struct {
		name   string
		header string
		want   bool
	}{
		{"correct token", "Bearer secret-token", true},
		{"wrong token", "Bearer nope", false},
		{"empty header", "", false},
		{"missing Bearer prefix", "secret-token", false},
		{"wrong scheme", "Basic secret-token", false},
		{"trailing whitespace differs", "Bearer secret-token ", false},
		{"empty token", "Bearer ", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := s.validBearer(c.header); got != c.want {
				t.Errorf("validBearer(%q) = %v, want %v", c.header, got, c.want)
			}
		})
	}
}

func TestAuthMiddlewareNoKeyConfigured(t *testing.T) {
	s := &Server{apiKey: ""}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	rec := httptest.NewRecorder()
	s.auth(inner).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("unauthenticated server: got status %d, want 200 (no key configured means no auth required)", rec.Code)
	}
}

func TestAuthMiddlewareRequiresToken(t *testing.T) {
	s := &Server{apiKey: "secret-token"}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	handler := s.auth(inner)

	t.Run("missing header rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("got status %d, want 401", rec.Code)
		}
		if rec.Header().Get("WWW-Authenticate") == "" {
			t.Error("expected WWW-Authenticate header on 401")
		}
	})

	t.Run("wrong token rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
		req.Header.Set("Authorization", "Bearer wrong")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("got status %d, want 401", rec.Code)
		}
	})

	t.Run("correct token accepted", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
		req.Header.Set("Authorization", "Bearer secret-token")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("got status %d, want 200", rec.Code)
		}
	})

	t.Run("ui path stays public", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("got status %d, want 200 (console must load without a bearer)", rec.Code)
		}
	})
}

func TestHealthzAlwaysOK(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("GET /healthz: status %d, want 200", rec.Code)
	}
}

func TestHealthzUnauthenticatedEvenWithAPIKey(t *testing.T) {
	s := &Server{apiKey: "secret-token"}
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("GET /healthz with an API key configured: status %d, want 200 (probes carry no bearer token)", rec.Code)
	}
}

func TestConsoleRoutesServe(t *testing.T) {
	s := &Server{}
	h := s.Handler()

	t.Run("root serves index", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /: status %d", rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "Rivora") && !strings.Contains(body, "root") {
			t.Errorf("GET /: unexpected body prefix %q", body[:min(80, len(body))])
		}
	})
}
