#!/usr/bin/env bash
#
# workspace-fork-latency.sh
#
# Reproducible source for the workspace fork latency number (EPIC W4, issue
# #21). Any published workspace-fork number MUST be reproducible from this
# script (CLAUDE.md operating principle 1): this file records the METHOD;
# numbers live only in bench/results/ next to the hardware and cluster.
#
# What it measures and asserts:
#
#   fork_ms   = the wall clock of `mitos ws fork <src-ws> <revision> <dst-ws>`,
#               which branches a committed revision into a new workspace.
#   O(0) bytes = a fork is a CONTENT-ADDRESSED BRANCH: the new revision SHARES
#               the parent's content manifest, so NO new content-store bytes are
#               written (ADR-0002 reason 1, docs/adr/0002-workspace-not-csi.md).
#               This script asserts that the forked revision's contentManifest
#               equals the parent revision's contentManifest. If they differ the
#               script FAILS: the fork wrote new bytes and dedup is broken.
#
# Because a fork writes no content bytes, fork_ms is a control-plane number (the
# revision-object create plus the reconcile to Committed), not a data-path
# number; that is the point of the workspace fork design and what this asserts.
#
# Requirements: a running mitos cluster with at least one Workspace that has a
# committed head revision, the mitos CLI on PATH (built from this repo), and
# kubectl + a KUBECONFIG that can read/create the objects in the namespace.
#
# Usage:
#   bench/workspace-fork-latency.sh <kubeconfig> <src-workspace> [namespace] [iterations]
#
#   <kubeconfig>    path to a kubeconfig for the target cluster
#   <src-workspace> a Workspace whose head revision is forked each iteration
#   [namespace]     namespace (default: default)
#   [iterations]    forks to measure (default: 11)
#
set -euo pipefail

if [ "$#" -lt 2 ]; then
  echo "usage: $0 <kubeconfig> <src-workspace> [namespace] [iterations]" >&2
  exit 2
fi

export KUBECONFIG="$1"
SRC_WS="$2"
NAMESPACE="${3:-default}"
ITERS="${4:-11}"

command -v kubectl >/dev/null 2>&1 || { echo "kubectl not found on PATH" >&2; exit 1; }
command -v mitos >/dev/null 2>&1 || { echo "mitos CLI not found on PATH; build it from this repo (go build -o mitos ./cmd/agent or your CLI entrypoint)" >&2; exit 1; }

RUN_ID="$(date +%s)-$$"
samples=()
created_branches=()

cleanup() {
  for b in "${created_branches[@]}"; do
    mitos ws rm "$b" >/dev/null 2>&1 || \
      kubectl -n "$NAMESPACE" delete workspace "$b" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  done
}
trap cleanup EXIT

# Resolve the src workspace head revision and its content manifest.
HEAD_REV="$(kubectl -n "$NAMESPACE" get workspace "$SRC_WS" -o jsonpath='{.status.head}')"
if [ -z "$HEAD_REV" ]; then
  echo "src workspace $SRC_WS has no committed head revision; commit one first" >&2
  exit 1
fi
PARENT_MANIFEST="$(kubectl -n "$NAMESPACE" get workspacerevision "$HEAD_REV" -o jsonpath='{.spec.contentManifest}')"
echo "src=$SRC_WS head=$HEAD_REV parentManifest=${PARENT_MANIFEST:0:12}... iterations=$ITERS"

for i in $(seq 1 "$ITERS"); do
  branch="fork-bench-${RUN_ID}-${i}"
  mitos ws create "$branch" >/dev/null 2>&1 || \
    kubectl -n "$NAMESPACE" apply -f - >/dev/null <<EOF
apiVersion: mitos.run/v1
kind: Workspace
metadata:
  name: ${branch}
spec: {}
EOF
  created_branches+=("$branch")

  # Time the fork verb itself.
  start_ns=$(date +%s%N)
  new_rev="$(mitos ws fork "$SRC_WS" "$HEAD_REV" "$branch")"
  end_ns=$(date +%s%N)
  fork_ms=$(awk -v s="$start_ns" -v e="$end_ns" 'BEGIN { printf "%.1f", (e - s) / 1000000.0 }')

  # Assert O(0) new bytes: the forked revision shares the parent content manifest.
  # Wait briefly for the revision object to materialize, then read its manifest.
  child_manifest=""
  for _ in $(seq 1 60); do
    child_manifest="$(kubectl -n "$NAMESPACE" get workspacerevision "$new_rev" -o jsonpath='{.spec.contentManifest}' 2>/dev/null || true)"
    [ -n "$child_manifest" ] && break
    sleep 0.2
  done
  if [ "$child_manifest" != "$PARENT_MANIFEST" ]; then
    echo "FAIL: fork $i wrote new content: child manifest ${child_manifest:0:12}... != parent ${PARENT_MANIFEST:0:12}..." >&2
    echo "A workspace fork must be a content-addressed branch (O(0) new bytes); dedup is broken." >&2
    exit 1
  fi
  echo "  fork $i: ${fork_ms} ms (rev=${new_rev}, shared manifest: O(0) new bytes)"
  samples+=("$fork_ms")
done

n="${#samples[@]}"
if [ "$n" -eq 0 ]; then
  echo "no successful samples" >&2
  exit 1
fi
sorted=$(printf '%s\n' "${samples[@]}" | sort -n)
nth() {
  local p="$1" rank
  rank=$(awk -v p="$p" -v n="$n" 'BEGIN { r = (p/100.0)*n; ri = int(r); if (r > ri) ri = ri + 1; if (ri < 1) ri = 1; print ri }')
  printf '%s\n' "$sorted" | sed -n "${rank}p"
}
echo
echo "workspace fork latency (ms), N=$n (every fork verified O(0) new bytes):"
echo "  min  $(printf '%s\n' "$sorted" | head -1)"
echo "  P50  $(nth 50)"
echo "  P95  $(nth 95)"
echo "  max  $(printf '%s\n' "$sorted" | tail -1)"
echo
echo "Record in bench/results/ with the hardware and cluster. Numbers are"
echo "reproducible only from this script (CLAUDE.md operating principle 1)."
