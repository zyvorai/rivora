// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// Colors only when it's safe to: respects NO_COLOR and only lights up on a
// real terminal — same convention scripts/deploy-remote.sh and
// scripts/demo-traffic.sh already use, so piping `rivora --help` to a file
// or a non-terminal consumer never gets raw escape codes.
var colorEnabled = supportsColor()

func supportsColor() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

const (
	ansiReset    = "\033[0m"
	ansiDim      = "\033[2m"
	ansiBoldCyan = "\033[1;36m"
	ansiGreen    = "\033[32m"
)

func colorize(code, s string) string {
	return applyColor(colorEnabled, code, s)
}

// applyColor is colorize's actual logic, taking enabled explicitly so it's
// testable without depending on the test runner's own terminal state
// (colorEnabled is fixed at process start from the real stdout, which
// `go test` never runs against a TTY).
func applyColor(enabled bool, code, s string) string {
	if !enabled || s == "" {
		return s
	}
	return code + s + ansiReset
}

func rpad(s string, padding int) string {
	return fmt.Sprintf("%-*s", padding, s)
}

// coloredUsageFunc mirrors cobra's own defaultUsageFunc line for line,
// coloring only structural elements (section headers, command/flag
// section names) — Short/Long descriptions stay plain prose. Registered
// once on the root command; cobra's UsageFunc() resolution walks up to a
// parent's func when a subcommand doesn't set its own, so every `rivora
// <cmd> --help` inherits this automatically.
func coloredUsageFunc(c *cobra.Command) error {
	w := c.OutOrStderr()
	fmt.Fprint(w, colorize(ansiBoldCyan, "Usage:"))
	if c.Runnable() {
		fmt.Fprintf(w, "\n  %s", c.UseLine())
	}
	if c.HasAvailableSubCommands() {
		fmt.Fprintf(w, "\n  %s [command]", c.CommandPath())
	}
	if len(c.Aliases) > 0 {
		fmt.Fprintf(w, "\n\n%s\n", colorize(ansiBoldCyan, "Aliases:"))
		fmt.Fprintf(w, "  %s", c.NameAndAliases())
	}
	if c.HasExample() {
		fmt.Fprintf(w, "\n\n%s\n", colorize(ansiBoldCyan, "Examples:"))
		fmt.Fprint(w, c.Example)
	}
	if c.HasAvailableSubCommands() {
		cmds := c.Commands()
		printCmd := func(subcmd *cobra.Command) {
			fmt.Fprintf(w, "\n  %s %s", colorize(ansiGreen, rpad(subcmd.Name(), subcmd.NamePadding())), subcmd.Short)
		}
		if len(c.Groups()) == 0 {
			fmt.Fprintf(w, "\n\n%s", colorize(ansiBoldCyan, "Available Commands:"))
			for _, subcmd := range cmds {
				if subcmd.IsAvailableCommand() || subcmd.Name() == "help" {
					printCmd(subcmd)
				}
			}
		} else {
			for _, group := range c.Groups() {
				fmt.Fprintf(w, "\n\n%s", colorize(ansiBoldCyan, group.Title))
				for _, subcmd := range cmds {
					if subcmd.GroupID == group.ID && (subcmd.IsAvailableCommand() || subcmd.Name() == "help") {
						printCmd(subcmd)
					}
				}
			}
			if !c.AllChildCommandsHaveGroup() {
				fmt.Fprintf(w, "\n\n%s", colorize(ansiBoldCyan, "Additional Commands:"))
				for _, subcmd := range cmds {
					if subcmd.GroupID == "" && (subcmd.IsAvailableCommand() || subcmd.Name() == "help") {
						printCmd(subcmd)
					}
				}
			}
		}
	}
	if c.HasAvailableLocalFlags() {
		fmt.Fprintf(w, "\n\n%s\n", colorize(ansiBoldCyan, "Flags:"))
		fmt.Fprint(w, strings.TrimRight(c.LocalFlags().FlagUsages(), " \t\n"))
	}
	if c.HasAvailableInheritedFlags() {
		fmt.Fprintf(w, "\n\n%s\n", colorize(ansiBoldCyan, "Global Flags:"))
		fmt.Fprint(w, strings.TrimRight(c.InheritedFlags().FlagUsages(), " \t\n"))
	}
	if c.HasHelpSubCommands() {
		fmt.Fprintf(w, "\n\n%s", colorize(ansiBoldCyan, "Additional help topics:"))
		for _, subcmd := range c.Commands() {
			if subcmd.IsAdditionalHelpTopicCommand() {
				fmt.Fprintf(w, "\n  %s %s", rpad(subcmd.CommandPath(), subcmd.CommandPathPadding()), subcmd.Short)
			}
		}
	}
	if c.HasAvailableSubCommands() {
		fmt.Fprintf(w, "\n\n%s", colorize(ansiDim, fmt.Sprintf("Use \"%s [command] --help\" for more information about a command.", c.CommandPath())))
	}
	fmt.Fprintln(w)
	return nil
}
