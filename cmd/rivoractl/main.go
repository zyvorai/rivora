// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// rivoractl is Rivora's CLI: an HTTP client for rivorad's local API. Plain
// flag parsing and hand-formatted output, no CLI framework — matches
// netractl's style in the sibling netra repo.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/zyvorai/rivora/internal/apiclient"
)

var version = "dev"

const usage = `rivoractl: status | vips | backends [--format json] [--api HOST:PORT]
  status              Overview: VIP, mode, healthy/total backends, packet counters
  vips                VIP configuration and live counters
  backends            Per-backend health and packet counters
  version             Print rivoractl version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	var apiAddr, format string
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--api":
			i++
			if i < len(args) {
				apiAddr = args[i]
			}
		case "--format":
			i++
			if i < len(args) {
				format = args[i]
			}
		default:
			rest = append(rest, args[i])
		}
	}
	if apiAddr == "" {
		apiAddr = "127.0.0.1:9870"
	}
	client := apiclient.New(apiAddr)

	var err error
	switch cmd {
	case "status":
		err = cmdStatus(client, format)
	case "vips":
		err = cmdVIPs(client, format)
	case "backends":
		err = cmdBackends(client, format)
	case "version":
		fmt.Println("rivoractl", version)
		return
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func cmdStatus(c *apiclient.Client, format string) error {
	st, err := c.Status()
	if err != nil {
		return err
	}
	if format == "json" {
		return printJSON(st)
	}
	healthy := 0
	for _, b := range st.Backends {
		if b.Healthy {
			healthy++
		}
	}
	fmt.Printf("VIP        %s:%d/%s\n", st.VIPAddress, st.VIPPort, st.Protocol)
	fmt.Printf("mode       %s\n", st.Mode)
	fmt.Printf("interface  %s\n", st.Interface)
	fmt.Printf("backends   %d/%d healthy\n", healthy, len(st.Backends))
	fmt.Printf("traffic    %d packets, %d bytes, %d dropped\n", st.Packets, st.Bytes, st.Dropped)
	fmt.Printf("uptime     since %s\n", st.StartedAt.Format("2006-01-02T15:04:05Z07:00"))
	return nil
}

func cmdVIPs(c *apiclient.Client, format string) error {
	st, err := c.Status()
	if err != nil {
		return err
	}
	if format == "json" {
		return printJSON(st)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "VIP\tPORT\tPROTO\tMODE\tBACKENDS\tPACKETS")
	fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%d\t%d\n", st.VIPAddress, st.VIPPort, st.Protocol, st.Mode, len(st.Backends), st.Packets)
	return w.Flush()
}

func cmdBackends(c *apiclient.Client, format string) error {
	bs, err := c.Backends()
	if err != nil {
		return err
	}
	if format == "json" {
		return printJSON(bs)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tADDRESS\tPORT\tHEALTHY\tPACKETS\tBYTES")
	for _, b := range bs {
		fmt.Fprintf(w, "%d\t%s\t%d\t%t\t%d\t%d\n", b.ID, b.Address, b.Port, b.Healthy, b.Packets, b.Bytes)
	}
	return w.Flush()
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
