// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package api serves rivorad's local HTTP API — the same shape rivoractl
// and netractl both expect: plain JSON over a loopback listener. Auth is
// off by default (matching netrad's own binary-level default) but, when an
// API key is configured, every route requires a matching bearer token —
// same constant-time-compare shape as netrad's validBearer().
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/zyvorai/rivora/internal/dataplane"
)

var errUnauthorized = errors.New("unauthorized")

type Server struct {
	dp     *dataplane.Dataplane
	apiKey string
}

// New creates a Server. apiKey may be empty, in which case the API is
// unauthenticated (today's default behavior).
func New(dp *dataplane.Dataplane, apiKey string) *Server {
	return &Server{dp: dp, apiKey: apiKey}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/status", s.handleStatus)
	mux.HandleFunc("GET /api/v1/vips", s.handleVIPs)
	mux.HandleFunc("GET /api/v1/backends", s.handleBackends)
	return s.auth(mux)
}

func (s *Server) auth(next http.Handler) http.Handler {
	if s.apiKey == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.validBearer(r.Header.Get("Authorization")) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="rivorad"`)
			writeError(w, http.StatusUnauthorized, errUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) validBearer(header string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	token := strings.TrimPrefix(header, prefix)
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.apiKey)) == 1
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.dp.Status()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, st)
}

func (s *Server) handleVIPs(w http.ResponseWriter, r *http.Request) {
	sts, err := s.dp.Statuses()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, sts)
}

func (s *Server) handleBackends(w http.ResponseWriter, r *http.Request) {
	st, err := s.dp.Status()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, st.Backends)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
