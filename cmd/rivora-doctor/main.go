// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// rivora-doctor checks whether this host is ready to run rivorad. Mirrors
// netra-doctor's shape in the sibling netra repo: Report of Checks plus a
// Summary, human or JSON output, --strict promotes warnings to failures.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/zyvorai/rivora/internal/doctor"
)

func main() {
	var (
		jsonOut    = flag.Bool("json", false, "emit a machine-readable JSON report")
		strict     = flag.Bool("strict", false, "exit non-zero on warnings as well as failures")
		requireTCX = flag.Bool("require-tcx", false, "treat missing Linux 6.6+ TCX baseline as a failure")
		iface      = flag.String("interface", "", "also report the given interface's driver (XDP mode hint)")
		root       = flag.String("root", "/", "filesystem root to inspect (useful for mounted support bundles)")
	)
	flag.Parse()

	r := doctor.Run(doctor.Options{
		Root:       *root,
		RequireTCX: *requireTCX,
		Interface:  *iface,
	})

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(r); err != nil {
			fmt.Fprintln(os.Stderr, "rivora-doctor: encode report:", err)
			os.Exit(1)
		}
	} else {
		printHuman(r)
	}

	if r.Summary.Fail > 0 || (*strict && r.Summary.Warn > 0) {
		os.Exit(2)
	}
}

func printHuman(r doctor.Report) {
	fmt.Printf("Rivora host readiness\n")
	fmt.Printf("host=%s os=%s arch=%s kernel=%s\n\n", empty(r.Hostname, "unknown"), r.OS, r.Architecture, empty(r.KernelRelease, "unknown"))
	for _, c := range r.Checks {
		fmt.Printf("%-5s %-28s %s\n", strings.ToUpper(string(c.Status)), c.Title, c.Detail)
		if c.Remediation != "" && (c.Status == doctor.StatusWarn || c.Status == doctor.StatusFail) {
			fmt.Printf("      -> %s\n", c.Remediation)
		}
	}
	fmt.Printf("\nsummary: pass=%d warn=%d fail=%d info=%d\n", r.Summary.Pass, r.Summary.Warn, r.Summary.Fail, r.Summary.Info)
}

func empty(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
