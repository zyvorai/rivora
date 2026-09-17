// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package installer

import (
	"fmt"
	"log"

	"helm.sh/helm/v3/pkg/action"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/zyvorai/rivora/internal/k8s"
)

// ClusterOptions are the connection settings every rivora subcommand that
// talks to a cluster shares (--kubeconfig, --context, --namespace).
type ClusterOptions struct {
	Kubeconfig string
	Context    string
	Namespace  string
}

// RESTConfig resolves a *rest.Config honoring an explicit --kubeconfig
// path and --context override — the standard client-go loading rules
// (explicit path, then $KUBECONFIG, then ~/.kube/config), same as any
// kubectl-like CLI. internal/k8s.BuildConfig isn't reused here: it's tuned
// for rivorad/rivora-controller's in-cluster-first, no-context-override
// needs as pods, not a workstation CLI's --context flag.
func (o ClusterOptions) RESTConfig() (*rest.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if o.Kubeconfig != "" {
		loadingRules.ExplicitPath = o.Kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if o.Context != "" {
		overrides.CurrentContext = o.Context
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
}

// Clients builds the client-go clientset/dynamic client pair, reusing
// internal/k8s.New (the same constructor rivorad/rivora-controller use).
func (o ClusterOptions) Clients() (*k8s.Clients, error) {
	cfg, err := o.RESTConfig()
	if err != nil {
		return nil, fmt.Errorf("build kubeconfig: %w", err)
	}
	return k8s.New(cfg)
}

// HelmConfiguration builds a Helm action.Configuration wired to the same
// cluster/namespace, for install/upgrade/uninstall/get actions.
func (o ClusterOptions) HelmConfiguration(logf action.DebugLog) (*action.Configuration, error) {
	getter := genericclioptions.NewConfigFlags(true)
	getter.Namespace = &o.Namespace
	if o.Context != "" {
		getter.Context = &o.Context
	}
	if o.Kubeconfig != "" {
		getter.KubeConfig = &o.Kubeconfig
	}
	cfg := &action.Configuration{}
	if logf == nil {
		logf = func(format string, v ...interface{}) { log.Printf(format, v...) }
	}
	if err := cfg.Init(getter, o.Namespace, "secrets", logf); err != nil {
		return nil, fmt.Errorf("init helm configuration: %w", err)
	}
	return cfg, nil
}
