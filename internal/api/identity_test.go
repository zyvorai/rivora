// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package api

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/zyvorai/rivora/internal/dataplane"
)

func TestParseCredentialsNamesKeys(t *testing.T) {
	creds, err := parseCredentials("plain, id:alice=s3cret ,id:bob.ops=k3y==,  another")
	if err != nil {
		t.Fatal(err)
	}
	want := []credential{{"key-1", "plain"}, {"alice", "s3cret"}, {"bob.ops", "k3y=="}, {"key-4", "another"}}
	if len(creds) != len(want) {
		t.Fatalf("got %+v, want %+v", creds, want)
	}
	for i := range want {
		if creds[i] != want[i] {
			t.Errorf("credential %d = %+v, want %+v", i, creds[i], want[i])
		}
	}
	// A bare key that happens to end in "=" (base64) is a key, not a name.
	if c, _ := parseCredentials("abc=="); len(c) != 1 || c[0].key != "abc==" {
		t.Errorf("a bare base64 key was misread: %+v", c)
	}
	for _, bad := range []string{"id:noequals", "id:=secret", "id:name=", "id:bad name=x", "id:" + strings.Repeat("a", 65) + "=x", "id:a/b=x"} {
		if _, err := parseCredentials(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		} else if strings.Contains(err.Error(), "secret") && strings.Contains(bad, "secret") {
			t.Errorf("the error echoed the key text: %v", err)
		}
	}
}

func TestValidateKeysNamedKeys(t *testing.T) {
	if err := ValidateKeys("id:alice=a1,id:bob=b1", "id:carol=c1"); err != nil {
		t.Errorf("valid named keys rejected: %v", err)
	}
	if err := ValidateKeys("id:alice=a1", "id:alice=c1"); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("a name reused across keys must be rejected, got %v", err)
	}
	if err := ValidateKeys("id:bad name=x", ""); err == nil {
		t.Error("a malformed named admin key must be rejected")
	}
	if err := ValidateKeys("a1", "id:r=oops,id:r=again"); err == nil {
		t.Error("a duplicate name within one list must be rejected")
	}
	if !matchAny("s3cret", parseKeys("id:alice=s3cret")) {
		t.Error("the secret of a named key must still be what is compared")
	}
	if matchAny("alice", parseKeys("id:alice=s3cret")) || matchAny("id:alice=s3cret", parseKeys("id:alice=s3cret")) {
		t.Error("neither the name nor the whole entry may authenticate")
	}
}

func TestMatchCredReturnsTheKeysName(t *testing.T) {
	creds, _ := parseCredentials("id:alice=one,id:bob=two,three")
	for token, want := range map[string]string{"one": "alice", "two": "bob", "three": "key-3"} {
		if name, ok := matchCred(token, creds); !ok || name != want {
			t.Errorf("matchCred(%q) = %q, %v, want %q", token, name, ok, want)
		}
	}
	if name, ok := matchCred("nope", creds); ok || name != "" {
		t.Errorf("a wrong token matched %q", name)
	}
}

// capture is a logger whose output a test can read.
func capture() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

func newAuditedServer(admin, readOnly string) (*Server, *bytes.Buffer, *prometheus.CounterVec) {
	changes := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_changes_total"}, []string{"user", "code"})
	s := &Server{admin: &fakeAdmin{}, apiKey: admin, changes: changes}
	s.SetReadOnlyKeys(readOnly)
	logger, buf := capture()
	s.SetLogger(logger)
	return s, buf, changes
}

