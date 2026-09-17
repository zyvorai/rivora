// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/zyvorai/rivora/internal/installer"
)

// valuesFlags is shared by install and upgrade — same -f/--set surface,
// same precedence (files in order, then --set in order, later wins),
// matching `helm install`/`helm upgrade`.
type valuesFlags struct {
	files []string
	sets  []string
}

func (f *valuesFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringArrayVarP(&f.files, "values", "f", nil, "values file(s) to apply, in order (repeatable)")
	cmd.Flags().StringArrayVar(&f.sets, "set", nil, "set a value on the command line, e.g. rivorad.interface=eth0 (repeatable, later wins)")
}

func (f *valuesFlags) merge() (map[string]interface{}, error) {
	return installer.MergeValues(f.files, f.sets)
}

func newInstallCmd() *cobra.Command {
	var vf valuesFlags
	var createNamespace bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install Rivora into the cluster",
		Long: "Install Rivora into the cluster using the chart bundled with this CLI release.\n" +
			"rivorad.interface is required — every node the DaemonSet schedules onto must\n" +
			"have a network interface of this name. Example:\n\n" +
			"  rivora install --set rivorad.interface=eth0 \\\n" +
			"    --set addressPools[0].name=default \\\n" +
			"    --set addressPools[0].addresses='{10.0.0.0/24}'",
		RunE: func(cmd *cobra.Command, args []string) error {
			vals, err := vf.merge()
			if err != nil {
				return err
			}
			rel, err := installer.Install(installer.InstallOptions{
				Cluster:         cluster,
				ReleaseName:     releaseName,
				CreateNamespace: createNamespace,
				Values:          vals,
			})
			if err != nil {
				return fmt.Errorf("install: %w", err)
			}
			fmt.Printf("installed %q in namespace %q (chart %s)\n", rel.Name, rel.Namespace, rel.Chart.Metadata.Version)
			fmt.Println("check rollout with: rivora status")
			return nil
		},
	}
	vf.register(cmd)
	cmd.Flags().BoolVar(&createNamespace, "create-namespace", true, "create the target namespace if it doesn't exist")
	return cmd
}

func newUpgradeCmd() *cobra.Command {
	var vf valuesFlags
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade an existing Rivora installation",
		RunE: func(cmd *cobra.Command, args []string) error {
			vals, err := vf.merge()
			if err != nil {
				return err
			}
			rel, err := installer.Upgrade(installer.UpgradeOptions{
				Cluster:     cluster,
				ReleaseName: releaseName,
				Values:      vals,
			})
			if err != nil {
				return fmt.Errorf("upgrade: %w", err)
			}
			fmt.Printf("upgraded %q to chart %s (release revision %d)\n", rel.Name, rel.Chart.Metadata.Version, rel.Version)
			return nil
		},
	}
	vf.register(cmd)
	return cmd
}

func newUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Remove Rivora from the cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, advisory, err := installer.Uninstall(installer.UninstallOptions{
				Cluster:     cluster,
				ReleaseName: releaseName,
			})
			if err != nil {
				return fmt.Errorf("uninstall: %w", err)
			}
			fmt.Printf("uninstalled %q\n", resp.Release.Name)
			fmt.Println(advisory)
			return nil
		},
	}
}
