// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package api serves rivorad's local HTTP API — the same shape rivoractl
// and netractl both expect: plain JSON over a loopback listener, no auth
// (loopback-only, like netrad's 127.0.0.1 API).
package api

import (
	"encoding/json"
	"net/http"

	"github.com/zyvorai/rivora/internal/dataplane"
)

type Server struct {
	dp *dataplane.Dataplane
}

func New(dp *dataplane.Dataplane) *Server {
	return &Server{dp: dp}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/status", s.handleStatus)
	mux.HandleFunc("GET /api/v1/vips", s.handleVIPs)
	mux.HandleFunc("GET /api/v1/backends", s.handleBackends)
	return mux
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
	st, err := s.dp.Status()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, []dataplane.Status{st})
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
