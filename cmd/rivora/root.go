// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/zyvorai/rivora/internal/installer"
)

var cluster installer.ClusterOptions
var releaseName string

// Command groups shown as separate sections in --help output (kubectl/
// cilium-style), via coloredUsageFunc in help.go.
const (
	groupLifecycle = "lifecycle"
	groupInfo      = "info"
)

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "rivora",
		Short:         "Install and manage Rivora on a Kubernetes cluster",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&cluster.Kubeconfig, "kubeconfig", "", "path to a kubeconfig file (default: $KUBECONFIG, then ~/.kube/config)")
	root.PersistentFlags().StringVar(&cluster.Context, "context", "", "kubeconfig context to use (default: current context)")
	root.PersistentFlags().StringVar(&cluster.Namespace, "namespace", "rivora-system", "namespace Rivora is (or will be) installed into")
	root.PersistentFlags().StringVar(&releaseName, "release-name", "rivora", "Helm release name")

	root.AddGroup(
		&cobra.Group{ID: groupLifecycle, Title: "Lifecycle Commands:"},
		&cobra.Group{ID: groupInfo, Title: "Info Commands:"},
	)
	root.SetUsageFunc(coloredUsageFunc)

	root.AddCommand(newInstallCmd())
	root.AddCommand(newUpgradeCmd())
	root.AddCommand(newUninstallCmd())
	root.AddCommand(newStatusCmd())
	root.AddCommand(newVersionCmd())
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "version",
		Short:   "Print rivora's version",
		GroupID: groupInfo,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("rivora", version)
			return nil
		},
	}
}
