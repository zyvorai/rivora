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
	"syscall"
	"time"

	"github.com/zyvorai/rivora/internal/api"
	"github.com/zyvorai/rivora/internal/bpfmaps"
	"github.com/zyvorai/rivora/internal/config"
	"github.com/zyvorai/rivora/internal/controller"
	"github.com/zyvorai/rivora/internal/dataplane"
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

		kubeMode   = flag.Bool("kubernetes", false, "run the Kubernetes reconciler + ARP speaker instead of loading -config; VIPs come from Service/EndpointSlice")
		kubeconfig = flag.String("kubeconfig", "", "path to a kubeconfig file (default: in-cluster config, falling back to $KUBECONFIG / ~/.kube/config); only used with -kubernetes")
		ifaceFlag  = flag.String("interface", "", "network interface to attach to (required with -kubernetes; static-YAML mode reads this from -config instead)")
		apiListen  = flag.String("api-listen", "127.0.0.1:9870", "local API listen address; only used with -kubernetes (static-YAML mode reads this from -config instead)")
		lbClass    = flag.String("loadbalancer-class", "", "only manage Services whose spec.loadBalancerClass matches this value (default: services with no class set); only used with -kubernetes")
		namespace  = flag.String("namespace", envOr("POD_NAMESPACE", "rivora-system"), "namespace the ARP speaker's leader-election Lease lives in; only used with -kubernetes")
		workers    = flag.Int("workers", 2, "number of concurrent Service reconcile workers; only used with -kubernetes")
		speakerOn  = flag.Bool("speaker", true, "run the L2/ARP speaker (requires CAP_NET_RAW); only used with -kubernetes")

		healthInterval = flag.Duration("health-interval", 3*time.Second, "active health check interval; only used with -kubernetes")
		healthTimeout  = flag.Duration("health-timeout", time.Second, "active health check timeout; only used with -kubernetes")
		healthFail     = flag.Int("health-fail-threshold", 2, "consecutive failures before marking a backend down; only used with -kubernetes")
		healthSuccess  = flag.Int("health-success-threshold", 2, "consecutive successes before marking a backend healthy; only used with -kubernetes")
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

	checker := healthcheck.New(
		cfg.HealthCheck.Interval, cfg.HealthCheck.Timeout,
		cfg.HealthCheck.FailThreshold, cfg.HealthCheck.SuccessThreshold,
		func(backendID uint32, healthy bool) {
			logger.Info("backend health changed", "backend", backendID, "healthy", healthy)
			if err := plane.SetBackendHealth(backendID, healthy); err != nil {
				logger.Error("update backend health", "backend", backendID, "err", err)
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

		reconciler.OnChange = func() {
			var targets []healthcheck.Target
			for _, t := range plane.Targets() {
				targets = append(targets, healthcheck.Target{BackendID: t.ID, Address: t.Address, Port: t.Port})
			}
			checker.SetTargets(targets)
			if sp != nil {
				sp.AnnounceNow()
			}
		}

		go func() {
			if err := reconciler.Run(ctx, factory, *workers); err != nil {
				logger.Error("k8s reconciler exited", "err", err)
			}
		}()
		if sp != nil {
			go func() {
				if err := sp.Run(ctx); err != nil {
					logger.Error("arp speaker exited", "err", err)
				}
			}()
		}
		logger.Info("kubernetes mode enabled", "lb_class", *lbClass, "speaker", *speakerOn)
	}

	apiKey := os.Getenv("RIVORA_API_KEY")
	tlsCert := os.Getenv("RIVORA_TLS_CERT")
	tlsKey := os.Getenv("RIVORA_TLS_KEY")
	selfSigned := os.Getenv("RIVORA_TLS_SELF_SIGNED") != ""

	srv := &http.Server{Addr: cfg.APIListen, Handler: api.New(plane, apiKey).Handler()}
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
