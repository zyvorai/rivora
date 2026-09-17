// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestApplyColor(t *testing.T) {
	if got := applyColor(true, ansiGreen, "install"); got != ansiGreen+"install"+ansiReset {
		t.Errorf("applyColor(true, ...) = %q, want wrapped in ANSI codes", got)
	}
	if got := applyColor(false, ansiGreen, "install"); got != "install" {
		t.Errorf("applyColor(false, ...) = %q, want plain text unchanged", got)
	}
	if got := applyColor(true, ansiGreen, ""); got != "" {
		t.Errorf("applyColor on an empty string should stay empty, got %q", got)
	}
}

func TestSupportsColorRespectsNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if supportsColor() {
		t.Error("NO_COLOR=1 should disable color regardless of terminal state")
	}
}

func TestSupportsColorFalseUnderGoTest(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	// go test's stdout is never a real terminal, so this should
	// deterministically report false — the same reason colorEnabled is
	// computed once at startup rather than re-checked per call.
	if supportsColor() {
		t.Error("supportsColor() should be false when stdout isn't a character device (as under go test)")
	}
}

func TestRpad(t *testing.T) {
	if got := rpad("install", 10); got != "install   " {
		t.Errorf("rpad(%q, 10) = %q (len %d), want length 10", "install", got, len(got))
	}
	if got := rpad("uninstall", 9); len(got) != 9 {
		t.Errorf("rpad should not truncate a string already at its padding width: got %q", got)
	}
}

func TestColoredUsageFuncGroupsCommands(t *testing.T) {
	root := &cobra.Command{Use: "rivora"}
	root.AddGroup(
		&cobra.Group{ID: groupLifecycle, Title: "Lifecycle Commands:"},
		&cobra.Group{ID: groupInfo, Title: "Info Commands:"},
	)
	root.SetUsageFunc(coloredUsageFunc)
	root.AddCommand(&cobra.Command{Use: "install", Short: "Install it", GroupID: groupLifecycle, Run: func(*cobra.Command, []string) {}})
	root.AddCommand(&cobra.Command{Use: "status", Short: "Show status", GroupID: groupInfo, Run: func(*cobra.Command, []string) {}})

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	if err := root.Usage(); err != nil {
		t.Fatalf("Usage(): %v", err)
	}
	out := buf.String()

	for _, want := range []string{"Lifecycle Commands:", "install", "Info Commands:", "status"} {
		if !strings.Contains(out, want) {
			t.Errorf("usage output missing %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "Lifecycle Commands:") > strings.Index(out, "install") {
		t.Error("group title should print before its commands")
	}
	// colorEnabled is false under go test (stdout isn't a terminal), so the
	// output should be plain text, not raw escape codes.
	if strings.Contains(out, ansiBoldCyan) {
		t.Error("usage output should not contain ANSI codes when not running on a terminal")
	}
}
