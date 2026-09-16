// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package doctor

import "testing"

func TestParseKernelVersion(t *testing.T) {
	cases := []struct {
		release      string
		major, minor int
		ok           bool
	}{
		{"6.8.0-139-generic", 6, 8, true},
		{"5.10.0-30-amd64", 5, 10, true},
		{"6.6.0", 6, 6, true},
		{"bogus", 0, 0, false},
	}
	for _, c := range cases {
		major, minor, ok := parseKernelVersion(c.release)
		if ok != c.ok || (ok && (major != c.major || minor != c.minor)) {
			t.Errorf("parseKernelVersion(%q) = (%d, %d, %v), want (%d, %d, %v)",
				c.release, major, minor, ok, c.major, c.minor, c.ok)
		}
	}
}

func TestCheckKernelVersionTCX(t *testing.T) {
	if c := checkKernelVersion("6.8.0-139-generic", false); c.Status != StatusPass {
		t.Errorf("6.8 kernel: got %s, want pass", c.Status)
	}
	if c := checkKernelVersion("5.15.0-1-generic", false); c.Status != StatusWarn {
		t.Errorf("5.15 without --require-tcx: got %s, want warn", c.Status)
	}
	if c := checkKernelVersion("5.15.0-1-generic", true); c.Status != StatusFail {
		t.Errorf("5.15 with --require-tcx: got %s, want fail", c.Status)
	}
}
