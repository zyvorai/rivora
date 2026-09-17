// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zyvorai/rivora/internal/installer"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the cluster-wide status of a Rivora installation",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := installer.GetClusterStatus(cluster, releaseName)
			if err != nil {
				return fmt.Errorf("status: %w", err)
			}
			printClusterStatus(st)
			return nil
		},
	}
}

func printClusterStatus(st *installer.ClusterStatus) {
	fmt.Printf("release      %s", st.ReleaseName)
	if st.ChartVersion != "" {
		fmt.Printf(" (chart %s", st.ChartVersion)
		if st.AppVersion != "" {
			fmt.Printf(", app %s", st.AppVersion)
		}
		fmt.Print(")")
	}
	fmt.Println()
	printWorkload("rivorad (DaemonSet)", st.Rivorad)
	printWorkload("rivora-controller (Deployment)", st.Controller)
	if st.AddressPoolErr != nil {
		fmt.Printf("AddressPool CRD  unreachable: %v\n", st.AddressPoolErr)
	} else {
		fmt.Println("AddressPool CRD  installed")
	}
}

func printWorkload(label string, w installer.WorkloadStatus) {
	verdict := "healthy"
	if !w.Healthy() {
		verdict = "degraded"
	}
	fmt.Printf("%-32s %d/%d ready, %d available — %s\n", label, w.Ready, w.Desired, w.Available, verdict)
	if len(w.Degraded) > 0 {
		fmt.Printf("  %s\n", strings.Join(w.Degraded, "; "))
	}
}
