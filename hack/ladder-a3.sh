#!/usr/bin/env bash
# A3 (one shared manager) across the ladder, plus the A1 N=64 cell that failed
# to come up on the first run.
set -uo pipefail
cd "$(dirname "$0")/.."
unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY all_proxy ALL_PROXY
BENCH=${BENCH:-/tmp/bench}
export PATH=/usr/local/bin:$PATH SUDO_ASKPASS=${SUDO_ASKPASS:-/tmp/askpass.sh}

# Built here, every run — see hack/ladder.sh for why a cached /tmp binary is a
# silent-stale hazard rather than a convenience.
go build -o "$BENCH" ./cmd/bench
export BENCH_APISERVER="$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}'|sed 's|https://||')"

run_cell() {  # arm n up_timeout converge_timeout
    local arm=$1 n=$2 up=$3 conv=$4
    echo "===== N=$n arm=$arm (up<=${up}s)"
    if ! timeout $((up + 60)) $BENCH up -arm "$arm" -n "$n" -timeout "${up}s"; then
        echo "UP FAILED: N=$n arm=$arm"; $BENCH down -arm "$arm" >/dev/null 2>&1; return 1
    fi
    sleep 10
    sudo -A -E "$BENCH" run -arm "$arm" -n "$n" \
        -settle 10s -idle 45s -change 45s -converge-timeout "${conv}s" \
        || echo "RUN FAILED (recorded): N=$n arm=$arm"
    $BENCH down -arm "$arm" >/dev/null 2>&1
    sleep 5
}

for n in 1 8 32 64; do
    $BENCH fixtures -n "$n" || continue
    run_cell a3-cr-shared "$n" $(( 120 + n * 2 )) $(( 60 + n * 3 ))
done

# ***THE A1 N=64 RETEST, WITH DOUBLE THE PATIENCE.*** It reached 60/64 in 632s
# and the message could not say whether the other four were missing or unready.
# It can now, and this gives the scheduler 20 minutes instead of 10.
$BENCH fixtures -n 64
run_cell a1-cr-leader 64 1200 252

echo "===== done; fixtures to 0"; $BENCH fixtures -n 0
