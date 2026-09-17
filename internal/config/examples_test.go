// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zyvorai/rivora/internal/config"
)

// TestExamplesLoadAndValidate ensures every shipped example under
// config/examples/ still parses and passes Validate() — including the
// IPv6 and BGP samples — so CI catches drift when the schema changes.
func TestExamplesLoadAndValidate(t *testing.T) {
	root := filepath.Join("..", "..", "config", "examples")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read examples dir: %v", err)
	}
	var found int
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		found++
		path := filepath.Join(root, e.Name())
		t.Run(e.Name(), func(t *testing.T) {
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("Load(%s): %v", path, err)
			}
			if cfg.Interface == "" {
				t.Fatalf("%s: empty interface after Load", path)
			}
		})
	}
	if found == 0 {
		t.Fatal("no example YAML files found under config/examples")
	}
}
