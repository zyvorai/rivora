#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
# Build Rivora's BPF objects and Go binaries on Linux.
# Usage: ./scripts/build-linux-release.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

if [ "$(uname -s)" != "Linux" ]; then
    echo "build-linux-release.sh must run on Linux (BPF objects need linux/bpf.h)." >&2
    exit 1
fi

echo "Building BPF objects..."
make bpf

echo "Building Go binaries..."
make build

echo "  rivorad:       $(./bin/rivorad -version 2>/dev/null || echo built)"
echo "  rivoractl:     $(./bin/rivoractl version 2>/dev/null || echo built)"
echo "  rivora-doctor: built"
