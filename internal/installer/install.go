// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package installer

import (
	"fmt"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/release"
)

// InstallOptions configures Install.
type InstallOptions struct {
	Cluster         ClusterOptions
	ReleaseName     string
	CreateNamespace bool
	Values          map[string]interface{}
}

// Install runs `helm install` (as a library call) with the embedded chart.
func Install(opts InstallOptions) (*release.Release, error) {
	if err := RequireInterface(opts.Values); err != nil {
		return nil, err
	}
	chrt, err := LoadChart()
	if err != nil {
		return nil, fmt.Errorf("load embedded chart: %w", err)
	}
	cfg, err := opts.Cluster.HelmConfiguration(nil)
	if err != nil {
		return nil, err
	}
	client := action.NewInstall(cfg)
	client.ReleaseName = opts.ReleaseName
	client.Namespace = opts.Cluster.Namespace
	client.CreateNamespace = opts.CreateNamespace
	return client.Run(chrt, opts.Values)
}

// UpgradeOptions configures Upgrade.
type UpgradeOptions struct {
	Cluster     ClusterOptions
	ReleaseName string
	Values      map[string]interface{}
}

// Upgrade runs `helm upgrade` (as a library call) with the embedded chart.
func Upgrade(opts UpgradeOptions) (*release.Release, error) {
	if err := RequireInterface(opts.Values); err != nil {
		return nil, err
	}
	chrt, err := LoadChart()
	if err != nil {
		return nil, fmt.Errorf("load embedded chart: %w", err)
	}
	cfg, err := opts.Cluster.HelmConfiguration(nil)
	if err != nil {
		return nil, err
	}
	client := action.NewUpgrade(cfg)
	client.Namespace = opts.Cluster.Namespace
	return client.Run(opts.ReleaseName, chrt, opts.Values)
}

// UninstallOptions configures Uninstall.
type UninstallOptions struct {
	Cluster     ClusterOptions
	ReleaseName string
}

// crdAdvisory is printed after every uninstall — the AddressPool CRD is
// deliberately not Helm-managed (see deploy/helm/rivora/README.md's "The
// CRD is not managed by upgrade/uninstall"), so `helm uninstall`'s own
// silence about it would otherwise look like an oversight rather than
// documented, intentional behavior.
const crdAdvisory = `
AddressPool objects and the AddressPool CRD were left in place — this
matches Helm's documented CRD convention (see
deploy/helm/rivora/README.md#the-crd-is-not-managed-by-upgradeuninstall).
To remove them too:
  kubectl delete addresspools.rivora.zyvor.dev --all
  kubectl delete -f deploy/helm/rivora/crds/addresspool-crd.yaml`

// Uninstall runs `helm uninstall` and returns the CRD-retention advisory
// message alongside Helm's own response.
func Uninstall(opts UninstallOptions) (*release.UninstallReleaseResponse, string, error) {
	cfg, err := opts.Cluster.HelmConfiguration(nil)
	if err != nil {
		return nil, "", err
	}
	client := action.NewUninstall(cfg)
	resp, err := client.Run(opts.ReleaseName)
	if err != nil {
		return nil, "", err
	}
	return resp, crdAdvisory, nil
}

// GetRelease returns the currently installed release (chart/app version,
// values-in-use) — the basis for `rivora status`'s Helm-side reporting.
func GetRelease(cluster ClusterOptions, releaseName string) (*release.Release, error) {
	cfg, err := cluster.HelmConfiguration(nil)
	if err != nil {
		return nil, err
	}
	return action.NewGet(cfg).Run(releaseName)
}
