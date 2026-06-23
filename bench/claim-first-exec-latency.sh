#!/usr/bin/env bash
#
# claim-first-exec-latency.sh
#
# Reproducible source for the claim -> first-exec end-to-end number (issue #15
# item 1). This is the FULL controller + pool + scheduler path, NOT the in-process
# engine data path that cmd/bench measures: it creates a real SandboxClaim, waits
# for the controller to drive it Ready, and runs the first exec over the sandbox
# HTTP API (the same endpoint + per-sandbox bearer token the SDK and
# kubectl-mitos use).
#
# What it measures: for each of N sequential claims, the wall clock from claim
# create to the first successful exec result. Summarized as nearest-rank
# P50/P90/P99 by internal/benchstat (the same summarizer cmd/bench uses).
#
# This script is a thin wrapper that builds and runs the bench/claim Go harness
# in claim-exec mode. The harness drives a REAL cluster: there is no offline or
# faked path. On a host with no reachable cluster (for example a darwin laptop)
# it fails at client construction with a clear message and produces NO number
# (CLAUDE.md operating principle 1: no unverified claims).
#
# Requirements: a running mitos cluster with the controller deployed and a warm
# SandboxPool, plus Go (to build the harness) and a KUBECONFIG that can create
# SandboxClaims and read the per-claim token Secret in the namespace.
#
# Usage:
#   bench/claim-first-exec-latency.sh <kubeconfig> <pool> [namespace] [iterations]
#
#   <kubeconfig>   path to a kubeconfig for the target cluster
#   <pool>         name of an existing, warm SandboxPool to claim from
#   [namespace]    namespace to create claims in (default: default)
#   [iterations]   number of sequential claims to measure (default: 20)
#
# Record the printed distribution in bench/results/ alongside the hardware and
# cluster spec so the number is reproducible and auditable.
#
set -euo pipefail

if [ "$#" -lt 2 ]; then
  echo "usage: $0 <kubeconfig> <pool> [namespace] [iterations]" >&2
  exit 2
fi

KUBECONFIG_PATH="$1"
POOL="$2"
NAMESPACE="${3:-default}"
ITERATIONS="${4:-20}"

command -v go >/dev/null 2>&1 || { echo "go not found on PATH; needed to build the bench/claim harness" >&2; exit 1; }

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$(mktemp -t claim-bench.XXXXXX)"
trap 'rm -f "$BIN"' EXIT

echo "building bench/claim harness ..."
( cd "$REPO_ROOT" && go build -o "$BIN" ./bench/claim/ )

echo "measuring claim -> first-exec: pool=$POOL ns=$NAMESPACE iterations=$ITERATIONS"
"$BIN" \
  --mode claim-exec \
  --kubeconfig "$KUBECONFIG_PATH" \
  --namespace "$NAMESPACE" \
  --pool "$POOL" \
  --iterations "$ITERATIONS"
