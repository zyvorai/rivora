// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package logging builds the slog.Logger the Rivora daemons share, so rivorad
// and rivora-controller expose the same -log-level / -log-format flags.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// New returns a logger writing to w. level is one of debug, info, warn/warning,
// error (case-insensitive); format is "text" (the default, human-friendly) or
// "json" (one object per line, for log shippers). Empty strings mean info/text.
func New(w io.Writer, level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "", "info":
		lvl = slog.LevelInfo
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, fmt.Errorf("invalid log level %q (want debug, info, warn or error)", level)
	}

	opts := &slog.HandlerOptions{Level: lvl}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	default:
		return nil, fmt.Errorf("invalid log format %q (want text or json)", format)
	}
}
