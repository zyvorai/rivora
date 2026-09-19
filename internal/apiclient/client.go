// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package apiclient is rivoractl's HTTP client for rivorad's local API —
// deliberately a thin net/http wrapper, matching netractl's own hand-rolled
// client rather than pulling in an HTTP client framework.
package apiclient

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zyvorai/rivora/internal/dataplane"
)

// Options configures the client. APIKey, when set, is sent as a bearer
// token on every request — matching rivorad requiring RIVORA_API_KEY.
// TLSInsecure skips certificate verification, for rivorad's self-signed
// cert (RIVORA_TLS_SELF_SIGNED) — matching netractl's NETRA_TLS_INSECURE.
type Options struct {
	APIKey      string
	TLSInsecure bool
}

type Client struct {
	base   string
	apiKey string
	hc     *http.Client
}

// New creates a client for addr, which may be a bare host:port (defaults to
// http://) or a full http://.../https://... URL.
func New(addr string, opts Options) *Client {
	base := addr
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "http://" + base
	}

	transport := http.DefaultTransport
	if opts.TLSInsecure {
		transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // explicit opt-in for rivorad's self-signed cert
	}

	return &Client{
		base:   base,
		apiKey: opts.APIKey,
		hc:     &http.Client{Timeout: 5 * time.Second, Transport: transport},
	}
}

func (c *Client) Status() (dataplane.Status, error) {
	var st dataplane.Status
	err := c.get("/api/v1/status", &st)
	return st, err
}

func (c *Client) Backends() ([]dataplane.BackendStatus, error) {
	var bs []dataplane.BackendStatus
	err := c.get("/api/v1/backends", &bs)
	return bs, err
}

// VIPs returns every VIP rivorad owns, unlike Status() which only succeeds
// when exactly one is configured.
func (c *Client) VIPs() ([]dataplane.Status, error) {
	var vips []dataplane.Status
	err := c.get("/api/v1/vips", &vips)
	return vips, err
}

// Drain stops backend id from receiving new flows (established ones keep
// flowing). Undrain reverses it. Both act on the operator's own drain flag,
// independent of any Kubernetes-driven drain.
func (c *Client) Drain(id uint32) error   { return c.backendAction(id, "drain", nil) }
func (c *Client) Undrain(id uint32) error { return c.backendAction(id, "undrain", nil) }

// SetWeight overrides backend id's Maglev weight, returning how many VIPs were
// changed. vip ("addr:port:proto") scopes it to one VIP; empty applies to all
// VIPs using the backend. weight 0 clears the override.
func (c *Client) SetWeight(id uint32, weight uint32, vip string) (int, error) {
	body := map[string]any{"weight": weight}
	if vip != "" {
		body["vip"] = vip
	}
	var resp struct {
		VIPs int `json:"vips"`
	}
	err := c.do(http.MethodPost, "/api/v1/backends/"+strconv.FormatUint(uint64(id), 10)+"/weight", body, &resp)
	return resp.VIPs, err
}

func (c *Client) backendAction(id uint32, action string, body any) error {
	return c.do(http.MethodPost, "/api/v1/backends/"+strconv.FormatUint(uint64(id), 10)+"/"+action, body, nil)
}

func (c *Client) get(path string, out any) error {
	return c.do(http.MethodGet, path, nil, out)
}

// do sends one request. A JSON body (when non-nil) always goes with
// Content-Type: application/json, which rivorad requires on mutations as a
// CSRF guard; mutations without a payload still send the header.
func (c *Client) do(method, path string, body, out any) error {
	var rdr *bytes.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(buf)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return err
	}
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		// http.Client wraps transport errors in *url.Error, so a direct type
		// assertion never matched; errors.As unwraps to the *net.OpError.
		var opErr *net.OpError
		if errors.As(err, &opErr) {
			return fmt.Errorf("cannot reach rivorad at %s (is it running?): %w", c.base, err)
		}
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("rivorad: unauthorized — set RIVORA_API_KEY or pass --api-key")
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error != "" {
			return fmt.Errorf("rivorad: %s", e.Error)
		}
		return fmt.Errorf("rivorad: unexpected status %s", resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
