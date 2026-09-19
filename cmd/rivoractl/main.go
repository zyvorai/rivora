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
	"strconv"
	"text/tabwriter"

	"github.com/zyvorai/rivora/internal/apiclient"
	"github.com/zyvorai/rivora/internal/config"
)

var version = "dev"

const usage = `rivoractl: status | vips | backends [--format json] [--api HOST:PORT|URL]
  status              Overview: VIP, mode, healthy/total backends, packet counters
  vips                VIP configuration and live counters
  backends            Per-backend state (healthy/draining/down) and packet counters
  drain ID            Stop sending NEW flows to backend ID; established flows keep flowing
  undrain ID          Undo an operator drain (does not affect Kubernetes-driven draining)
  weight ID N         Override backend ID's Maglev weight to N (0 clears the override)
  validate [FILE]     Check a static-YAML config offline (default /etc/rivora/config.yaml)
  version             Print rivoractl version

Options:
  --vip KEY           weight: only change this VIP (addr:port:proto, e.g. 10.0.0.1:80:tcp);
                      default is every VIP that uses the backend
  --api-key KEY       Bearer token (default: $RIVORA_API_KEY)
  --ca-file FILE      Verify rivorad's certificate against this PEM (the certificate it was
                      started with via RIVORA_TLS_CERT, or its CA); default: $RIVORA_CA_FILE.
                      Implies https. Preferred over --tls-insecure.
  --cert FILE --key FILE
                      Present this client certificate (PEM), for a rivorad started with
                      RIVORA_TLS_CLIENT_CA; default: $RIVORA_CLIENT_CERT and $RIVORA_CLIENT_KEY.
                      Implies https and replaces --api-key.
  --tls-insecure      Skip certificate verification (rivorad's ephemeral self-signed cert);
                      default: $RIVORA_TLS_INSECURE. Implies https.

Backend IDs come from 'rivoractl backends'. Drain and weight changes are live
and are not persisted: they survive Kubernetes reconciles but not a rivorad
restart.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	apiAddr := ""
	format := ""
	vipKey := ""
	apiKey := os.Getenv("RIVORA_API_KEY")
	tlsInsecure := os.Getenv("RIVORA_TLS_INSECURE") != ""
	insecureFlag := false // --tls-insecure given explicitly, as opposed to via the environment
	caFile := os.Getenv("RIVORA_CA_FILE")
	certFile, keyFile := os.Getenv("RIVORA_CLIENT_CERT"), os.Getenv("RIVORA_CLIENT_KEY")
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
		case "--api-key":
			i++
			if i < len(args) {
				apiKey = args[i]
			}
		case "--vip":
			i++
			if i < len(args) {
				vipKey = args[i]
			}
		case "--tls-insecure":
			tlsInsecure, insecureFlag = true, true
		case "--ca-file":
			i++
			if i < len(args) {
				caFile = args[i]
			}
		case "--cert":
			i++
			if i < len(args) {
				certFile = args[i]
			}
		case "--key":
			i++
			if i < len(args) {
				keyFile = args[i]
			}
		default:
			rest = append(rest, args[i])
		}
	}
	if apiAddr == "" {
		apiAddr = "127.0.0.1:9870"
	}
	opts := apiclient.Options{APIKey: apiKey, TLSInsecure: tlsInsecure}
	if caFile != "" {
		// Saying both on the command line is a contradiction (verify against this CA,
		// and also don't verify). An insecure setting that only came from the
		// environment is not: the CA wins, as it always does in the client.
		if insecureFlag {
			fmt.Fprintln(os.Stderr, "error: --ca-file and --tls-insecure contradict each other; use one")
			os.Exit(1)
		}
		pool, err := apiclient.LoadCAFile(caFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		opts.RootCAs = pool
	}
	if certFile != "" || keyFile != "" {
		if certFile == "" || keyFile == "" {
			fmt.Fprintln(os.Stderr, "error: --cert and --key go together")
			os.Exit(1)
		}
		cert, err := apiclient.LoadClientCert(certFile, keyFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		opts.Certificate = cert
	}
	client := apiclient.New(apiAddr, opts)

	var err error
	switch cmd {
	case "status":
		err = cmdStatus(client, format)
	case "vips":
		err = cmdVIPs(client, format)
	case "backends":
		err = cmdBackends(client, format)
	case "drain":
		err = cmdDrain(client, rest, true)
	case "undrain":
		err = cmdDrain(client, rest, false)
	case "weight":
		err = cmdWeight(client, rest, vipKey)
	case "validate":
		err = cmdValidate(rest)
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
	fmt.Printf("VIP        %s:%s/%s\n", st.VIPAddress, st.PortLabel(), st.Protocol)
	fmt.Printf("mode       %s\n", st.Mode)
	fmt.Printf("interface  %s\n", st.Interface)
	fmt.Printf("backends   %d/%d healthy\n", healthy, len(st.Backends))
	fmt.Printf("traffic    %d packets, %d bytes, %d dropped\n", st.Packets, st.Bytes, st.Dropped)
	fmt.Printf("uptime     since %s\n", st.StartedAt.Format("2006-01-02T15:04:05Z07:00"))
	return nil
}

func cmdVIPs(c *apiclient.Client, format string) error {
	vips, err := c.VIPs()
	if err != nil {
		return err
	}
	if format == "json" {
		return printJSON(vips)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "VIP\tPORT\tPROTO\tMODE\tBACKENDS\tPACKETS")
	for _, st := range vips {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%d\n", st.VIPAddress, st.PortLabel(), st.Protocol, st.Mode, len(st.Backends), st.Packets)
	}
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
	fmt.Fprintln(w, "ID\tADDRESS\tPORT\tWEIGHT\tSTATE\tPACKETS\tBYTES")
	for _, b := range bs {
		state := b.State
		if state == "" { // older rivorad without the state field
			state = "down"
			if b.Healthy {
				state = "healthy"
			}
		}
		if b.AdminDraining {
			state += " (operator)"
		}
		fmt.Fprintf(w, "%d\t%s\t%d\t%d\t%s\t%d\t%d\n", b.ID, b.Address, b.Port, b.Weight, state, b.Packets, b.Bytes)
	}
	return w.Flush()
}

// parseBackendID parses the positional backend ID argument.
func parseBackendID(arg string) (uint32, error) {
	id, err := strconv.ParseUint(arg, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("backend ID must be a non-negative integer (see 'rivoractl backends'), got %q", arg)
	}
	return uint32(id), nil
}

func cmdDrain(c *apiclient.Client, args []string, drain bool) error {
	verb := "undrain"
	if drain {
		verb = "drain"
	}
	if len(args) != 1 {
		return fmt.Errorf("usage: rivoractl %s ID", verb)
	}
	id, err := parseBackendID(args[0])
	if err != nil {
		return err
	}
	if drain {
		err = c.Drain(id)
	} else {
		err = c.Undrain(id)
	}
	if err != nil {
		return err
	}
	if drain {
		fmt.Printf("backend %d draining: no new flows, established flows unaffected\n", id)
	} else {
		fmt.Printf("backend %d undrained\n", id)
	}
	return nil
}

func cmdWeight(c *apiclient.Client, args []string, vip string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: rivoractl weight ID N [--vip addr:port:proto]  (N=0 clears the override)")
	}
	id, err := parseBackendID(args[0])
	if err != nil {
		return err
	}
	w, err := strconv.ParseUint(args[1], 10, 32)
	if err != nil {
		return fmt.Errorf("weight must be a non-negative integer, got %q", args[1])
	}
	n, err := c.SetWeight(id, uint32(w), vip)
	if err != nil {
		return err
	}
	if w == 0 {
		fmt.Printf("backend %d weight override cleared on %d VIP(s)\n", id, n)
	} else {
		fmt.Printf("backend %d weight set to %d on %d VIP(s)\n", id, w, n)
	}
	return nil
}

// cmdValidate checks a static-YAML config without contacting rivorad, using the
// same loader rivorad itself starts with — so "validate passes" means rivorad
// will accept the file (modulo host state like the interface existing).
func cmdValidate(args []string) error {
	path := "/etc/rivora/config.yaml"
	switch len(args) {
	case 0:
	case 1:
		path = args[0]
	default:
		return fmt.Errorf("usage: rivoractl validate [FILE]")
	}
	cfg, err := config.Load(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	backends := 0
	for _, v := range cfg.VIPs {
		backends += len(v.Backends)
	}
	fmt.Printf("%s: ok (%d VIP(s), %d backend(s))\n", path, len(cfg.VIPs), backends)
	return nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
