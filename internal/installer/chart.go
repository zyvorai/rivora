// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package installer drives the Rivora Helm chart's lifecycle
// (install/upgrade/uninstall/status) as a library, for the `rivora` CLI
// (cmd/rivora). It uses helm.sh/helm/v3 directly rather than reimplementing
// chart templating, against a copy of deploy/helm/rivora embedded into the
// binary — see chartdata.go and the Makefile's sync-chart/check-chart-sync
// targets for how that copy is kept in sync with the canonical chart.
package installer

import (
	"embed"
	"io/fs"
	"strings"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
)

//go:embed all:chartdata/rivora
var chartFS embed.FS

// chartRoot is chartFS's single top-level directory — chart.yaml et al.
// live one level below the embed directive's own root.
const chartRoot = "chartdata/rivora"

// LoadChart parses the embedded copy of deploy/helm/rivora into a
// *chart.Chart, ready to pass to an install/upgrade/template action.
func LoadChart() (*chart.Chart, error) {
	var files []*loader.BufferedFile
	err := fs.WalkDir(chartFS, chartRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(chartFS, path)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(path, chartRoot), "/")
		files = append(files, &loader.BufferedFile{Name: rel, Data: data})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return loader.LoadFiles(files)
}
