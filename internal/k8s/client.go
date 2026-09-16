// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package k8s is the shared client-go bootstrap used by both
// cmd/rivora-controller and rivorad's in-process reconciler/speaker: build
// a *rest.Config (in-cluster, falling back to a kubeconfig file for
// out-of-cluster development/testing) and the typed + dynamic clients built
// from it.
package k8s

import (
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Clients bundles the typed clientset (for built-in types like Service and
// EndpointSlice, which client-go already generates) and the dynamic client
// (for AddressPool — a single small CRD doesn't need a generated
// clientset).
type Clients struct {
	Config    *rest.Config
	Clientset kubernetes.Interface
	Dynamic   dynamic.Interface
}

// BuildConfig loads an in-cluster config when running as a Pod (the normal
// case for both binaries in production), falling back to kubeconfigPath —
// or $KUBECONFIG, or ~/.kube/config — for local development and the
// selftest scripts. An explicit kubeconfigPath always wins.
func BuildConfig(kubeconfigPath string) (*rest.Config, error) {
	if kubeconfigPath == "" {
		if cfg, err := rest.InClusterConfig(); err == nil {
			return cfg, nil
		}
		kubeconfigPath = os.Getenv("KUBECONFIG")
		if kubeconfigPath == "" {
			if home, err := os.UserHomeDir(); err == nil {
				kubeconfigPath = filepath.Join(home, ".kube", "config")
			}
		}
	}
	if kubeconfigPath == "" {
		return nil, fmt.Errorf("no in-cluster config and no kubeconfig found (set --kubeconfig or $KUBECONFIG)")
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfigPath)
}

// New builds both clients from a config produced by BuildConfig.
func New(cfg *rest.Config) (*Clients, error) {
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build clientset: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build dynamic client: %w", err)
	}
	return &Clients{Config: cfg, Clientset: clientset, Dynamic: dyn}, nil
}
