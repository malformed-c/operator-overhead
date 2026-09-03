#!/usr/bin/env bash
# The experiment: three arms across {1, 8, 32, 64}.
#
#   ./hack/ladder.sh [N...]
set -uo pipefail
cd "$(dirname "$0")/.."
unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY all_proxy ALL_PROXY

BENCH=${BENCH:-/tmp/bench}
export PATH=/usr/local/bin:$PATH SUDO_ASKPASS=${SUDO_ASKPASS:-/tmp/askpass.sh}

# ***BUILT HERE, EVERY RUN, NEVER TRUSTED FROM A PRIOR ONE.*** /tmp is tmpfs: a
# binary left there survives across sessions but nothing rebuilds it, so it goes
# stale the moment a source file changes. Measured 2026-09-03: a day-old
# /tmp/bench silently ran every b2-perseid-fused cell against a since-fixed bug
# (WaitPerseidsRunning waiting for N objects on an arm that creates 1), and every
# rung failed identically with a message from code that no longer existed on
# disk. `go build` is seconds; a ladder run against the wrong binary is an hour.
go build -o "$BENCH" ./cmd/bench
export BENCH_APISERVER="$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}'|sed 's|https://||')"
export RADIANT_METRICS="http://$(kubectl get pod -n apsis -l app=radiant -o jsonpath='{.items[0].status.podIP}'):9600/metrics"

LADDER=("$@"); [ ${#LADDER[@]} -eq 0 ] && LADDER=(1 8 32 64)
# ***ALL FIVE, INCLUDING BOTH CONSOLIDATED ARMS.*** A ladder over the per-instance
# arms alone answers "what does the Nth operator cost" and cannot answer "is that a
# property of the runtime or of the process count" — which is the question A3 and B2
# exist for. They run one process at every N, so their cells are cheap.
ARMS=(a2-cr-noleader a1-cr-leader a3-cr-shared b-perseid b2-perseid-fused)

for n in "${LADDER[@]}"; do
    # Timeouts scale with N: 64 pods take longer to schedule than 1, and a
    # Perseid's first pass waits on radiant's tick. A bound tight enough for N=1
    # would fail N=64 for being large rather than for being broken.
    up_to=$(( 120 + n * 8 ))
    conv_to=$(( 60 + n * 3 ))

    echo "===== fixtures N=$n"
    $BENCH fixtures -n "$n" || { echo "FIXTURES FAILED at N=$n"; continue; }

    for arm in "${ARMS[@]}"; do
        echo "===== N=$n arm=$arm  (up<=${up_to}s converge<=${conv_to}s)"
        if ! timeout $((up_to + 30)) $BENCH up -arm "$arm" -n "$n" -timeout "${up_to}s"; then
            echo "UP FAILED: N=$n arm=$arm — skipping this cell, not retrying"
            $BENCH down -arm "$arm" >/dev/null 2>&1
            continue
        fi
        sleep 10
        # sudo for PSS: smaps_rollup needs PTRACE_MODE_READ and the workers are root.
        sudo -A -E "$BENCH" run -arm "$arm" -n "$n" \
            -settle 10s -idle 45s -change 45s -converge-timeout "${conv_to}s" \
            || echo "RUN FAILED (recorded): N=$n arm=$arm"
        $BENCH down -arm "$arm" >/dev/null 2>&1
        sleep 5
    done
done

echo "===== ladder complete; tearing fixtures down to 0"
$BENCH fixtures -n 0
