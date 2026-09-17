// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package api serves rivorad's HTTP API and embedded web console. Auth is
// off by default (matching netrad's own binary-level default) but, when an
// API key is configured, every /api/* route requires a matching bearer
// token — same constant-time-compare shape as netrad's validBearer(). The
// console UI itself stays reachable so the browser can collect credentials.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/zyvorai/rivora/internal/dataplane"
	"github.com/zyvorai/rivora/internal/metrics"
)

var errUnauthorized = errors.New("unauthorized")

type Server struct {
	dp       *dataplane.Dataplane
	apiKey   string
	registry *prometheus.Registry
}

// New creates a Server. apiKey may be empty, in which case the API is
// unauthenticated (today's default behavior).
func New(dp *dataplane.Dataplane, apiKey string) *Server {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		metrics.NewDataplaneCollector(dp),
	)
	return &Server{dp: dp, apiKey: apiKey, registry: reg}
}

// MetricsHandler serves only /healthz, /readyz and /metrics — no console,
// no /api/*. Meant for a second listener bound wider than the main API's
// loopback-only address (see cmd/rivorad), since a hostNetwork Pod's
// 127.0.0.1 isn't reachable from an in-cluster Prometheus or from kubelet
// unless kubelet itself runs on that node — which it does for probes, but
// a cluster-wide scraper needs this on a routable address instead.
func (s *Server) MetricsHandler() http.Handler {
	mux := http.NewServeMux()
	s.registerObservabilityRoutes(mux)
	return mux
}

func (s *Server) registerObservabilityRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	if s.registry != nil {
		mux.Handle("GET /metrics", promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{}))
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/status", s.handleStatus)
	mux.HandleFunc("GET /api/v1/vips", s.handleVIPs)
	mux.HandleFunc("GET /api/v1/backends", s.handleBackends)
	// /healthz, /readyz and /metrics are deliberately outside /api/* so they
	// stay reachable without a bearer token — same routes MetricsHandler
	// serves standalone, kept here too so a local kubelet/rivoractl caller
	// can still reach them over the loopback-only API address.
	s.registerObservabilityRoutes(mux)

	uiFS, err := fs.Sub(uiContent, "ui")
	if err != nil {
		panic("api: embed ui: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(uiFS))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		// SPA: unknown paths fall back to index.html (Netra console shape).
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		f, err := uiFS.Open(path)
		if err != nil {
			http.ServeFileFS(w, r, uiFS, "index.html")
			return
		}
		_ = f.Close()
		fileServer.ServeHTTP(w, r)
	})

	return s.auth(mux)
}

func (s *Server) auth(next http.Handler) http.Handler {
	if s.apiKey == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
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

// handleHealthz is a liveness probe: it only proves the HTTP server is up
// and answering, not that the dataplane itself is healthy (see handleReadyz
// for that check).
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleReadyz is a readiness probe: it reads the BPF maps the same way
// handleVIPs does, so a node whose pinned maps have gone missing or become
// unreadable (e.g. /sys/fs/bpf/rivora-lb was tampered with) fails readiness
// even though the process is still alive.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if _, err := s.dp.Statuses(); err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
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
