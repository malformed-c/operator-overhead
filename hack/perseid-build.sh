#!/usr/bin/env bash
# Build one Perseid component, ingest it, and prove the node published a
# ComponentManifest for it.
#
#   hack/perseid-build.sh perseid/relay relay:v1
#   hack/perseid-build.sh perseid/fused relay-fused:v1
set -euo pipefail

DIR="${1:?usage: perseid-build.sh <component-dir> <name:tag>}"
NAME="${2:?usage: perseid-build.sh <component-dir> <name:tag>}"
OUT="${OUT:-build/perseid/$(basename "$DIR")}"
APSIS="${APSIS:-apsis}"
TRAIL="${TRAIL:-trail}"

cd "$(dirname "$0")/.."
mkdir -p "$OUT"
ARTIFACT="$(realpath -m "$OUT/relay.wasm")"

echo "building $DIR -> $ARTIFACT"
"$DIR/build.sh" "$ARTIFACT"

# ASSERTION 1: the component imports wasi:cli/environment.
#
# ***THE STEP READS ITS PAIR FROM `process.env`, WHICH dwarf BACKS WITH THAT
# INTERFACE.*** Without the import it throws at the first access, the pass fails,
# and the failure text is about a missing backing import rather than about
# configuration — so a relay that cannot read which ConfigMaps it relays looks
# perfectly admitted while relaying nothing. The ComponentManifest cannot catch
# this: it records what the artifact imports, and this is a claim about what the
# artifact MUST import to do its job.
if ! "$TRAIL" --inspect "$ARTIFACT" 2>/dev/null | grep -q 'wasi:cli/environment'; then
  echo "error: $ARTIFACT imports no wasi:cli/environment, so process.env is empty" >&2
  echo "       inside the guest and the step yields at its config guard" >&2
  exit 1
fi

echo "ingesting as $NAME"
"$APSIS" ingest "$ARTIFACT" --name "$NAME"

# ASSERTION 2: a ComponentManifest exists for this component.
#
# ***THIS IS AN ORDERING DEPENDENCY, AND NOTHING ELSE CHECKS IT.*** With no
# `spec.imports`, admission needs the manifest — and the manifest is written by
# whichever node inspected the component, cluster-scoped, `Create` as the
# arbiter. Ingesting on a node that has not published one yet leaves a Perseid
# that refuses for a reason that reads like a capability problem. Fail here,
# where the cause is one line up, instead of there.
for _ in $(seq 1 30); do
  found=$(kubectl get componentmanifests -o \
    jsonpath="{range .items[?(@.spec.component=='$NAME')]}{.metadata.name} {.status.inspectedBy}{end}" 2>/dev/null)
  [ -n "$found" ] && { echo "manifest: $found"; exit 0; }
  sleep 2
done

echo "error: no ComponentManifest for $NAME after 60s." >&2
echo "       Admission has no artifact evidence and no spec.imports to fall back on," >&2
echo "       so every Perseid naming this component will be refused." >&2
exit 1