func doReq(h http.Handler, method, path, token string) int {
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestChangesAreAuditedByKeyName(t *testing.T) {
	s, buf, changes := newAuditedServer("id:alice=alice-secret-key,id:bob=bob-secret-key", "id:viewer=viewer-secret-key")
	h := s.Handler()

	if code := doReq(h, http.MethodPost, "/api/v1/backends/1/drain", "alice-secret-key"); code != http.StatusOK {
		t.Fatalf("alice's drain = %d", code)
	}
	if code := doReq(h, http.MethodPost, "/api/v1/backends/2/undrain", "bob-secret-key"); code != http.StatusOK {
		t.Fatalf("bob's undrain = %d", code)
	}
	if code := doReq(h, http.MethodPost, "/api/v1/backends/1/drain", "viewer-secret-key"); code != http.StatusForbidden {
		t.Errorf("a read-only key changed something: %d", code)
	}
	if code := doReq(h, http.MethodPost, "/api/v1/backends/1/drain", "wrong"); code != http.StatusUnauthorized {
		t.Errorf("a wrong key = %d", code)
	}
	if code := doReq(h, http.MethodGet, "/api/v1/nothing", "viewer-secret-key"); code == http.StatusForbidden || code == http.StatusUnauthorized {
		t.Errorf("a read-only key must be able to read: %d", code)
	}

	out := buf.String()
	for _, want := range []string{
		`msg="api change" user=alice role=admin method=POST path=/api/v1/backends/1/drain status=200`,
		`msg="api change" user=bob role=admin method=POST path=/api/v1/backends/2/undrain status=200`,
		`msg="api request rejected" reason=forbidden user=viewer`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	// Reads are not audited, and no key ever reaches the log.
	if strings.Contains(out, "method=GET") {
		t.Errorf("a read was audited:\n%s", out)
	}
	for _, secret := range []string{"alice-secret-key", "bob-secret-key", "viewer-secret-key", "wrong"} {
		if strings.Contains(out, secret) {
			t.Errorf("the log contains a key: %q", secret)
		}
	}
	if got := testutil.ToFloat64(changes.WithLabelValues("alice", "200")); got != 1 {
		t.Errorf("changes{alice,200} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(changes.WithLabelValues("viewer", "200")); got != 0 {
		t.Errorf("a refused change was counted as made: %v", got)
	}
}

func TestAuditRecordsTheStatusOfAFailedChange(t *testing.T) {
	s, buf, changes := newAuditedServer("id:alice=alice-secret-key", "")
	s.admin = &fakeAdmin{err: fmt.Errorf("backend 9: %w", dataplane.ErrBackendNotFound)}
	if code := doReq(s.Handler(), http.MethodPost, "/api/v1/backends/9/drain", "alice-secret-key"); code == http.StatusOK {
		t.Fatalf("the change should have failed, got %d", code)
	}
	if !strings.Contains(buf.String(), "user=alice") || strings.Contains(buf.String(), "status=200") {
		t.Errorf("the audit line must record the failure:\n%s", buf.String())
	}
	if got := testutil.ToFloat64(changes.WithLabelValues("alice", "404")); got != 1 {
		t.Errorf("changes{alice,404} = %v, want 1", got)
	}
}

// ---- client certificates ----

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T, cn string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &testCA{cert: cert, key: key, pem: pemCert(der)}
}

func pemCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func (ca *testCA) issue(t *testing.T, cn string, usage x509.ExtKeyUsage) tls.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// newMTLSServer serves s over TLS accepting client certificates from ca, the way rivorad configures it.
func newMTLSServer(t *testing.T, s *Server, ca *testCA, required bool) *httptest.Server {
	t.Helper()
	serverCert := ca.issue(t, "localhost", x509.ExtKeyUsageServerAuth)
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	auth := tls.VerifyClientCertIfGiven
	if required {
		auth = tls.RequireAndVerifyClientCert
	}
	ts := httptest.NewUnstartedServer(s.Handler())
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}, ClientCAs: pool, ClientAuth: auth, MinVersion: tls.VersionTLS12}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts
}

func clientWith(ca *testCA, cert *tls.Certificate) *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	cfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if cert != nil {
		cfg.Certificates = []tls.Certificate{*cert}
	}
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
}

