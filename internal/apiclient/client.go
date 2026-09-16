// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package apiclient is rivoractl's HTTP client for rivorad's local API —
// deliberately a thin net/http wrapper, matching netractl's own hand-rolled
// client rather than pulling in an HTTP client framework.
package apiclient

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/zyvorai/rivora/internal/dataplane"
)

type Client struct {
	base string
	hc   *http.Client
}

func New(addr string) *Client {
	return &Client{
		base: "http://" + addr,
		hc:   &http.Client{Timeout: 5 * time.Second},
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

func (c *Client) get(path string, out any) error {
	resp, err := c.hc.Get(c.base + path)
	if err != nil {
		if _, ok := err.(*net.OpError); ok {
			return fmt.Errorf("cannot reach rivorad at %s (is it running?): %w", c.base, err)
		}
		return err
	}
	defer resp.Body.Close()
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
