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
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/zyvorai/rivora/internal/ipamctrl"
	"github.com/zyvorai/rivora/internal/k8s"
)

var version = "dev"

func main() {
	var (
		kubeconfig = flag.String("kubeconfig", "", "path to a kubeconfig file (default: in-cluster config, falling back to $KUBECONFIG / ~/.kube/config)")
		namespace  = flag.String("namespace", envOr("POD_NAMESPACE", "rivora-system"), "namespace the leader-election Lease lives in")
		lbClass    = flag.String("loadbalancer-class", "", "only manage Services whose spec.loadBalancerClass matches this value (default: services with no class set)")
		workers    = flag.Int("workers", 2, "number of concurrent Service reconcile workers")
		gatewayAPI = flag.Bool("gateway-api", false, "also watch GatewayClass/Gateway and assign addresses to managed Gateways; requires the Gateway API CRDs to be installed")
		showVer    = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("rivora-controller", version)
		return
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

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
				if err := reconciler.Run(ctx, factory, dynFactory, *workers); err != nil {
					logger.Error("reconciler exited", "err", err)
				}
			},
			OnStoppedLeading: func() {
				logger.Info("lost leadership", "identity", identity)
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
