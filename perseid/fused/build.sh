#!/usr/bin/env bash
# Build the fused relay. Shares ../relay/wit — the same `world relay`, so the
# fused component demands exactly what the per-pair one demands.
set -euo pipefail
cd "$(dirname "$0")"
OUT="${1:?usage: build.sh <out.wasm>}"
DWARF_BIN="${DWARF_BIN:-dwarf}"
bunx vite build --logLevel warn
"$DWARF_BIN" --wit ../relay/wit --js dist/main.js --world relay --minify --opt-size -o "$OUT"
