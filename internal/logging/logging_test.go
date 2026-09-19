// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package logging

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestLevelFiltering(t *testing.T) {
	cases := []struct {
		level     string
		wantDebug bool
		wantInfo  bool
		wantWarn  bool
	}{
		{"", false, true, true}, // default is info
		{"info", false, true, true},
		{"DEBUG", true, true, true},
		{"warn", false, false, true},
		{"warning", false, false, true},
		{"error", false, false, false},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		l, err := New(&buf, c.level, "text")
		if err != nil {
			t.Fatalf("level %q: %v", c.level, err)
		}
		l.Debug("dbg")
		l.Info("inf")
		l.Warn("wrn")
		out := buf.String()
		for msg, want := range map[string]bool{"dbg": c.wantDebug, "inf": c.wantInfo, "wrn": c.wantWarn} {
			if got := strings.Contains(out, msg); got != want {
				t.Errorf("level %q: %q emitted=%v, want %v", c.level, msg, got, want)
			}
		}
	}
}

func TestJSONFormat(t *testing.T) {
	var buf bytes.Buffer
	l, err := New(&buf, "info", "json")
	if err != nil {
		t.Fatal(err)
	}
	l.Info("hello", "addr", "10.0.0.1")
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("output is not one JSON object: %v: %q", err, buf.String())
	}
	if rec["msg"] != "hello" || rec["addr"] != "10.0.0.1" {
		t.Errorf("record = %v", rec)
	}
}

func TestInvalidValuesAreRejected(t *testing.T) {
	// A typo'd flag must fail loudly at startup, not silently fall back to info.
	if _, err := New(&bytes.Buffer{}, "verbose", "text"); err == nil {
		t.Error("invalid level accepted")
	}
	if _, err := New(&bytes.Buffer{}, "info", "yaml"); err == nil {
		t.Error("invalid format accepted")
	}
}