func statusOf(t *testing.T, c *http.Client, method, url string) (int, error) {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

func TestClientCertificatesAuthenticateWithoutAKey(t *testing.T) {
	ca := newTestCA(t, "rivora test CA")
	s, buf, changes := newAuditedServer("", "") // no keys at all: certificates are the only credential
	s.SetClientCerts([]string{"ops", " deploy-bot "})
	ts := newMTLSServer(t, s, ca, false)

	ops := ca.issue(t, "ops", x509.ExtKeyUsageClientAuth)
	viewer := ca.issue(t, "viewer", x509.ExtKeyUsageClientAuth)
	other := newTestCA(t, "some other CA")
	stranger := other.issue(t, "ops", x509.ExtKeyUsageClientAuth) // right name, wrong CA

	if code, err := statusOf(t, clientWith(ca, &ops), http.MethodPost, ts.URL+"/api/v1/backends/1/drain"); err != nil || code != http.StatusOK {
		t.Errorf("an admin certificate's drain = %d, %v", code, err)
	}
	if code, err := statusOf(t, clientWith(ca, &viewer), http.MethodGet, ts.URL+"/api/v1/nothing"); err != nil || code == http.StatusUnauthorized || code == http.StatusForbidden {
		t.Errorf("a valid certificate must be able to read: %d, %v", code, err)
	}
	if code, _ := statusOf(t, clientWith(ca, &viewer), http.MethodPost, ts.URL+"/api/v1/backends/1/drain"); code != http.StatusForbidden {
		t.Errorf("a certificate not in the admin list changed something: %d", code)
	}
	if code, _ := statusOf(t, clientWith(ca, nil), http.MethodGet, ts.URL+"/api/v1/nothing"); code != http.StatusUnauthorized {
		t.Errorf("no certificate and no key must be 401, got %d", code)
	}
	// A certificate from another CA never authenticates: Go's client does not even offer one the server's CA
	// list does not name, so the request arrives anonymous and is refused (or the handshake fails).
	if code, err := statusOf(t, clientWith(ca, &stranger), http.MethodPost, ts.URL+"/api/v1/backends/1/drain"); err == nil && code != http.StatusUnauthorized {
		t.Errorf("a certificate from an untrusted CA changed something: %d", code)
	}

	out := buf.String()
	if !strings.Contains(out, `msg="api change" user=cert:ops role=admin method=POST path=/api/v1/backends/1/drain status=200`) {
		t.Errorf("the certificate holder must be audited by common name:\n%s", out)
	}
	if !strings.Contains(out, "reason=forbidden user=cert:viewer") {
		t.Errorf("a refused certificate change must name its holder:\n%s", out)
	}
	if got := testutil.ToFloat64(changes.WithLabelValues("cert:ops", "200")); got != 1 {
		t.Errorf("changes{cert:ops,200} = %v, want 1", got)
	}
}

func TestBearerKeysStillWorkBesideCertificates(t *testing.T) {
	ca := newTestCA(t, "rivora test CA")
	s, _, _ := newAuditedServer("id:alice=alice-secret-key", "")
	s.SetClientCerts([]string{"ops"})
	ts := newMTLSServer(t, s, ca, false)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/backends/1/drain", nil)
	req.Header.Set("Authorization", "Bearer alice-secret-key")
	req.Header.Set("Content-Type", "application/json")
	resp, err := clientWith(ca, nil).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("a bearer key beside certificates = %d, want 200", resp.StatusCode)
	}
}

func TestRequiredClientCertificateRejectsAnyoneWithout(t *testing.T) {
	ca := newTestCA(t, "rivora test CA")
	s, _, _ := newAuditedServer("id:alice=alice-secret-key", "")
	s.SetClientCerts(nil)
	ts := newMTLSServer(t, s, ca, true)
	// Even a valid bearer key cannot get past a listener that demands a certificate.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/nothing", nil)
	req.Header.Set("Authorization", "Bearer alice-secret-key")
	if resp, err := clientWith(ca, nil).Do(req); err == nil {
		resp.Body.Close()
		t.Error("a client with no certificate connected to a listener that requires one")
	}
}

// A refused change by an authenticated caller is always logged; only anonymous failures are throttled.
func TestForbiddenChangesByKnownCallersAreNeverThrottled(t *testing.T) {
	s, buf, _ := newAuditedServer("id:alice=alice-secret-key", "id:viewer=viewer-secret-key")
	h := s.Handler()
	for i := 0; i < 3; i++ {
		doReq(h, http.MethodPost, "/api/v1/backends/1/drain", "wrong-key") // anonymous: throttled after the first
		doReq(h, http.MethodPost, "/api/v1/backends/1/drain", "viewer-secret-key")
	}
	if n := strings.Count(buf.String(), "reason=forbidden user=viewer"); n != 3 {
		t.Errorf("%d forbidden lines for a known caller, want 3 (never throttled):\n%s", n, buf.String())
	}
	if n := strings.Count(buf.String(), "reason=unauthenticated"); n != 1 {
		t.Errorf("%d unauthenticated lines, want 1 (throttled):\n%s", n, buf.String())
	}
}

// A certificate the TLS layer accepted without verifying (a listener misconfigured to merely request
// one) must never authenticate anyone: only a verified chain counts.
func TestUnverifiedCertificatesNeverAuthenticate(t *testing.T) {
	ca := newTestCA(t, "rivora test CA")
	s, _, _ := newAuditedServer("", "")
	s.SetClientCerts([]string{"ops"})
	serverCert := ca.issue(t, "localhost", x509.ExtKeyUsageServerAuth)
	ts := httptest.NewUnstartedServer(s.Handler())
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}, ClientAuth: tls.RequestClientCert, MinVersion: tls.VersionTLS12}
	ts.StartTLS()
	t.Cleanup(ts.Close)

	forged := newTestCA(t, "forger").issue(t, "ops", x509.ExtKeyUsageClientAuth) // named like the admin, signed by nobody trusted
	if code, err := statusOf(t, clientWith(ca, &forged), http.MethodPost, ts.URL+"/api/v1/backends/1/drain"); err != nil || code != http.StatusUnauthorized {
		t.Errorf("an unverified certificate named like an admin = %d, %v; want 401", code, err)
	}
}
