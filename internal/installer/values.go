// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package installer

import (
	"fmt"
	"strings"

	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/strvals"
)

// MergeValues builds a user-overrides values map from -f files (applied in
// order) followed by --set expressions (applied in order, later wins) —
// the same precedence `helm install`/`helm upgrade` use. This is only the
// override map: action.Install/Upgrade.Run coalesces it with the chart's
// own values.yaml defaults internally, so callers should not pre-merge
// chart defaults themselves.
func MergeValues(valuesFiles []string, setValues []string) (map[string]interface{}, error) {
	vals := map[string]interface{}{}
	for _, f := range valuesFiles {
		fileVals, err := chartutil.ReadValuesFile(f)
		if err != nil {
			return nil, fmt.Errorf("read values file %s: %w", f, err)
		}
		vals = mergeMaps(vals, fileVals)
	}
	for _, s := range setValues {
		if err := strvals.ParseInto(s, vals); err != nil {
			return nil, fmt.Errorf("parse --set %q: %w", s, err)
		}
	}
	return vals, nil
}

func mergeMaps(dst, src map[string]interface{}) map[string]interface{} {
	for k, v := range src {
		if srcMap, ok := v.(map[string]interface{}); ok {
			if dstMap, ok := dst[k].(map[string]interface{}); ok {
				dst[k] = mergeMaps(dstMap, srcMap)
				continue
			}
		}
		dst[k] = v
	}
	return dst
}

// RequireInterface fails fast if rivorad.interface isn't set anywhere in
// the user-supplied overrides — it can't be safely defaulted or
// auto-detected (it must match the interface name across whichever nodes
// the DaemonSet schedules onto, not something knowable from the operator's
// own workstation) and a missing value otherwise only surfaces much later
// as an opaque Helm template-render error.
func RequireInterface(vals map[string]interface{}) error {
	rivorad, ok := vals["rivorad"].(map[string]interface{})
	if !ok {
		return errMissingInterface
	}
	iface, _ := rivorad["interface"].(string)
	if strings.TrimSpace(iface) == "" {
		return errMissingInterface
	}
	return nil
}

var errMissingInterface = fmt.Errorf(
	"rivorad.interface is required — set it with --set rivorad.interface=<iface> or in a -f values.yaml file " +
		"(the host network interface XDP/TCX attach to; every node the DaemonSet schedules onto must have an interface of this name)")
