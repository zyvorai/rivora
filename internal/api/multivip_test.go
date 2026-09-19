// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zyvorai/rivora/internal/dataplane"
)

// fakeReader serves a fixed set of VIPs the way *dataplane.Dataplane does, including Status's rule that
// it answers only for exactly one.
type fakeReader struct {
	vips []dataplane.Status
	err  error
}

func (f fakeReader) Statuses() ([]dataplane.Status, error) { return f.vips, f.err }

func (f fakeReader) Status() (dataplane.Status, error) {
	if f.err != nil {
		return dataplane.Status{}, f.err
	}
	if len(f.vips) != 1 {
		return dataplane.Status{}, fmt.Errorf("%w: %d VIPs", dataplane.ErrNotSingleVIP, len(f.vips))
	}
	return f.vips[0], nil
}

func (f fakeReader) Backends() ([]dataplane.BackendStatus, error) {
	if f.err != nil {
		return nil, f.err
	}
	rows := []dataplane.BackendStatus{}
	for _, v := range f.vips {
		for _, b := range v.Backends {
			b.VIP = v.VIPKey()
			rows = append(rows, b)
		}
	}
	return rows, nil
}

func twoVIPs() []dataplane.Status {
	return []dataplane.Status{
		{VIPAddress: "10.0.0.1", VIPPort: 80, Protocol: "tcp", Backends: []dataplane.BackendStatus{{ID: 0, Address: "10.1.0.1", Port: 80}}},
		{VIPAddress: "10.0.0.2", VIPPort: 443, Protocol: "tcp", Backends: []dataplane.BackendStatus{{ID: 0, Address: "10.1.0.1", Port: 443}, {ID: 1, Address: "10.1.0.2", Port: 443}}},
	}
}

func get(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// The bug: on a node with several VIPs (every Kubernetes node), /api/v1/backends was a 500, and so was
// /api/v1/status, which the console used to check a sign-in.
func TestBackendsListsEveryVIPsBackends(t *testing.T) {
	rec := get(t, &Server{dp: fakeReader{vips: twoVIPs()}}, "/api/v1/backends")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var rows []dataplane.BackendStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0].VIP != "10.0.0.1:80:tcp" || rows[2].VIP != "10.0.0.2:443:tcp" {
		t.Errorf("want one row per (VIP, backend), labelled with its VIP: %+v", rows)
	}
}

func TestBackendsWithNoVIPsIsAnEmptyList(t *testing.T) {
	rec := get(t, &Server{dp: fakeReader{}}, "/api/v1/backends")
	if rec.Code != http.StatusOK || rec.Body.String() != "[]\n" {
		t.Errorf("want 200 [], got %d %q", rec.Code, rec.Body)
	}
}

// /status still answers only for a single VIP, but the refusal is a client-side 409, not a server fault.
func TestStatusOnAMultiVIPNodeIsAConflictNotAServerError(t *testing.T) {
	for name, vips := range map[string][]dataplane.Status{"several": twoVIPs(), "none": nil} {
		rec := get(t, &Server{dp: fakeReader{vips: vips}}, "/api/v1/status")
		if rec.Code != http.StatusConflict {
			t.Errorf("%s VIPs: status %d, want 409: %s", name, rec.Code, rec.Body)
		}
	}
	if rec := get(t, &Server{dp: fakeReader{vips: twoVIPs()[:1]}}, "/api/v1/status"); rec.Code != http.StatusOK {
		t.Errorf("one VIP: status %d, want 200", rec.Code)
	}
}

// A genuine failure reading the maps is still a 500.
func TestReadFailuresStayServerErrors(t *testing.T) {
	s := &Server{dp: fakeReader{err: errors.New("bpf map unreadable")}}
	for _, p := range []string{"/api/v1/status", "/api/v1/backends", "/api/v1/vips"} {
		if rec := get(t, s, p); rec.Code != http.StatusInternalServerError {
			t.Errorf("%s: status %d, want 500", p, rec.Code)
		}
	}
}

// The console signs in by calling /api/v1/vips, so it must answer for any number of VIPs.
func TestVIPsAnswersForAnyNumberOfVIPs(t *testing.T) {
	for _, vips := range [][]dataplane.Status{nil, twoVIPs()[:1], twoVIPs()} {
		if rec := get(t, &Server{dp: fakeReader{vips: vips}}, "/api/v1/vips"); rec.Code != http.StatusOK {
			t.Errorf("%d VIPs: status %d, want 200", len(vips), rec.Code)
		}
	}
}
