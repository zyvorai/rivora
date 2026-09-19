// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// rivorad is Rivora's single-node daemon: loads/pins the XDP ingress (and,
// in NAT mode, TCX egress) BPF programs, applies the static YAML config to
// the maps, runs active health checks, and serves the local HTTP API.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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

	"github.com/zyvorai/rivora/api/v1alpha1"
	"github.com/zyvorai/rivora/internal/api"
	"github.com/zyvorai/rivora/internal/bgp"
	"github.com/zyvorai/rivora/internal/bgppeers"
	"github.com/zyvorai/rivora/internal/bpfmaps"
	"github.com/zyvorai/rivora/internal/config"
	"github.com/zyvorai/rivora/internal/controller"
	"github.com/zyvorai/rivora/internal/dataplane"
	"github.com/zyvorai/rivora/internal/gatewayapi"
	"github.com/zyvorai/rivora/internal/healthcheck"
	"github.com/zyvorai/rivora/internal/initsync"
	"github.com/zyvorai/rivora/internal/k8s"
	"github.com/zyvorai/rivora/internal/loader"
	"github.com/zyvorai/rivora/internal/logging"
	"github.com/zyvorai/rivora/internal/speaker"
	"github.com/zyvorai/rivora/internal/tlsutil"
)

var version = "dev"

