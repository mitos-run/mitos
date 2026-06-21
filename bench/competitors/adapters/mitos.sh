#!/usr/bin/env bash
#
# mitos.sh -- the reference adapter, wired to THIS repo's own harness.
#
# This adapter measures mitos's create-sandbox -> first-exec via the same
# bench/claim Go harness bench/claim-first-exec-latency.sh uses. It is the only
# adapter here wired to a real system, because mitos is the system this repo
# owns. The competitor adapters in this directory are placeholders a reproducer
# fills in for their own deployment.
#
# It runs ONE claim per create_exec call (--iterations 1) and parses the single
# claim's measured milliseconds out of the harness output, so run-comparison.sh
# can aggregate across systems with its own percentile logic.
#
# "Warm" for mitos: a warm SandboxPool with dormant slots ready, so the measured
# number is the warm claim hot path, not a cold pool fill.
#
# Required environment:
#   MITOS_KUBECONFIG  kubeconfig for the mitos cluster
#   MITOS_POOL        a warm SandboxPool name
#   MITOS_NAMESPACE   namespace (default: default)
#
# Sourced by run-comparison.sh; defines warm() and create_exec() only.

: "${MITOS_NAMESPACE:=default}"

_mitos_bin=""

warm() {
  if [ -z "${MITOS_KUBECONFIG:-}" ] || [ -z "${MITOS_POOL:-}" ]; then
    echo "mitos adapter needs MITOS_KUBECONFIG and MITOS_POOL set" >&2
    return 1
  fi
  command -v go >/dev/null 2>&1 || { echo "go not found on PATH" >&2; return 1; }
  # Build the harness once.
  local repo_root
  repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
  _mitos_bin="$(mktemp -t claim-bench.XXXXXX)"
  ( cd "$repo_root" && go build -o "$_mitos_bin" ./bench/claim/ )
  # The warm pool is assumed already filled by the operator; we do not size it
  # here. Document the pool's warm replica count alongside the result.
  echo "mitos adapter warmed: harness built, pool=$MITOS_POOL assumed warm" >&2
}

create_exec() {
  # Run a single claim->first-exec and parse the per-claim "claim 0: <ms> ms"
  # line the harness prints.
  local out ms
  out="$("$_mitos_bin" \
    --mode claim-exec \
    --kubeconfig "$MITOS_KUBECONFIG" \
    --namespace "$MITOS_NAMESPACE" \
    --pool "$MITOS_POOL" \
    --iterations 1 2>/dev/null)" || return 1
  ms="$(printf '%s\n' "$out" | sed -n 's/^  claim 0: \([0-9][0-9.]*\) ms$/\1/p' | head -1)"
  if [ -z "$ms" ]; then
    echo "mitos adapter: could not parse a sample from harness output" >&2
    return 1
  fi
  printf '%s\n' "$ms"
}
