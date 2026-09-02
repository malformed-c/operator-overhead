#!/usr/bin/env bash
# Build the guest-side probe. Shares ../relay/wit — the same `world relay`, so the
# probe demands exactly what the relay demands and cannot pass on a world the real
# component would fail.
#
#   ./build.sh <out.wasm>
set -euo pipefail
cd "$(dirname "$0")"
OUT="${1:?usage: build.sh <out.wasm>}"

DWARF_BIN="${DWARF_BIN:-dwarf}"
command -v "$DWARF_BIN" >/dev/null 2>&1 || {
  for c in "$HOME/git/dwarf/target/release/dwarf" "$HOME/git/dwarf/target/debug/dwarf"; do
    [ -x "$c" ] && DWARF_BIN="$c" && break
  done
}
bunx --bun vite build --logLevel warn --config ../relay/vite.config.ts --root . 2>/dev/null ||
  bunx vite build --logLevel warn --config vite.config.ts
"$DWARF_BIN" --wit ../relay/wit --js dist/main.js --world relay --minify --opt-size -o "$OUT"
