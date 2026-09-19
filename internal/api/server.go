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
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/zyvorai/rivora/internal/dataplane"
	"github.com/zyvorai/rivora/internal/metrics"
)

var errUnauthorized = errors.New("unauthorized")

// Admin is the operator-facing mutation surface (drain/undrain/weight),
// narrowed to an interface so the handlers can be tested without loading BPF
// maps. *dataplane.Dataplane implements it.
type Admin interface {
	SetBackendAdminDraining(backendID uint32, draining bool) error
	SetBackendWeight(backendID uint32, vipKey string, weight uint32) (int, error)
}

type Server struct {
	dp       *dataplane.Dataplane
	admin    Admin
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
	return &Server{dp: dp, admin: dp, apiKey: apiKey, registry: reg}
}

// RegisterBGP adds the rivora_bgp_* series to /metrics. Call it once, before the
// server starts serving, and only when BGP is enabled: a node without BGP then
// exposes no BGP series instead of a misleading set of zeros.
func (s *Server) RegisterBGP(src metrics.BGPSource) {
	s.registry.MustRegister(metrics.NewBGPCollector(src))
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
	mux.HandleFunc("POST /api/v1/backends/{id}/drain", s.handleBackendDrain(true))
	mux.HandleFunc("POST /api/v1/backends/{id}/undrain", s.handleBackendDrain(false))
	mux.HandleFunc("POST /api/v1/backends/{id}/weight", s.handleBackendWeight)
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

// maxAdminBody bounds a mutation request body; the payloads are a few dozen
// bytes, so anything larger is a mistake or abuse.
const maxAdminBody = 4 << 10

// adminRequest validates the parts common to every mutation: an operator
// asked for it with a JSON content type, and the {id} path value is a valid
// backend id. Requiring application/json is a CSRF guard for the
// auth-disabled case — a cross-site <form> can only send simple content types,
// and a cross-origin fetch with application/json triggers a CORS preflight
// this server never approves — so a hostile web page can't drain backends
// through an operator's browser.
func adminRequest(w http.ResponseWriter, r *http.Request) (id uint32, ok bool) {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, errors.New(`Content-Type must be application/json`))
		return 0, false
	}
	n, err := strconv.ParseUint(r.PathValue("id"), 10, 32)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("backend id must be a non-negative integer"))
		return 0, false
	}
	return uint32(n), true
}

// adminStatus maps an admin-call error to an HTTP status: unknown backend or
// VIP is the caller's 404, a rejected value their 400, anything else ours.
func adminStatus(err error) int {
	switch {
	case errors.Is(err, dataplane.ErrBackendNotFound), errors.Is(err, dataplane.ErrVIPNotFound):
		return http.StatusNotFound
	case errors.Is(err, dataplane.ErrInvalidWeight):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func (s *Server) handleBackendDrain(draining bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := adminRequest(w, r)
		if !ok {
			return
		}
		if err := s.admin.SetBackendAdminDraining(id, draining); err != nil {
			writeError(w, adminStatus(err), err)
			return
		}
		writeJSON(w, map[string]any{"id": id, "draining": draining})
	}
}

func (s *Server) handleBackendWeight(w http.ResponseWriter, r *http.Request) {
	id, ok := adminRequest(w, r)
	if !ok {
		return
	}
	var body struct {
		Weight *uint32 `json:"weight"`
		VIP    string  `json:"vip"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAdminBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, errors.New(`body must be JSON like {"weight": 5} (weight 0 clears the override; optional "vip": "addr:port:proto")`))
		return
	}
	if body.Weight == nil {
		writeError(w, http.StatusBadRequest, errors.New(`"weight" is required (0 clears the override)`))
		return
	}
	vips, err := s.admin.SetBackendWeight(id, body.VIP, *body.Weight)
	if err != nil {
		writeError(w, adminStatus(err), err)
		return
	}
	writeJSON(w, map[string]any{"id": id, "weight": *body.Weight, "vips": vips})
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
