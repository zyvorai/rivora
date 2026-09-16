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
