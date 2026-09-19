// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// rivora-controller is v0.2's cluster-scoped IPAM controller: it assigns
// addresses from AddressPool objects to Service(type=LoadBalancer) objects
// and patches their Status.LoadBalancer.Ingress[].IP. It does no BPF/
// dataplane work at all — that's rivorad's per-node reconciler
// (internal/controller) — so it runs unprivileged, and as a single
// cluster-wide writer to Service.Status it needs exactly one active
// replica, arbitrated by a coordination.k8s.io/v1 Lease.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/zyvorai/rivora/internal/ipamctrl"
	"github.com/zyvorai/rivora/internal/k8s"
	"github.com/zyvorai/rivora/internal/logging"
)

var version = "dev"

func main() {
	var (
		kubeconfig    = flag.String("kubeconfig", "", "path to a kubeconfig file (default: in-cluster config, falling back to $KUBECONFIG / ~/.kube/config)")
		namespace     = flag.String("namespace", envOr("POD_NAMESPACE", "rivora-system"), "namespace the leader-election Lease lives in")
		lbClass       = flag.String("loadbalancer-class", "", "only manage Services whose spec.loadBalancerClass matches this value (default: services with no class set)")
		workers       = flag.Int("workers", 2, "number of concurrent Service reconcile workers")
		gatewayAPI    = flag.Bool("gateway-api", false, "also watch GatewayClass/Gateway and assign addresses to managed Gateways; requires the Gateway API CRDs to be installed")
		metricsListen = flag.String("metrics-listen", ":9871", "address the /metrics, /healthz, /readyz endpoints listen on")
		showVer       = flag.Bool("version", false, "print version and exit")
		logLevel      = flag.String("log-level", "info", "log level: debug, info, warn or error")
		logFormat     = flag.String("log-format", "text", "log format: text or json")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("rivora-controller", version)
		return
	}

	logger, err := logging.New(os.Stdout, *logLevel, *logFormat)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rivora-controller:", err)
		os.Exit(2)
	}

	cfg, err := k8s.BuildConfig(*kubeconfig)
	if err != nil {
		logger.Error("build kubeconfig", "err", err)
		os.Exit(1)
	}
	clients, err := k8s.New(cfg)
	if err != nil {
		logger.Error("build clients", "err", err)
		os.Exit(1)
	}

	identity, err := os.Hostname()
	if err != nil || identity == "" {
		identity = fmt.Sprintf("rivora-controller-%d", os.Getpid())
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	reconciler, factory, dynFactory := ipamctrl.New(clients.Clientset, clients.Dynamic, *lbClass, *gatewayAPI, logger)

	leaderGauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "rivora", Subsystem: "controller", Name: "leader",
		Help: "Whether this replica currently holds the leader-election Lease (1) or not (0).",
	})
	registry := prometheus.NewRegistry()
	registry.MustRegister(leaderGauge, collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	metricsMux := http.NewServeMux()
	metricsMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	metricsMux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	metricsMux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	metricsSrv := &http.Server{Addr: *metricsListen, Handler: metricsMux}
	go func() {
		if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("metrics server", "err", err)
		}
	}()
	defer metricsSrv.Close()

	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{Name: "rivora-controller", Namespace: *namespace},
		Client:    clients.Clientset.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: identity,
		},
	}

	elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   15 * time.Second,
		RenewDeadline:   10 * time.Second,
		RetryPeriod:     2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				logger.Info("acquired leadership", "identity", identity)
				leaderGauge.Set(1)
				if err := reconciler.Run(ctx, factory, dynFactory, *workers); err != nil {
					logger.Error("reconciler exited", "err", err)
				}
			},
			OnStoppedLeading: func() {
				logger.Info("lost leadership", "identity", identity)
				leaderGauge.Set(0)
			},
			OnNewLeader: func(newIdentity string) {
				if newIdentity != identity {
					logger.Info("observed new leader", "leader", newIdentity)
				}
			},
		},
	})
	if err != nil {
		logger.Error("build leader elector", "err", err)
		os.Exit(1)
	}

	logger.Info("rivora-controller starting", "identity", identity, "namespace", *namespace, "lb_class", *lbClass, "gateway_api", *gatewayAPI)
	elector.Run(ctx)
	logger.Info("shut down")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
