#!/usr/bin/env bash
#
# pool-rebuild-propagation.sh
#
# Reproducible source for the pool-rebuild propagation number (issue #15 item 3):
# the time from a pool update to all of the pool's nodes having the snapshot
# ready again (ReadySnapshots == TotalSnapshots in the pool status). This ties to
# snapshot distribution: it is how long a fresh template snapshot takes to become
# claimable across every node of the pool.
#
# What it measures: the harness reads the pool's current digest and node
# distribution, triggers a rebuild (it bumps spec.replicas to force the pool
# reconcile to re-evaluate snapshot distribution to every node; a maintainer
# measuring a real template change instead points spec.templateRef at a new
# template before running, with the same method), then times from the spec change
# to the pool status reporting all snapshots ready, and restores the original
# replicas.
#
# This is meaningful on a MULTI-NODE cluster (the propagation is per node). On a
# single-node cluster it still runs but measures only that one node; the harness
# prints a note to that effect.
#
# Thin wrapper around the bench/claim Go harness in pool-rebuild mode. It drives
# a REAL cluster; with no reachable cluster it fails with a clear message and
# produces NO number (no unverified claims).
#
# Requirements: a running Mitos cluster (ideally multi-node) with the controller
# and an existing SandboxPool, Go, and a KUBECONFIG that can update the pool and
# read its status.
#
# Usage:
#   bench/pool-rebuild-propagation.sh <kubeconfig> <pool> [namespace]
#
#   <kubeconfig>   path to a kubeconfig for the target cluster
#   <pool>         name of an existing SandboxPool
#   [namespace]    namespace (default: default)
#
# Record the printed propagation latency in bench/results/ with the node count,
# the template size, and the hardware so the number is reproducible.
#
set -euo pipefail

if [ "$#" -lt 2 ]; then
  echo "usage: $0 <kubeconfig> <pool> [namespace]" >&2
  exit 2
fi

KUBECONFIG_PATH="$1"
POOL="$2"
NAMESPACE="${3:-default}"

command -v go >/dev/null 2>&1 || { echo "go not found on PATH; needed to build the bench/claim harness" >&2; exit 1; }

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$(mktemp -t claim-bench.XXXXXX)"
trap 'rm -f "$BIN"' EXIT

echo "building bench/claim harness ..."
( cd "$REPO_ROOT" && go build -o "$BIN" ./bench/claim/ )

echo "measuring pool-rebuild propagation: pool=$POOL ns=$NAMESPACE"
"$BIN" \
  --mode pool-rebuild \
  --kubeconfig "$KUBECONFIG_PATH" \
  --namespace "$NAMESPACE" \
  --pool "$POOL"
