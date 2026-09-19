// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zyvorai/rivora/internal/dataplane"
)

type fakeAdmin struct {
	drainID   uint32
	drainSet  *bool
	weightID  uint32
	weightVIP string
	weightVal uint32
	err       error
	vips      int
}

func (f *fakeAdmin) SetBackendAdminDraining(id uint32, draining bool) error {
	f.drainID, f.drainSet = id, &draining
	return f.err
}

func (f *fakeAdmin) SetBackendWeight(id uint32, vip string, w uint32) (int, error) {
	f.weightID, f.weightVIP, f.weightVal = id, vip, w
	return f.vips, f.err
}

func post(t *testing.T, h http.Handler, path, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestDrainAndUndrain(t *testing.T) {
	f := &fakeAdmin{}
	h := (&Server{admin: f}).Handler()

	rec := post(t, h, "/api/v1/backends/7/drain", "application/json", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("drain: status %d, body %s", rec.Code, rec.Body)
	}
	if f.drainSet == nil || !*f.drainSet || f.drainID != 7 {
		t.Errorf("drain called with id=%d draining=%v, want 7/true", f.drainID, f.drainSet)
	}

	rec = post(t, h, "/api/v1/backends/7/undrain", "application/json; charset=utf-8", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("undrain: status %d, body %s", rec.Code, rec.Body)
	}
	if f.drainSet == nil || *f.drainSet {
		t.Errorf("undrain should set draining=false, got %v", f.drainSet)
	}
}

func TestAdminRejectsNonJSONContentType(t *testing.T) {
	// A cross-site <form> can only send these simple content types; refusing
	// them is the CSRF guard when the API runs without a bearer token.
	for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data"} {
		f := &fakeAdmin{}
		rec := post(t, (&Server{admin: f}).Handler(), "/api/v1/backends/1/drain", ct, "")
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Errorf("content-type %q: status %d, want 415", ct, rec.Code)
		}
		if f.drainSet != nil {
			t.Errorf("content-type %q: admin was called despite rejection", ct)
		}
	}
}

func TestAdminRejectsBadBackendID(t *testing.T) {
	for _, id := range []string{"abc", "-1", "1.5", "99999999999"} {
		f := &fakeAdmin{}
		rec := post(t, (&Server{admin: f}).Handler(), "/api/v1/backends/"+id+"/drain", "application/json", "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("id %q: status %d, want 400", id, rec.Code)
		}
		if f.drainSet != nil {
			t.Errorf("id %q: admin was called despite rejection", id)
		}
	}
}

func TestAdminErrorStatusMapping(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{fmt.Errorf("backend 9: %w", dataplane.ErrBackendNotFound), http.StatusNotFound},
		{fmt.Errorf("vip x: %w", dataplane.ErrVIPNotFound), http.StatusNotFound},
		{fmt.Errorf("weight: %w", dataplane.ErrInvalidWeight), http.StatusBadRequest},
		{fmt.Errorf("update map: boom"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		rec := post(t, (&Server{admin: &fakeAdmin{err: c.err}}).Handler(), "/api/v1/backends/9/drain", "application/json", "")
		if rec.Code != c.want {
			t.Errorf("%v: status %d, want %d", c.err, rec.Code, c.want)
		}
		var e map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e["error"] == "" {
			t.Errorf("%v: body %q is not a JSON error", c.err, rec.Body)
		}
	}
}

func TestSetWeight(t *testing.T) {
	f := &fakeAdmin{vips: 2}
	h := (&Server{admin: f}).Handler()

	rec := post(t, h, "/api/v1/backends/3/weight", "application/json", `{"weight": 5, "vip": "10.0.0.1:80:tcp"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	if f.weightID != 3 || f.weightVal != 5 || f.weightVIP != "10.0.0.1:80:tcp" {
		t.Errorf("SetBackendWeight got id=%d weight=%d vip=%q", f.weightID, f.weightVal, f.weightVIP)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["vips"] != float64(2) || resp["weight"] != float64(5) {
		t.Errorf("response %v, want weight=5 vips=2", resp)
	}
}

func TestSetWeightZeroIsAValidReset(t *testing.T) {
	// 0 means "clear the override" and must not be confused with "missing".
	f := &fakeAdmin{vips: 1}
	rec := post(t, (&Server{admin: f}).Handler(), "/api/v1/backends/3/weight", "application/json", `{"weight": 0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	if f.weightVal != 0 || f.weightID != 3 {
		t.Errorf("got id=%d weight=%d, want 3/0", f.weightID, f.weightVal)
	}
}

func TestSetWeightBadBodies(t *testing.T) {
	for name, body := range map[string]string{
		"empty":         ``,
		"not json":      `weight=5`,
		"missing field": `{}`,
		"negative":      `{"weight": -1}`,
		"string":        `{"weight": "5"}`,
		"unknown field": `{"weight": 5, "wieght": 1}`,
	} {
		f := &fakeAdmin{}
		rec := post(t, (&Server{admin: f}).Handler(), "/api/v1/backends/3/weight", "application/json", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, rec.Code)
		}
		if f.weightVal != 0 || f.weightID != 0 {
			t.Errorf("%s: admin was called despite rejection", name)
		}
	}
}

func TestAdminRoutesRequireAuthWhenKeySet(t *testing.T) {
	f := &fakeAdmin{}
	h := (&Server{admin: f, apiKey: "secret"}).Handler()

	rec := post(t, h, "/api/v1/backends/1/drain", "application/json", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: status %d, want 401", rec.Code)
	}
	if f.drainSet != nil {
		t.Fatal("drain executed without authentication")
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/backends/1/drain", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("valid token: status %d, want 200", rec.Code)
	}
}

func TestGETOnAdminRouteIsNotAllowed(t *testing.T) {
	h := (&Server{admin: &fakeAdmin{}}).Handler()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/backends/1/drain", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Errorf("GET on a mutation route returned 200; mutations must be POST-only")
	}
}
