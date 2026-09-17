// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// rivora is Rivora's cluster-lifecycle CLI: install/upgrade/uninstall/status
// against a live Kubernetes cluster, driving the embedded copy of
// deploy/helm/rivora via the Helm SDK (see internal/installer) — the same
// role Cilium's `cilium` CLI plays for Cilium. It's deliberately separate
// from rivoractl (a per-node dataplane API client, talks to rivorad's
// loopback API) and rivora-doctor (pre-install host readiness checks) —
// three tools, three scopes, no overlap.
package main

import (
	"fmt"
	"os"
)

var version = "dev"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
