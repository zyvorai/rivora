// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package doctor

import (
	"strings"
	"testing"
)

func apiSecurityWith(t *testing.T, admin, readOnly string) Check {
	t.Helper()
	t.Setenv("RIVORA_API_KEY", admin)
	t.Setenv("RIVORA_API_READONLY_KEY", readOnly)
	t.Setenv("RIVORA_TLS_CERT", "")
	t.Setenv("RIVORA_TLS_KEY", "")
	t.Setenv("RIVORA_TLS_SELF_SIGNED", "")
	return checkAPISecurity()
}

func TestAPISecurityReportsTheAuthPosture(t *testing.T) {
	c := apiSecurityWith(t, "", "")
	if c.Status != StatusInfo || !strings.Contains(c.Detail, "unauthenticated") {
		t.Errorf("no keys: %+v, want an info that the API is unauthenticated", c)
	}
	c = apiSecurityWith(t, "admin-key", "")
	if c.Status != StatusInfo || !strings.Contains(c.Detail, "requires a bearer token") || strings.Contains(c.Detail, "read-only") {
		t.Errorf("admin key only: %+v", c)
	}
	c = apiSecurityWith(t, "admin-key", "reader-key")
	if c.Status != StatusInfo || !strings.Contains(c.Detail, "READONLY") {
		t.Errorf("admin + read-only: %+v, want the read-only key mentioned", c)
	}
}

func TestAPISecurityWarnsWhenAReadOnlyKeyHasNoAdminKey(t *testing.T) {
	// rivorad refuses to start like this, so the doctor should say so up front.
	c := apiSecurityWith(t, "", "reader-key")
	if c.Status != StatusWarn {
		t.Fatalf("status = %v, want a warning: %+v", c.Status, c)
	}
	if !strings.Contains(c.Remediation, "RIVORA_API_KEY") {
		t.Errorf("remediation should name the fix: %q", c.Remediation)
	}
}

func TestAPISecurityTreatsABlankReadOnlyKeyAsUnset(t *testing.T) {
	if c := apiSecurityWith(t, "", "   "); c.Status == StatusWarn {
		t.Errorf("a whitespace-only read-only key raised a warning: %+v", c)
	}
}

func TestAPISecurityNeverPrintsAKey(t *testing.T) {
	c := apiSecurityWith(t, "super-secret-admin-value", "super-secret-reader-value")
	all := c.Detail + c.Remediation
	if strings.Contains(all, "super-secret") {
		t.Errorf("a key value leaked into the report: %q", all)
	}
}
