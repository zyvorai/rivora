// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package apiclient

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/zyvorai/rivora/internal/dataplane"
	"github.com/zyvorai/rivora/internal/tlsutil"
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

// ---- TLS trust: pinning a certificate instead of skipping verification ----

// tlsServer starts a TLS server with its OWN freshly generated certificate.
// (httptest.NewTLSServer hands every server the same built-in certificate, which
// would make "trust some other server's certificate" trivially succeed.)
func tlsServer(t *testing.T) (*httptest.Server, *x509.Certificate) {
	t.Helper()
	cert, err := tlsutil.GenerateSelfSigned()
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(dataplane.Status{VIPAddress: "10.0.0.1"})
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, leaf
}

func poolOf(cert *x509.Certificate) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return pool
}

func TestPinnedCertificateIsVerified(t *testing.T) {
	srv, cert := tlsServer(t)
	if _, err := New(srv.URL, Options{RootCAs: poolOf(cert)}).Status(); err != nil {
		t.Errorf("a client pinned to the server's own certificate failed: %v", err)
	}
}

func TestAWrongPinnedCertificateIsRejected(t *testing.T) {
	// Verification must actually happen: trusting a *different* certificate has to
	// fail, or "pinning" is just a slower InsecureSkipVerify.
	srv, _ := tlsServer(t)
	_, otherCert := tlsServer(t)
	_, err := New(srv.URL, Options{RootCAs: poolOf(otherCert)}).Status()
	if err == nil {
		t.Fatal("a client pinned to some other certificate accepted this server")
	}
	if !strings.Contains(err.Error(), "--ca-file") {
		t.Errorf("the error should say how to fix it, got: %v", err)
	}
}

func TestPinningTakesPrecedenceOverInsecure(t *testing.T) {
	// If a caller sets both, verification wins: the safer reading. This server's
	// cert is NOT in the pool, so it must be rejected even though Insecure is set.
	srv, _ := tlsServer(t)
	_, otherCert := tlsServer(t)
	if _, err := New(srv.URL, Options{RootCAs: poolOf(otherCert), TLSInsecure: true}).Status(); err == nil {
		t.Error("TLSInsecure overrode a configured CA pool; verification must win")
	}
}

func TestDefaultClientStillRejectsAnUnknownCertificate(t *testing.T) {
	srv, _ := tlsServer(t)
	_, err := New(srv.URL, Options{}).Status()
	if err == nil || !strings.Contains(err.Error(), "not trusted") {
		t.Errorf("err = %v, want a 'not trusted' explanation", err)
	}
}

func TestTLSOptionsImplyHTTPSForABareAddress(t *testing.T) {
	// "host:port" with --tls-insecure or a CA must mean https, not plain HTTP to a
	// TLS listener (which answers with an unexplained 400).
	srv, cert := tlsServer(t)
	bare := strings.TrimPrefix(srv.URL, "https://")
	if _, err := New(bare, Options{TLSInsecure: true}).Status(); err != nil {
		t.Errorf("bare address with TLSInsecure: %v", err)
	}
	if _, err := New(bare, Options{RootCAs: poolOf(cert)}).Status(); err != nil {
		t.Errorf("bare address with a CA pool: %v", err)
	}
}

func TestPlainHTTPToATLSServerExplainsItself(t *testing.T) {
	srv, _ := tlsServer(t)
	plain := "http://" + strings.TrimPrefix(srv.URL, "https://")
	_, err := New(plain, Options{}).Status()
	if err == nil || !strings.Contains(err.Error(), "speaks HTTPS") || !strings.Contains(err.Error(), "https://") {
		t.Errorf("err = %v, want a hint that the server speaks HTTPS", err)
	}
}

func TestAnErrorBodyIsStillReportedAfterTheHTTPSCheck(t *testing.T) {
	// The HTTPS-mismatch check reads the body; a normal JSON error must survive it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "backend id must be a non-negative integer"})
	}))
	defer srv.Close()
	err := New(srv.URL, Options{}).Drain(1)
	if err == nil || !strings.Contains(err.Error(), "non-negative integer") {
		t.Errorf("err = %v, want the server's own message", err)
	}
}

func TestReadOnlyKeyForbiddenSurfacesTheServersExplanation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "forbidden: this key is read-only and cannot change state"})
	}))
	defer srv.Close()
	err := New(srv.URL, Options{APIKey: "reader"}).Drain(1)
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("err = %v, want the read-only explanation", err)
	}
}

func TestLoadCAFile(t *testing.T) {
	srv, cert := tlsServer(t)
	dir := t.TempDir()

	good := dir + "/ca.pem"
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	if err := os.WriteFile(good, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	pool, err := LoadCAFile(good)
	if err != nil {
		t.Fatalf("a valid PEM was rejected: %v", err)
	}
	if _, err := New(srv.URL, Options{RootCAs: pool}).Status(); err != nil {
		t.Errorf("a client using the loaded pool failed: %v", err)
	}

	junk := dir + "/junk.pem"
	_ = os.WriteFile(junk, []byte("not a certificate"), 0o600)
	if _, err := LoadCAFile(junk); err == nil || !strings.Contains(err.Error(), "no PEM certificate") {
		t.Errorf("garbage accepted or unclear error: %v", err)
	}
	if _, err := LoadCAFile(dir + "/missing.pem"); err == nil {
		t.Error("a missing file was accepted")
	}
}
