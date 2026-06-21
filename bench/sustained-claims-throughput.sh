#!/usr/bin/env bash
#
# sustained-claims-throughput.sh
#
# Reproducible source for the sustained claims/sec and per-node density numbers
# (issue #15 item 2). It drives a sustained claim ARRIVAL rate against a real
# pool for a fixed window and records the ACHIEVED claims/sec, the peak
# concurrency, and the per-node density (the density curve), with methodology.
#
# What it measures: the harness arrives claims at --rate claims/sec for
# --duration, records each claim's Ready offset, the concurrency at that instant,
# and the node it landed on (status.node), then reports achieved claims/sec, peak
# concurrent claims, and per-node peak density. The arrival rate is the driver;
# the achieved rate and density are the measurement. Aggregation is the
# unit-tested internal/benchstat.AggregateThroughput.
#
# This is a thin wrapper around the bench/claim Go harness in sustained mode. It
# drives a REAL cluster; there is no offline path. With no reachable cluster it
# fails with a clear message and produces NO number (no unverified claims).
#
# To trace the density curve, run several times sweeping --rate (or
# --max-concurrent) and record each (rate -> achieved/sec, peak density,
# per-node density) point. The p99 degradation point is where achieved/sec stops
# tracking the target rate as you push the arrival rate up.
#
# Requirements: a running mitos cluster with the controller and a warm
# SandboxPool sized for the target concurrency, Go, and a KUBECONFIG that can
# create and read SandboxClaims in the namespace.
#
# Usage:
#   bench/sustained-claims-throughput.sh <kubeconfig> <pool> [namespace] [rate] [duration] [max_concurrent]
#
#   <kubeconfig>      path to a kubeconfig for the target cluster
#   <pool>            name of an existing SandboxPool to claim from
#   [namespace]       namespace (default: default)
#   [rate]            target claim arrival rate, claims/sec (default: 5)
#   [duration]        how long to keep arriving claims, e.g. 30s, 2m (default: 30s)
#   [max_concurrent]  cap on simultaneously-live claims, 0 = no cap (default: 0)
#
# Record each run's printed table in bench/results/ with the hardware, the pool
# size, and the rate sweep so the density curve is reproducible and auditable.
#
set -euo pipefail

if [ "$#" -lt 2 ]; then
  echo "usage: $0 <kubeconfig> <pool> [namespace] [rate] [duration] [max_concurrent]" >&2
  exit 2
fi

KUBECONFIG_PATH="$1"
POOL="$2"
NAMESPACE="${3:-default}"
RATE="${4:-5}"
DURATION="${5:-30s}"
MAX_CONCURRENT="${6:-0}"

command -v go >/dev/null 2>&1 || { echo "go not found on PATH; needed to build the bench/claim harness" >&2; exit 1; }

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$(mktemp -t claim-bench.XXXXXX)"
trap 'rm -f "$BIN"' EXIT

echo "building bench/claim harness ..."
( cd "$REPO_ROOT" && go build -o "$BIN" ./bench/claim/ )

echo "driving sustained claims: pool=$POOL ns=$NAMESPACE rate=${RATE}/s duration=$DURATION max_concurrent=$MAX_CONCURRENT"
"$BIN" \
  --mode sustained \
  --kubeconfig "$KUBECONFIG_PATH" \
  --namespace "$NAMESPACE" \
  --pool "$POOL" \
  --rate "$RATE" \
  --duration "$DURATION" \
  --max-concurrent "$MAX_CONCURRENT"
