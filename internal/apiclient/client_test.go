// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package apiclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zyvorai/rivora/internal/dataplane"
)

func TestNewDefaultsToHTTPScheme(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(dataplane.Status{VIPAddress: "10.0.0.1"})
	}))
	defer srv.Close()

	bareAddr := strings.TrimPrefix(srv.URL, "http://")
	c := New(bareAddr, Options{})

	st, err := c.Status()
	if err != nil {
		t.Fatalf("Status() with bare host:port addr: %v", err)
	}
	if st.VIPAddress != "10.0.0.1" {
		t.Errorf("VIPAddress = %q, want 10.0.0.1", st.VIPAddress)
	}
	if gotAuth != "" {
		t.Errorf("Authorization header sent %q, want none when APIKey unset", gotAuth)
	}
}

func TestGetSendsBearerToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer my-token" {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			return
		}
		_ = json.NewEncoder(w).Encode(dataplane.Status{})
	}))
	defer srv.Close()

	t.Run("correct key succeeds", func(t *testing.T) {
		c := New(srv.URL, Options{APIKey: "my-token"})
		if _, err := c.Status(); err != nil {
			t.Errorf("Status() with correct key: %v", err)
		}
	})

	t.Run("missing key fails with clear error", func(t *testing.T) {
		c := New(srv.URL, Options{})
		_, err := c.Status()
		if err == nil || !strings.Contains(err.Error(), "unauthorized") {
			t.Errorf("Status() without key: err=%v, want an 'unauthorized' error", err)
		}
	})
}

func TestTLSInsecureAcceptsSelfSignedCert(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(dataplane.Status{})
	}))
	defer srv.Close()

	t.Run("insecure client succeeds", func(t *testing.T) {
		c := New(srv.URL, Options{TLSInsecure: true})
		if _, err := c.Status(); err != nil {
			t.Errorf("Status() with TLSInsecure against self-signed cert: %v", err)
		}
	})

	t.Run("default client rejects self-signed cert", func(t *testing.T) {
		c := New(srv.URL, Options{})
		if _, err := c.Status(); err == nil {
			t.Error("Status() without TLSInsecure: expected a certificate verification error, got nil")
		}
	})
}

func TestDrainUndrainRequests(t *testing.T) {
	var gotMethod, gotPath, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotCT = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()
	c := New(srv.URL, Options{})

	if err := c.Drain(12); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v1/backends/12/drain" {
		t.Errorf("Drain sent %s %s", gotMethod, gotPath)
	}
	// rivorad refuses mutations without a JSON content type (CSRF guard).
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json even with no body", gotCT)
	}

	if err := c.Undrain(12); err != nil {
		t.Fatalf("Undrain: %v", err)
	}
	if gotPath != "/api/v1/backends/12/undrain" {
		t.Errorf("Undrain sent path %s", gotPath)
	}
}

func TestSetWeightRequest(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody = nil
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 4, "weight": 5, "vips": 2})
	}))
	defer srv.Close()
	c := New(srv.URL, Options{})

	n, err := c.SetWeight(4, 5, "")
	if err != nil {
		t.Fatalf("SetWeight: %v", err)
	}
	if n != 2 || gotPath != "/api/v1/backends/4/weight" {
		t.Errorf("got vips=%d path=%s", n, gotPath)
	}
	if gotBody["weight"] != float64(5) {
		t.Errorf("body = %v, want weight 5", gotBody)
	}
	if _, has := gotBody["vip"]; has {
		t.Errorf("empty vip must be omitted (means all VIPs), body = %v", gotBody)
	}

	if _, err := c.SetWeight(4, 0, "10.0.0.1:80:tcp"); err != nil {
		t.Fatalf("SetWeight reset: %v", err)
	}
	// Weight 0 (clear override) must still be sent, not dropped as "empty".
	if w, has := gotBody["weight"]; !has || w != float64(0) {
		t.Errorf("body = %v, want explicit weight 0", gotBody)
	}
	if gotBody["vip"] != "10.0.0.1:80:tcp" {
		t.Errorf("vip = %v", gotBody["vip"])
	}
}

func TestMutationErrorsSurfaceServerMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "backend 99: backend not found"})
	}))
	defer srv.Close()

	err := New(srv.URL, Options{}).Drain(99)
	if err == nil || !strings.Contains(err.Error(), "backend not found") {
		t.Errorf("Drain(99) err = %v, want the server's 'backend not found' message", err)
	}
}

func TestUnreachableServerGivesFriendlyError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.URL
	srv.Close() // nothing listening any more

	for name, call := range map[string]func(*Client) error{
		"get":   func(c *Client) error { _, err := c.Status(); return err },
		"drain": func(c *Client) error { return c.Drain(1) },
	} {
		err := call(New(addr, Options{}))
		if err == nil || !strings.Contains(err.Error(), "is it running?") {
			t.Errorf("%s: err = %v, want the 'cannot reach rivorad ... is it running?' hint", name, err)
		}
	}
}
