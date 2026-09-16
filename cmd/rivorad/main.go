// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// rivorad is Rivora's single-node daemon: loads/pins the XDP ingress (and,
// in NAT mode, TCX egress) BPF programs, applies the static YAML config to
// the maps, runs active health checks, and serves the local HTTP API.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/zyvorai/rivora/internal/api"
	"github.com/zyvorai/rivora/internal/bpfmaps"
	"github.com/zyvorai/rivora/internal/config"
	"github.com/zyvorai/rivora/internal/dataplane"
	"github.com/zyvorai/rivora/internal/healthcheck"
	"github.com/zyvorai/rivora/internal/loader"
)

var version = "dev"

func main() {
	var (
		configPath = flag.String("config", "/etc/rivora/config.yaml", "path to VIP/backend config")
		bpfDir     = flag.String("bpf-dir", "/usr/local/share/rivora/bpf", "directory containing xdp_ingress.o and tc_nat.o")
		showVer    = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("rivorad", version)
		return
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("load config", "err", err)
		os.Exit(1)
	}

	iface, err := net.InterfaceByName(cfg.Interface)
	if err != nil {
		logger.Error("interface lookup", "interface", cfg.Interface, "err", err)
		os.Exit(1)
	}

	natObj := ""
	if cfg.VIPs[0].Mode == config.ModeNAT {
		natObj = bpfDirPath(*bpfDir, bpfmaps.ProgTCNATEgress)
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
	logger.Info("attached", "interface", cfg.Interface, "mode", cfg.VIPs[0].Mode)

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

	srv := &http.Server{Addr: cfg.APIListen, Handler: api.New(plane).Handler()}
	go func() {
		logger.Info("api listening", "addr", cfg.APIListen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("api server", "err", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	logger.Info("shutting down")
	_ = srv.Close()
}

func bpfDirPath(dir, prog string) string {
	name := "xdp_ingress.o"
	if prog == bpfmaps.ProgTCNATEgress {
		name = "tc_nat.o"
	}
	return dir + "/" + name
}