func main() {
	var (
		configPath = flag.String("config", "/etc/rivora/config.yaml", "path to VIP/backend config (static-YAML mode; ignored with -kubernetes)")
		bpfDir     = flag.String("bpf-dir", "/usr/local/share/rivora/bpf", "directory containing xdp_ingress.o and tc_nat.o")
		showVer    = flag.Bool("version", false, "print version and exit")
		logLevel   = flag.String("log-level", "info", "log level: debug, info, warn or error")
		logFormat  = flag.String("log-format", "text", "log format: text or json")

		persistDatapath = flag.Bool("persist-datapath", false, "pin the XDP/TCX links so the datapath keeps forwarding while rivorad is down, and hot-swap the program on the next start (no traffic gap on restart). The datapath then stays attached after rivorad exits — use -detach to remove it")
		xdpModeFlag     = flag.String("xdp-mode", "generic", "how the XDP program attaches: generic (works on any interface; the default), native (in the NIC driver: much faster, needs driver support, fails if unavailable) or auto (try native, fall back to generic); only used with -kubernetes (static-YAML mode reads xdpMode from -config)")
		detach          = flag.Bool("detach", false, "remove datapath links left attached by a -persist-datapath run, then exit (maps are kept)")

		kubeMode      = flag.Bool("kubernetes", false, "run the Kubernetes reconciler + ARP speaker instead of loading -config; VIPs come from Service/EndpointSlice")
		kubeconfig    = flag.String("kubeconfig", "", "path to a kubeconfig file (default: in-cluster config, falling back to $KUBECONFIG / ~/.kube/config); only used with -kubernetes")
		ifaceFlag     = flag.String("interface", "", "network interface to attach to (required with -kubernetes; static-YAML mode reads this from -config instead)")
		apiListen     = flag.String("api-listen", "127.0.0.1:9870", "local API listen address; only used with -kubernetes (static-YAML mode reads this from -config instead)")
		metricsListen = flag.String("metrics-listen", ":9871", "listen address for /healthz, /readyz and /metrics — unlike -api-listen this is meant to be routable from outside the node (e.g. an in-cluster Prometheus), since rivorad's hostNetwork Pod makes 127.0.0.1 unreachable except from the local kubelet")
		lbClass       = flag.String("loadbalancer-class", "", "only manage Services whose spec.loadBalancerClass matches this value (default: services with no class set); only used with -kubernetes")
		namespace     = flag.String("namespace", envOr("POD_NAMESPACE", "rivora-system"), "namespace the ARP speaker's leader-election Lease lives in; only used with -kubernetes")
		nodeName      = flag.String("node-name", os.Getenv("NODE_NAME"), "this node's Kubernetes name (default $NODE_NAME, which the Helm chart sets from spec.nodeName); needed to honour externalTrafficPolicy: Local, which is only honoured with -bgp and -speaker=false; only used with -kubernetes")
		workers       = flag.Int("workers", 2, "number of concurrent Service reconcile workers; only used with -kubernetes")
		speakerOn     = flag.Bool("speaker", true, "run the L2 ARP+NDP speaker (requires CAP_NET_RAW); only used with -kubernetes")

		servicePolicyOn = flag.Bool("service-policy", false, "honour ServicePolicy objects (per-Service health probe, per-source rate limit and endpoint weights); only used with -kubernetes; requires the ServicePolicy CRD (the Helm chart installs it)")
		gatewayAPIOn    = flag.Bool("gateway-api", false, "also watch GatewayClass/Gateway/TCPRoute/UDPRoute and program their VIPs; only used with -kubernetes; requires the Gateway API CRDs to be installed")

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
		bgpConfigFile  = flag.String("bgp-config", "", "path to a YAML file with a bgp: section (the same schema as the static config's), for BGP options -bgp-peers cannot express: peer passwords, multihop, graceful restart, communities, local-pref, aggregates, per-node peers; replaces -bgp, -bgp-asn, -bgp-router-id, -bgp-ipv6-next-hop and -bgp-peers; only used with -kubernetes")
		bgpPeerRes     = flag.Bool("bgp-peer-resources", false, "also read BGPPeer resources: each adds a BGP neighbour (with password, multihop, graceful restart and a node selector) to the peers this node started with; needs BGP (-bgp or -bgp-config) and the BGPPeer CRD (the Helm chart installs it); only used with -kubernetes")
		bgpPeers       = flag.String("bgp-peers", "", "comma-separated BGP peers when -bgp is set, each addr:asn or addr:asn:bfd (e.g. \"10.0.0.1:65000:bfd,10.0.0.2:65001\"); only used with -kubernetes")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("rivorad", version)
		return
	}

	logger, err := logging.New(os.Stdout, *logLevel, *logFormat)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rivorad:", err)
		os.Exit(2)
	}

	// Validate the API keys before anything touches the kernel: a bad security
	// configuration should stop rivorad before it has loaded BPF or attached XDP.
	// RIVORA_API_KEY is the admin credential and, being set, what turns API
	// authentication on. Either variable may hold a comma-separated list, which is
	// how a key is rotated with no outage. RIVORA_API_READONLY_KEY grants keys that
	// can read but not change anything.
	apiKey := os.Getenv("RIVORA_API_KEY")
	readOnlyKey := os.Getenv("RIVORA_API_READONLY_KEY")
	if err := api.ValidateKeys(apiKey, readOnlyKey); err != nil {
		logger.Error("api key configuration", "err", err)
		os.Exit(1)
	}
	if n := api.WeakKeys(apiKey) + api.WeakKeys(readOnlyKey); n > 0 {
		logger.Warn("some API keys are shorter than recommended; generate one with `openssl rand -hex 24`",
			"keys", n, "min_length", api.MinKeyLength)
	}

	if *detach {
		removed, err := loader.DetachPersisted()
		if err != nil {
			logger.Error("detach persisted datapath", "err", err, "removed", removed)
			os.Exit(1)
		}
		logger.Info("detached persisted datapath", "links", removed)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// SIGHUP reloads the static-YAML VIP set. Registered this early so a HUP
	// arriving while the dataplane is still coming up is queued rather than
	// taking the Go default action (terminate). Not used with -kubernetes,
	// where the reconcilers own the VIP set and there is no file to re-read;
	// a nil channel there simply never fires.
	var hup chan os.Signal
	if !*kubeMode {
		hup = make(chan os.Signal, 1)
		signal.Notify(hup, syscall.SIGHUP)
		defer signal.Stop(hup)
	}

	var cfg config.Config
	if *kubeMode {
		if *ifaceFlag == "" {
			logger.Error("-interface is required with -kubernetes")
			os.Exit(1)
		}
		cfg = config.Config{
			Interface: *ifaceFlag,
			XDPMode:   config.XDPMode(*xdpModeFlag),
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
		if err := cfg.XDPMode.Validate(); err != nil {
			logger.Error("-xdp-mode", "err", err)
			os.Exit(1)
		}
		if *rateLimitOn && (*rateLimitPPS == 0 || *rateLimitBurst == 0) {
			logger.Error("-rate-limit-pps and -rate-limit-burst must both be > 0 with -rate-limit")
			os.Exit(1)
		}
		if *bgpConfigFile != "" {
			if *bgpOn || *bgpASN != 0 || *bgpRouterID != "" || *bgpPeers != "" || *bgpIPv6NextHop != "" {
				logger.Error("-bgp-config replaces -bgp, -bgp-asn, -bgp-router-id, -bgp-ipv6-next-hop and -bgp-peers; set one or the other")
				os.Exit(1)
			}
			b, berr := config.LoadBGPWith(*bgpConfigFile, *bgpPeerRes)
			if berr != nil {
				logger.Error("load -bgp-config", "err", berr)
				os.Exit(1)
			}
			cfg.BGP = b
			*bgpOn = true // the rest of start-up (speaker, externalTrafficPolicy: Local) keys off this
		} else if *bgpOn {
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

				PeersFromResources: *bgpPeerRes,
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
	} else if config.HasNATVIP(cfg.VIPs) {
		natObj = bpfDirPath(*bpfDir, bpfmaps.ProgTCNATEgress)
	}

	dp, err := loader.Load(bpfDirPath(*bpfDir, bpfmaps.ProgXDPIngress), natObj)
	if err != nil {
		logger.Error("load bpf objects", "err", err)
		os.Exit(1)
	}
	defer dp.Close()
	dp.Persist = *persistDatapath
	dp.XDPMode = cfg.XDPMode

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
	logger.Info("attached", "interface", cfg.Interface, "xdp_mode", dp.XDPActive, "vips", len(cfg.VIPs), "nat_egress", natObj != "")
	if dp.XDPFallback != nil {
		logger.Warn("native XDP is unavailable on this interface; fell back to generic mode (much slower). Use a driver with native XDP support, or set xdpMode: generic to silence this",
			"interface", cfg.Interface, "err", dp.XDPFallback)
	}
	if dp.Persist {
		// "swapped" are links whose running program was replaced in place
		// (no detach, no gap); anything else was attached fresh this start.
		logger.Info("datapath persistence on: links are pinned and will keep forwarding if rivorad exits; run rivorad -detach to remove them",
			"hot_swapped", dp.Swapped)
		if orphans, err := dp.OrphanedLinks(); err != nil {
			logger.Warn("could not list persisted links", "err", err)
		} else if len(orphans) > 0 {
			logger.Warn("persisted links from an earlier run are still attached but not managed by this config (interface or NAT mode changed?); they keep forwarding until removed with rivorad -detach", "links", orphans)
		}
	}

	plane := dataplane.New(cfg, dp)
	if err := plane.Apply(iface); err != nil {
		logger.Error("apply config", "err", err)
		os.Exit(1)
	}
	logStartup(logger, plane.Startup())
	if config.HasTunnelVIP(cfg.VIPs) {
		v4, v6 := plane.TunnelSources()
		logger.Info("L3 DSR tunnel sources (outer header source addresses)", "ipv4", ipString(v4), "ipv6", ipString(v6))
	}

	// bgpSpeaker is not Kubernetes-specific (unlike the ARP speaker, which
	// needs a K8s Lease for its cluster-wide mutual exclusion) — a
	// standalone static-YAML deployment across multiple nodes can equally
	// want BGP+ECMP HA, so it's built here, before the -kubernetes branch,
	// from whichever mode populated cfg.BGP (the YAML file's bgp: section,
	// or the -bgp* flags above).
	var bgpSpeaker *bgp.Speaker
	var bgpStartupPeers []config.BGPPeer // the peers the speaker was started with, which BGPPeer resources add to
	if cfg.BGP.Enabled {
		var err error
		node := *nodeName
		if node == "" {
			node, _ = os.Hostname()
		}
		bgpCfg := cfg.BGP.ForNode(node)
		bgpStartupPeers = bgpCfg.Peers
		if len(bgpCfg.Peers) == 0 {
			logger.Warn("no configured BGP peer applies to this node, so it will advertise nothing", "node", node)
		} else if len(bgpCfg.Peers) != len(cfg.BGP.Peers) {
			logger.Info("some BGP peers are limited to other nodes and are not used here", "node", node, "used", len(bgpCfg.Peers), "configured", len(cfg.BGP.Peers))
		}
		bgpSpeaker, err = bgp.New(bgpCfg, plane, logger)
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
	refreshTargets := func() {
		var targets []healthcheck.Target
		for _, t := range plane.Targets() {
			targets = append(targets, healthcheck.Target{BackendID: t.ID, Address: t.Address, Port: t.Port, Probe: t.Probe})
		}
		checker.SetTargets(targets)
	}
	refreshTargets()
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

	if !*kubeMode {
		// The startup config is the baseline for "needs a restart" warnings:
		// only the VIP set is ever re-applied, so those settings stay as they
		// were until a restart, however many times the file is reloaded.
		running, natLoaded := cfg, natObj != ""
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-hup:
					reloadStaticConfig(logger, *configPath, running, natLoaded, plane, func() {
						refreshTargets()
						if bgpSpeaker != nil {
							bgpSpeaker.AnnounceNow()
						}
					})
				}
			}
		}()
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
		localPolicy := localPolicyFor(*bgpOn, *speakerOn, *nodeName)
		reconciler.SetLocalPolicy(localPolicy)
		if localPolicy.Node != "" {
			logger.Info("externalTrafficPolicy: Local is honoured: Services that ask for it use only this node's endpoints", "node", localPolicy.Node)
		} else {
			logger.Info("externalTrafficPolicy: Local is not honoured on this node; such Services are treated as Cluster (each is warned about once)", "why", localPolicy.NotHonouredReason)
		}

		if *servicePolicyOn {
			// The chart turns this on by default, but Helm installs CRDs only on first install: after an
			// upgrade from before ServicePolicy existed the CRD may be missing, and an informer on it
			// would never sync and stall every Service. So check, and carry on without policies.
			if perr := k8s.CheckResource(ctx, clients.Dynamic, v1alpha1.ServicePolicyResource); perr != nil {
				logger.Error("ServicePolicy is disabled: the CRD is not installed or not readable; apply deploy/helm/rivora/crds/servicepolicy-crd.yaml and restart", "err", perr)
			} else {
				reconciler.EnableServicePolicies(clients.Dynamic)
				logger.Info("ServicePolicy objects are honoured")
			}
		}

		if *bgpPeerRes {
			switch {
			case bgpSpeaker == nil:
				logger.Error("-bgp-peer-resources needs BGP: set -bgp or -bgp-config; BGPPeer resources are ignored")
			default:
				// Like ServicePolicy: Helm does not upgrade CRDs, so probe rather than let an informer
				// on a missing resource wait forever.
				if perr := k8s.CheckResource(ctx, clients.Dynamic, v1alpha1.BGPPeerResource); perr != nil {
					logger.Error("BGPPeer resources are ignored: the CRD is not installed or not readable; apply deploy/helm/rivora/crds/bgppeer-crd.yaml and restart", "err", perr)
				} else {
					peers := bgppeers.New(clients.Dynamic, clients.Clientset, bgpSpeaker, bgpStartupPeers, cfg.BGP.ASN, *nodeName, *namespace, logger)
					go func() {
						if err := peers.Run(ctx); err != nil {
							logger.Error("BGPPeer controller exited", "err", err)
						}
					}()
				}
			}
		}

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
				targets = append(targets, healthcheck.Target{BackendID: t.ID, Address: t.Address, Port: t.Port, Probe: t.Probe})
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

		// VIPs recovered from the pinned maps stay programmed until the reconcilers have each
		// made a full first pass; whatever none of them claimed is then removed.
		firstPass := []*initsync.Tracker{initsync.New()}
		reconciler.SetInitTracker(firstPass[0])
		if gwReconciler != nil {
			firstPass = append(firstPass, initsync.New())
			gwReconciler.SetInitTracker(firstPass[1])
		}
		if plane.Unclaimed() > 0 {
			go func() {
				if err := initsync.WaitAll(ctx, firstReconcileTimeout, firstPass...); err != nil {
					if ctx.Err() == nil {
						logger.Error("not removing the VIPs recovered at start-up that nothing has claimed: the first reconcile did not finish; they stay programmed", "unclaimed", plane.Unclaimed(), "err", err)
					}
					return
				}
				if pruned := plane.PruneUnclaimed(); len(pruned) > 0 {
					logger.Warn("removed VIPs left programmed by an earlier run that no Service or Gateway wants any more", "vips", pruned)
					onChange()
				}
			}()
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

	tlsCert := os.Getenv("RIVORA_TLS_CERT")
	tlsKey := os.Getenv("RIVORA_TLS_KEY")
	selfSigned := os.Getenv("RIVORA_TLS_SELF_SIGNED") != ""

	apiServer := api.New(plane, apiKey)
	apiServer.SetReadOnlyKeys(readOnlyKey)
	apiServer.SetLogger(logger)

	// Client certificates (mutual TLS) as an alternative to bearer keys: a certificate signed by
	// RIVORA_TLS_CLIENT_CA authenticates its holder, by common name. Listed common names are admins; any
	// other verified certificate is read-only.
	var clientTLS *tls.Config
	if clientCAPath := os.Getenv("RIVORA_TLS_CLIENT_CA"); clientCAPath != "" {
		if tlsCert == "" && !selfSigned {
			logger.Error("RIVORA_TLS_CLIENT_CA needs TLS: set RIVORA_TLS_CERT and RIVORA_TLS_KEY (or RIVORA_TLS_SELF_SIGNED)")
			os.Exit(1)
		}
		pemBytes, rerr := os.ReadFile(clientCAPath)
		if rerr != nil {
			logger.Error("read RIVORA_TLS_CLIENT_CA", "err", rerr)
			os.Exit(1)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			logger.Error("RIVORA_TLS_CLIENT_CA holds no PEM certificate", "path", clientCAPath)
			os.Exit(1)
		}
		clientAuth := tls.VerifyClientCertIfGiven
		if os.Getenv("RIVORA_TLS_CLIENT_REQUIRED") != "" {
			clientAuth = tls.RequireAndVerifyClientCert
		}
		clientTLS = &tls.Config{ClientCAs: pool, ClientAuth: clientAuth, MinVersion: tls.VersionTLS12}
		var admins []string
		if v := os.Getenv("RIVORA_API_CERT_ADMIN_CNS"); v != "" {
			admins = strings.Split(v, ",")
		}
		apiServer.SetClientCerts(admins)
		logger.Info("client certificates accepted", "required", clientAuth == tls.RequireAndVerifyClientCert, "admin_cns", len(admins))
	}
	if bgpSpeaker != nil {
		apiServer.RegisterBGP(bgpSpeaker)
	}

	// metricsSrv is deliberately separate from the (loopback-only, optionally
	// TLS'd/authenticated) API server above: rivorad runs hostNetwork, so
	// 127.0.0.1 is unreachable to anything but the local kubelet, and an
	// in-cluster Prometheus needs a routable, plain-HTTP address instead.
	// /healthz, /readyz and /metrics carry no sensitive data, so no auth here.
	metricsSrv := &http.Server{Addr: *metricsListen, Handler: apiServer.MetricsHandler(), ReadHeaderTimeout: readHeaderTimeout}
	go func() {
		if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("metrics server", "err", err)
		}
	}()
	defer metricsSrv.Close()

	srv := &http.Server{Addr: cfg.APIListen, Handler: apiServer.Handler(), ReadHeaderTimeout: readHeaderTimeout}
	tlsMode := "off"
	switch {
	case tlsCert != "" && tlsKey != "":
		tlsMode = "file"
	case selfSigned:
		tlsMode = "self-signed"
	}
	authMode := "off"
	if apiKey != "" || clientTLS != nil {
		authMode = "on"
	}
	logger.Info("api listening", "addr", cfg.APIListen, "tls", tlsMode, "auth", authMode,
		"admin_keys", api.CountKeys(apiKey), "readonly_keys", api.CountKeys(readOnlyKey))
	logger.Info("metrics listening", "addr", *metricsListen)

	go func() {
		var err error
		switch tlsMode {
		case "file":
			srv.TLSConfig = clientTLS // nil unless client certificates are on; the key pair is loaded from the files
			err = srv.ListenAndServeTLS(tlsCert, tlsKey)
		case "self-signed":
			cert, cerr := tlsutil.GenerateSelfSigned()
			if cerr != nil {
				logger.Error("generate self-signed cert", "err", cerr)
				os.Exit(1)
			}
			srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
			if clientTLS != nil {
				srv.TLSConfig.ClientCAs, srv.TLSConfig.ClientAuth, srv.TLSConfig.MinVersion = clientTLS.ClientCAs, clientTLS.ClientAuth, clientTLS.MinVersion
			}
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
	// Let in-flight API requests finish, but never hold up exit (and the BPF
	// detach in the deferred dp.Close) for longer than shutdownTimeout.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("api graceful shutdown incomplete, closing", "err", err)
		_ = srv.Close()
	}
}

// localPolicyFor decides whether externalTrafficPolicy: Local is honoured on this
// node. It is only safe where a node that has none of a Service's pods won't attract
// its traffic. With BGP that holds: a node advertises a VIP only while it has a
// healthy local backend, so one with none withdraws the route. With the L2 speaker it
// does not: a single elected node answers ARP for every VIP whether or not it runs the
// pods, so a Local Service would blackhole whenever the leader has none. Anywhere it
// isn't safe, Local is treated as Cluster (as it always was), which still works because
// the NAT preserves the client address either way; the reason is what an operator
// reads in the once-per-Service warning.
func localPolicyFor(bgpOn, speakerOn bool, node string) controller.LocalPolicy {
	switch {
	case speakerOn:
		why := "the L2 speaker answers ARP/NDP from one elected node, which may not run this Service's pods, so honouring Local could blackhole it"
		if bgpOn {
			why += "; BGP is on too, so run with -speaker=false to honour Local"
		}
		return controller.LocalPolicy{NotHonouredReason: why}
	case !bgpOn:
		return controller.LocalPolicy{NotHonouredReason: "Local needs BGP: without it nothing stops a node with no local endpoints from receiving the traffic"}
	case strings.TrimSpace(node) == "":
		return controller.LocalPolicy{NotHonouredReason: "this node's name is unknown: set -node-name or the NODE_NAME environment variable (the Helm chart sets it from spec.nodeName)"}
	}
	return controller.LocalPolicy{Node: strings.TrimSpace(node)}
}

// logStartup reports what start-up found already programmed in the pinned maps.
// Removing a VIP the config no longer lists is worth a warning of its own: it
// was still forwarding traffic until this moment.
// firstReconcileTimeout bounds how long start-up waits for the reconcilers' first full pass before
// giving up on removing VIPs nothing has claimed. A Service that fails to reconcile would otherwise
// hold that back forever; on timeout nothing is removed, since one of them might still want its VIP.
const firstReconcileTimeout = 2 * time.Minute

func logStartup(logger *slog.Logger, s dataplane.StartupSummary) {
	if s.Adopted == 0 && s.Dropped == 0 {
		return // fresh maps: nothing was there
	}
	logger.Info("recovered existing datapath state from the pinned maps",
		"adopted", s.Adopted, "updated", s.Updated, "unchanged", s.Unchanged, "added", s.Added)
	if s.Removed > 0 {
		logger.Warn("removed VIPs left programmed by a previous config that this config no longer lists", "removed", s.Removed)
	}
	if s.Dropped > 0 {
		logger.Warn("dropped pinned VIP entries that were internally inconsistent", "dropped", s.Dropped)
	}
}

// vipReloader is the part of *dataplane.Dataplane a config reload needs,
// narrowed so the reload rules can be tested without loading BPF maps.
type vipReloader interface {
	ReloadVIPs(desired []config.VIP) (dataplane.ReloadResult, error)
}

type reloadOutcome int

const (
	reloadRejected reloadOutcome = iota // nothing changed; the running config is kept
	reloadApplied                       // every VIP change was applied
	reloadPartial                       // some VIPs failed; the rest were applied
)

// reloadStaticConfig re-reads the static-YAML file and applies its VIP set.
// A rejected reload must leave the running load balancer exactly as it was, so
// everything that can be checked up front is checked before anything is
// touched: the file must load and validate, and it must not introduce a NAT VIP
// when the tc egress program (which un-NATs replies) wasn't loaded at startup.
// Settings only read at startup are reported as ignored, not silently dropped.
// afterApply runs once VIPs have changed (refresh health targets, nudge BGP),
// including after a partial failure, since some VIPs did change.
func reloadStaticConfig(logger *slog.Logger, path string, running config.Config, natLoaded bool, plane vipReloader, afterApply func()) reloadOutcome {
	next, err := config.Load(path)
	if err != nil {
		logger.Error("config reload rejected; keeping the running config", "path", path, "err", err)
		return reloadRejected
	}
	if !natLoaded && config.HasNATVIP(next.VIPs) {
		logger.Error("config reload rejected: it adds a NAT VIP but the tc egress program was not loaded at startup, so replies would not be un-NATed; restart rivorad instead", "path", path)
		return reloadRejected
	}
	if changed := config.RestartRequired(running, next); len(changed) > 0 {
		logger.Warn("config reload: these settings are only read at startup and were NOT applied; restart rivorad to apply them", "settings", changed)
	}

	res, err := plane.ReloadVIPs(next.VIPs)
	afterApply()
	if err != nil {
		logger.Error("config reload partly applied",
			"added", res.Added, "updated", res.Updated, "removed", res.Removed, "unchanged", res.Unchanged, "err", err)
		return reloadPartial
	}
	logger.Info("config reloaded",
		"added", res.Added, "updated", res.Updated, "removed", res.Removed, "unchanged", res.Unchanged)
	return reloadApplied
}

const (
	// readHeaderTimeout bounds how long a client may take to send request
	// headers, closing the slowloris hole on the API and metrics listeners.
	readHeaderTimeout = 10 * time.Second
	// shutdownTimeout bounds the graceful drain of the API server on SIGTERM.
	shutdownTimeout = 5 * time.Second
)

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

// ipString renders an optional address for a log line.
func ipString(ip net.IP) string {
	if ip == nil {
		return "none"
	}
	return ip.String()
}
