#!/usr/bin/env bash
# Build ONE Perseid relay component from src/main.ts.
#
#   ./build.sh <out.wasm>
#
# `hack/perseid-build.sh` calls this, then ingests the artifact. Kept separate so
# a single component can be built and inspected by hand — which is what the
# quickstart walks through.
set -euo pipefail
cd "$(dirname "$0")"
OUT="${1:?usage: build.sh <out.wasm>}"

DWARF_BIN="${DWARF_BIN:-dwarf}"
if ! command -v "$DWARF_BIN" >/dev/null 2>&1; then
  for c in "$HOME/git/dwarf/target/release/dwarf" "$HOME/git/dwarf/target/debug/dwarf"; do
    [ -x "$c" ] && DWARF_BIN="$c" && break
  done
fi
command -v "$DWARF_BIN" >/dev/null 2>&1 || {
  echo "error: dwarf not found on PATH or at ~/git/dwarf/target/{release,debug}/dwarf" >&2
  exit 1
}

bunx vite build --logLevel warn

# --world relay: the world in wit/world.wit that imports observe + ensure and
# EXPORTS step. Named explicitly rather than defaulted, because the same wit
# also declares `provider`, `partial-provider` and `driver`, and picking the
# wrong one produces a component that links and does nothing.
"$DWARF_BIN" --wit wit --js dist/main.js --world relay --minify --opt-size -o "$OUT"
