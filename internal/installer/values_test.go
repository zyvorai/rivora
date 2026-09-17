// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package installer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMergeValuesSetOnly(t *testing.T) {
	vals, err := MergeValues(nil, []string{"rivorad.interface=eth0", "controller.workers=4"})
	if err != nil {
		t.Fatalf("MergeValues: %v", err)
	}
	rivorad, ok := vals["rivorad"].(map[string]interface{})
	if !ok || rivorad["interface"] != "eth0" {
		t.Errorf("rivorad.interface = %v, want eth0", vals["rivorad"])
	}
}

func TestMergeValuesFileThenSetOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "values.yaml")
	if err := os.WriteFile(path, []byte("rivorad:\n  interface: eth0\n  workers: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	vals, err := MergeValues([]string{path}, []string{"rivorad.interface=eth1"})
	if err != nil {
		t.Fatalf("MergeValues: %v", err)
	}
	rivorad := vals["rivorad"].(map[string]interface{})
	if rivorad["interface"] != "eth1" {
		t.Errorf("--set should win over -f: rivorad.interface = %v, want eth1", rivorad["interface"])
	}
	if fmtInt(rivorad["workers"]) != 2 {
		t.Errorf("unrelated file value should survive the --set merge: rivorad.workers = %v, want 2", rivorad["workers"])
	}
}

func TestMergeValuesMultipleFilesInOrder(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.yaml")
	second := filepath.Join(dir, "second.yaml")
	if err := os.WriteFile(first, []byte("rivorad:\n  interface: eth0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("rivorad:\n  interface: eth1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	vals, err := MergeValues([]string{first, second}, nil)
	if err != nil {
		t.Fatalf("MergeValues: %v", err)
	}
	rivorad := vals["rivorad"].(map[string]interface{})
	if rivorad["interface"] != "eth1" {
		t.Errorf("later -f file should win: rivorad.interface = %v, want eth1", rivorad["interface"])
	}
}

func TestMergeValuesInvalidSetErrors(t *testing.T) {
	if _, err := MergeValues(nil, []string{"rivorad.interface[abc]=eth0"}); err == nil {
		t.Error("expected an error for a non-numeric list index in a --set expression")
	}
}

func TestRequireInterface(t *testing.T) {
	cases := []struct {
		name    string
		vals    map[string]interface{}
		wantErr bool
	}{
		{"missing entirely", map[string]interface{}{}, true},
		{"empty string", map[string]interface{}{"rivorad": map[string]interface{}{"interface": ""}}, true},
		{"whitespace only", map[string]interface{}{"rivorad": map[string]interface{}{"interface": "  "}}, true},
		{"wrong type", map[string]interface{}{"rivorad": "not-a-map"}, true},
		{"set", map[string]interface{}{"rivorad": map[string]interface{}{"interface": "eth0"}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := RequireInterface(c.vals)
			if (err != nil) != c.wantErr {
				t.Errorf("RequireInterface(%v) error = %v, wantErr %v", c.vals, err, c.wantErr)
			}
		})
	}
}

func fmtInt(v interface{}) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return -1
	}
}
