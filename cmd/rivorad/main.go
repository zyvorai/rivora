// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// rivorad is Rivora's single-node daemon: loads/pins the XDP ingress (and,
// in NAT mode, TCX egress) BPF programs, applies the static YAML config to
// the maps, runs active health checks, and serves the local HTTP API.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"

	"github.com/zyvorai/rivora/internal/api"
	"github.com/zyvorai/rivora/internal/bgp"
	"github.com/zyvorai/rivora/internal/bpfmaps"
	"github.com/zyvorai/rivora/internal/config"
	"github.com/zyvorai/rivora/internal/controller"
	"github.com/zyvorai/rivora/internal/dataplane"
	"github.com/zyvorai/rivora/internal/gatewayapi"
	"github.com/zyvorai/rivora/internal/healthcheck"
	"github.com/zyvorai/rivora/internal/k8s"
	"github.com/zyvorai/rivora/internal/loader"
	"github.com/zyvorai/rivora/internal/speaker"
	"github.com/zyvorai/rivora/internal/tlsutil"
)

var version = "dev"

func main() {
	var (
		configPath = flag.String("config", "/etc/rivora/config.yaml", "path to VIP/backend config (static-YAML mode; ignored with -kubernetes)")
		bpfDir     = flag.String("bpf-dir", "/usr/local/share/rivora/bpf", "directory containing xdp_ingress.o and tc_nat.o")
		showVer    = flag.Bool("version", false, "print version and exit")

		kubeMode      = flag.Bool("kubernetes", false, "run the Kubernetes reconciler + ARP speaker instead of loading -config; VIPs come from Service/EndpointSlice")
		kubeconfig    = flag.String("kubeconfig", "", "path to a kubeconfig file (default: in-cluster config, falling back to $KUBECONFIG / ~/.kube/config); only used with -kubernetes")
		ifaceFlag     = flag.String("interface", "", "network interface to attach to (required with -kubernetes; static-YAML mode reads this from -config instead)")
		apiListen     = flag.String("api-listen", "127.0.0.1:9870", "local API listen address; only used with -kubernetes (static-YAML mode reads this from -config instead)")
		metricsListen = flag.String("metrics-listen", ":9871", "listen address for /healthz, /readyz and /metrics — unlike -api-listen this is meant to be routable from outside the node (e.g. an in-cluster Prometheus), since rivorad's hostNetwork Pod makes 127.0.0.1 unreachable except from the local kubelet")
		lbClass       = flag.String("loadbalancer-class", "", "only manage Services whose spec.loadBalancerClass matches this value (default: services with no class set); only used with -kubernetes")
		namespace     = flag.String("namespace", envOr("POD_NAMESPACE", "rivora-system"), "namespace the ARP speaker's leader-election Lease lives in; only used with -kubernetes")
		workers       = flag.Int("workers", 2, "number of concurrent Service reconcile workers; only used with -kubernetes")
		speakerOn     = flag.Bool("speaker", true, "run the L2 ARP+NDP speaker (requires CAP_NET_RAW); only used with -kubernetes")

		gatewayAPIOn = flag.Bool("gateway-api", false, "also watch GatewayClass/Gateway/TCPRoute/UDPRoute and program their VIPs; only used with -kubernetes; requires the Gateway API CRDs to be installed")

		healthInterval = flag.Duration("health-interval", 3*time.Second, "active health check interval; only used with -kubernetes")
		healthTimeout  = flag.Duration("health-timeout", time.Second, "active health check timeout; only used with -kubernetes")
		healthFail     = flag.Int("health-fail-threshold", 2, "consecutive failures before marking a backend down; only used with -kubernetes")
		healthSuccess  = flag.Int("health-success-threshold", 2, "consecutive successes before marking a backend healthy; only used with -kubernetes")

		rateLimitOn    = flag.Bool("rate-limit", false, "enable per-source-IP SYN-flood rate limiting; only used with -kubernetes (static-YAML mode reads this from -config's rateLimit section instead)")
		rateLimitPPS   = flag.Uint64("rate-limit-pps", 0, "per-source-IP new-connection (SYN) packets/sec allowed when -rate-limit is set; only used with -kubernetes")
		rateLimitBurst = flag.Uint64("rate-limit-burst", 0, "per-source-IP token-bucket burst size when -rate-limit is set; only used with -kubernetes")

		bgpOn          = flag.Bool("bgp", false, "advertise a BGP route for every VIP this node has a healthy backend for (active/active ECMP HA); only used with -kubernetes (static-YAML mode reads this from -config's bgp section instead)")
		bgpASN         = flag.Uint("bgp-asn", 0, "this node's BGP AS number when -bgp is set; only used with -kubernetes")
		bgpRouterID    = flag.String("bgp-router-id", "", "this node's BGP router-id (an IPv4 address, need not be routable) when -bgp is set; only used with -kubernetes")
		bgpIPv6NextHop = flag.String("bgp-ipv6-next-hop", "", "IPv6 next-hop for advertised IPv6 VIP /128 routes when -bgp is set; required to advertise IPv6 VIPs; only used with -kubernetes")
		bgpPeers       = flag.String("bgp-peers", "", "comma-separated BGP peers when -bgp is set, each addr:asn or addr:asn:bfd (e.g. \"10.0.0.1:65000:bfd,10.0.0.2:65001\"); only used with -kubernetes")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("rivorad", version)
		return
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var cfg config.Config
	if *kubeMode {
		if *ifaceFlag == "" {
			logger.Error("-interface is required with -kubernetes")
			os.Exit(1)
		}
		cfg = config.Config{
			Interface: *ifaceFlag,
			APIListen: *apiListen,
			HealthCheck: config.HealthCheck{
				Interval:         *healthInterval,
				Timeout:          *healthTimeout,
				FailThreshold:    *healthFail,
				SuccessThreshold: *healthSuccess,
			},
			RateLimit: config.RateLimit{
				Enabled:                   *rateLimitOn,
				PerSourcePacketsPerSecond: *rateLimitPPS,
				Burst:                     *rateLimitBurst,
			},
		}
		if *rateLimitOn && (*rateLimitPPS == 0 || *rateLimitBurst == 0) {
			logger.Error("-rate-limit-pps and -rate-limit-burst must both be > 0 with -rate-limit")
			os.Exit(1)
		}
		if *bgpOn {
			peers, perr := parseBGPPeers(*bgpPeers)
			if perr != nil {
				logger.Error("parse -bgp-peers", "err", perr)
				os.Exit(1)
			}
			cfg.BGP = config.BGP{
				Enabled:     true,
				ASN:         uint32(*bgpASN),
				RouterID:    *bgpRouterID,
				IPv6NextHop: *bgpIPv6NextHop,
				Peers:       peers,
			}
			if err := cfg.BGP.Validate(); err != nil {
				logger.Error("bgp flags", "err", err)
				os.Exit(1)
			}
		}
	} else {
		var err error
		cfg, err = config.Load(*configPath)
		if err != nil {
			logger.Error("load config", "err", err)
			os.Exit(1)
		}
	}

	iface, err := net.InterfaceByName(cfg.Interface)
	if err != nil {
		logger.Error("interface lookup", "interface", cfg.Interface, "err", err)
		os.Exit(1)
	}

	natObj := ""
	if *kubeMode {
		natObj = bpfDirPath(*bpfDir, bpfmaps.ProgTCNATEgress) // K8s-managed VIPs are NAT-only (v0.2)
	} else {
		for _, vip := range cfg.VIPs {
			if vip.Mode == config.ModeNAT {
				natObj = bpfDirPath(*bpfDir, bpfmaps.ProgTCNATEgress)
				break
			}
		}
	}

	dp, err := loader.Load(bpfDirPath(*bpfDir, bpfmaps.ProgXDPIngress), natObj)
	if err != nil {
		logger.Error("load bpf objects", "err", err)
		os.Exit(1)
	}
	defer dp.Close()

	if err := dp.AttachXDP(iface); err != nil {
		logger.Error("attach xdp", "err", err)
		os.Exit(1)
	}
	if natObj != "" {
		if err := dp.AttachTCXEgress(iface); err != nil {
			logger.Error("attach tcx egress", "err", err)
			os.Exit(1)
		}
	}
	logger.Info("attached", "interface", cfg.Interface, "vips", len(cfg.VIPs), "nat_egress", natObj != "")

	plane := dataplane.New(cfg, dp)
	if err := plane.Apply(iface); err != nil {
		logger.Error("apply config", "err", err)
		os.Exit(1)
	}

	// bgpSpeaker is not Kubernetes-specific (unlike the ARP speaker, which
	// needs a K8s Lease for its cluster-wide mutual exclusion) — a
	// standalone static-YAML deployment across multiple nodes can equally
	// want BGP+ECMP HA, so it's built here, before the -kubernetes branch,
	// from whichever mode populated cfg.BGP (the YAML file's bgp: section,
	// or the -bgp* flags above).
	var bgpSpeaker *bgp.Speaker
	if cfg.BGP.Enabled {
		var err error
		bgpSpeaker, err = bgp.New(cfg.BGP, plane, logger)
		if err != nil {
			logger.Error("start bgp speaker", "err", err)
			os.Exit(1)
		}
		defer bgpSpeaker.Close()
	}

	checker := healthcheck.New(
		cfg.HealthCheck.Interval, cfg.HealthCheck.Timeout,
		cfg.HealthCheck.FailThreshold, cfg.HealthCheck.SuccessThreshold,
		func(backendID uint32, healthy bool) {
			logger.Info("backend health changed", "backend", backendID, "healthy", healthy)
			if err := plane.SetBackendHealth(backendID, healthy); err != nil {
				logger.Error("update backend health", "backend", backendID, "err", err)
			}
			// A backend flipping healthy/unhealthy on an already-installed
			// VIP is exactly the signal BGP's health-gated advertise/
			// withdraw model is built around — react immediately rather
			// than waiting for the speaker's own periodic resync.
			if bgpSpeaker != nil {
				bgpSpeaker.AnnounceNow()
			}
		},
	)
	var targets []healthcheck.Target
	for _, t := range plane.Targets() {
		targets = append(targets, healthcheck.Target{BackendID: t.ID, Address: t.Address, Port: t.Port})
	}
	checker.SetTargets(targets)
	go checker.Run()
	defer checker.Stop()

	if bgpSpeaker != nil {
		go func() {
			if err := bgpSpeaker.Run(ctx); err != nil {
				logger.Error("bgp speaker exited", "err", err)
			}
		}()
		logger.Info("bgp speaker enabled", "asn", cfg.BGP.ASN, "router_id", cfg.BGP.RouterID, "peers", len(cfg.BGP.Peers))
	}

	if *kubeMode {
		kcfg, err := k8s.BuildConfig(*kubeconfig)
		if err != nil {
			logger.Error("build kubeconfig", "err", err)
			os.Exit(1)
		}
		clients, err := k8s.New(kcfg)
		if err != nil {
			logger.Error("build k8s clients", "err", err)
			os.Exit(1)
		}

		reconciler, factory := controller.New(clients.Clientset, plane, *lbClass, logger)

		var gwReconciler *gatewayapi.Reconciler
		var gwFactory informers.SharedInformerFactory
		var gwDynFactory dynamicinformer.DynamicSharedInformerFactory
		if *gatewayAPIOn {
			gwReconciler, gwFactory, gwDynFactory = gatewayapi.New(clients.Clientset, clients.Dynamic, plane, logger)
		}

		var sp *speaker.Speaker
		if *speakerOn {
			identity, herr := os.Hostname()
			if herr != nil || identity == "" {
				identity = fmt.Sprintf("rivorad-%d", os.Getpid())
			}
			sp, err = speaker.New(iface, plane, clients.Clientset, *namespace, identity, logger)
			if err != nil {
				logger.Error("start arp speaker", "err", err)
				os.Exit(1)
			}
			defer sp.Close()
		}

		onChange := func() {
			var targets []healthcheck.Target
			for _, t := range plane.Targets() {
				targets = append(targets, healthcheck.Target{BackendID: t.ID, Address: t.Address, Port: t.Port})
			}
			checker.SetTargets(targets)
			if sp != nil {
				sp.AnnounceNow()
			}
			if bgpSpeaker != nil {
				bgpSpeaker.AnnounceNow()
			}
		}
		reconciler.OnChange = onChange
		if gwReconciler != nil {
			gwReconciler.OnChange = onChange
		}

		go func() {
			if err := reconciler.Run(ctx, factory, *workers); err != nil {
				logger.Error("k8s reconciler exited", "err", err)
			}
		}()
		if gwReconciler != nil {
			go func() {
				if err := gwReconciler.Run(ctx, gwFactory, gwDynFactory, *workers); err != nil {
					logger.Error("gateway api reconciler exited", "err", err)
				}
			}()
		}
		if sp != nil {
			go func() {
				if err := sp.Run(ctx); err != nil {
					logger.Error("arp speaker exited", "err", err)
				}
			}()
		}
		logger.Info("kubernetes mode enabled", "lb_class", *lbClass, "speaker", *speakerOn, "gateway_api", *gatewayAPIOn)
	}

	apiKey := os.Getenv("RIVORA_API_KEY")
	tlsCert := os.Getenv("RIVORA_TLS_CERT")
	tlsKey := os.Getenv("RIVORA_TLS_KEY")
	selfSigned := os.Getenv("RIVORA_TLS_SELF_SIGNED") != ""

	apiServer := api.New(plane, apiKey)

	// metricsSrv is deliberately separate from the (loopback-only, optionally
	// TLS'd/authenticated) API server above: rivorad runs hostNetwork, so
	// 127.0.0.1 is unreachable to anything but the local kubelet, and an
	// in-cluster Prometheus needs a routable, plain-HTTP address instead.
	// /healthz, /readyz and /metrics carry no sensitive data, so no auth here.
	metricsSrv := &http.Server{Addr: *metricsListen, Handler: apiServer.MetricsHandler()}
	go func() {
		if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("metrics server", "err", err)
		}
	}()
	defer metricsSrv.Close()

	srv := &http.Server{Addr: cfg.APIListen, Handler: apiServer.Handler()}
	tlsMode := "off"
	switch {
	case tlsCert != "" && tlsKey != "":
		tlsMode = "file"
	case selfSigned:
		tlsMode = "self-signed"
	}
	authMode := "off"
	if apiKey != "" {
		authMode = "on"
	}
	logger.Info("api listening", "addr", cfg.APIListen, "tls", tlsMode, "auth", authMode)
	logger.Info("metrics listening", "addr", *metricsListen)

	go func() {
		var err error
		switch tlsMode {
		case "file":
			err = srv.ListenAndServeTLS(tlsCert, tlsKey)
		case "self-signed":
			cert, cerr := tlsutil.GenerateSelfSigned()
			if cerr != nil {
				logger.Error("generate self-signed cert", "err", cerr)
				os.Exit(1)
			}
			srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
			err = srv.ListenAndServeTLS("", "")
		default:
			err = srv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			logger.Error("api server", "err", err)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	_ = srv.Close()
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func bpfDirPath(dir, prog string) string {
	name := "xdp_ingress.o"
	if prog == bpfmaps.ProgTCNATEgress {
		name = "tc_nat.o"
	}
	return dir + "/" + name
}

// parseBGPPeers parses -bgp-peers' comma-separated "addr:asn" or
// "addr:asn:bfd" entries. An empty string yields no peers (letting
// config.BGP.Validate's "at least one peer is required" check produce a
// clear error rather than a confusing parse failure).
func parseBGPPeers(s string) ([]config.BGPPeer, error) {
	if s == "" {
		return nil, nil
	}
	var peers []config.BGPPeer
	for _, entry := range strings.Split(s, ",") {
		parts := strings.Split(entry, ":")
		if len(parts) < 2 || len(parts) > 3 {
			return nil, fmt.Errorf("invalid -bgp-peers entry %q: want addr:asn or addr:asn:bfd", entry)
		}
		asn, err := strconv.ParseUint(parts[1], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid -bgp-peers entry %q: asn: %w", entry, err)
		}
		peer := config.BGPPeer{Address: parts[0], ASN: uint32(asn)}
		if len(parts) == 3 {
			if parts[2] != "bfd" {
				return nil, fmt.Errorf("invalid -bgp-peers entry %q: third field must be \"bfd\"", entry)
			}
			peer.BFD = true
		}
		peers = append(peers, peer)
	}
	return peers, nil
}
