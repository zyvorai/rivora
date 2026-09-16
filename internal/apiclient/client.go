// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package apiclient is rivoractl's HTTP client for rivorad's local API —
// deliberately a thin net/http wrapper, matching netractl's own hand-rolled
// client rather than pulling in an HTTP client framework.
package apiclient

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
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

func (c *Client) get(path string, out any) error {
	req, err := http.NewRequest(http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		if _, ok := err.(*net.OpError); ok {
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
	return json.NewDecoder(resp.Body).Decode(out)
}
