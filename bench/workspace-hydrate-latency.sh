#!/usr/bin/env bash
#
# workspace-hydrate-latency.sh
#
# Reproducible source for the workspace hydrate-on-start and
# dehydrate-on-terminate latency numbers (EPIC W4, issue #21). Any published
# workspace hydrate/dehydrate number MUST be reproducible from this script per
# the repo's no-unverified-claims rule (CLAUDE.md operating principle 1): this
# file records the METHOD; numbers live only in bench/results/ next to the
# hardware, cluster, and store mode that produced them.
#
# What it measures (END-TO-END wall clock, user-visible, not an isolated engine
# number):
#
#   dehydrate = the time from asking a workspace-bound sandbox to
#               terminate (commit) until the workspace head advances to the new
#               committed revision (its /workspace tree captured into a
#               content-addressed revision).
#   hydrate   = the time from creating a fresh sandbox bound to that workspace
#               until it reaches Ready with the committed /workspace state
#               present (the head revision materialized into the new sandbox).
#
# Store mode: the numbers depend on the workspace store backend. Record which
# backend the measured workspace used (node CAS, S3 object store, and whether
# per-workspace encryption was enabled), because at-rest encryption and S3
# egress change the wall clock. The script prints the store mode it observed.
#
# It reuses the warm-pool wait pattern from bench/husk-activate-latency.sh so
# each hydrate sample is a real warm bind and not blocked on pool refill.
#
# Requirements: a running Mitos cluster with a warm pool, a Workspace object,
# kubectl + a KUBECONFIG that can create the objects in the namespace, and
# python3 with the mitos SDK installed.
#
# Usage:
#   bench/workspace-hydrate-latency.sh <kubeconfig> <pool> <workspace> [namespace] [iterations]
#
#   <kubeconfig>   path to a kubeconfig for the target cluster
#   <pool>         name of an existing, warm SandboxPool to bind from
#   <workspace>    name of an existing Workspace to hydrate/dehydrate
#   [namespace]    namespace (default: default)
#   [iterations]   sleep/wake cycles to measure (default: 11)
#
set -euo pipefail

if [ "$#" -lt 3 ]; then
  echo "usage: $0 <kubeconfig> <pool> <workspace> [namespace] [iterations]" >&2
  exit 2
fi

export KUBECONFIG="$1"
POOL="$2"
WS="$3"
NAMESPACE="${4:-default}"
ITERS="${5:-11}"

command -v kubectl >/dev/null 2>&1 || { echo "kubectl not found on PATH" >&2; exit 1; }
command -v python3 >/dev/null 2>&1 || { echo "python3 not found on PATH" >&2; exit 1; }

echo "measuring workspace hydrate/dehydrate latency: pool=$POOL workspace=$WS ns=$NAMESPACE iterations=$ITERS"

# The measured cycles run in the SDK so the wall clock spans the user-visible
# dehydrate (commit to head advance) and hydrate (bind to Ready with state
# present). Prints one "iter <i>: dehydrate_ms=<n> hydrate_ms=<n>" line per
# cycle, then the min/p50/p95/max distribution and the observed store mode.
python3 - "$NAMESPACE" "$POOL" "$WS" "$ITERS" <<'PY'
import sys, time
from mitos.client import AgentRun

ns, pool, ws_name, iters = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4])
client = AgentRun(namespace=ns, in_cluster=False)
ws = client.workspace(ws_name)


def wait_ready(sb, secs=240):
    try:
        sb.wait_until_ready(timeout=secs)
        return True
    except Exception as e:  # noqa: BLE001
        print(f"  not ready: {e}", file=sys.stderr)
        return False


def head_set():
    return {r.name for r in ws.log()}


dehydrate_ms, hydrate_ms = [], []

for i in range(iters):
    sb = client.create(pool=pool, workspace=ws_name, timeout="10m")
    if not wait_ready(sb):
        print(f"iter {i}: bind not Ready, skipping", file=sys.stderr)
        continue
    sb.exec("mkdir -p /workspace/state && date +%s%N > /workspace/state/marker.txt")
    before = head_set()

    # dehydrate: commit on terminate, timed until the head advances.
    t0 = time.time()
    sb.terminate()
    deadline = time.time() + 240
    advanced = False
    while time.time() < deadline:
        if head_set() - before:
            advanced = True
            break
        time.sleep(0.5)
    if not advanced:
        print(f"iter {i}: head did not advance, skipping dehydrate sample", file=sys.stderr)
        continue
    d_ms = (time.time() - t0) * 1000.0
    dehydrate_ms.append(d_ms)

    # hydrate: fresh bound sandbox, timed until Ready with state present.
    t1 = time.time()
    wk = client.create(pool=pool, workspace=ws_name, timeout="10m")
    if wait_ready(wk):
        # confirm the committed state actually materialized.
        res = wk.exec("cat /workspace/state/marker.txt", timeout=30)
        ok = (res.exit_code == 0 and res.stdout.strip() != "")
        h_ms = (time.time() - t1) * 1000.0
        if ok:
            hydrate_ms.append(h_ms)
        else:
            print(f"iter {i}: hydrated sandbox missing committed state", file=sys.stderr)
    wk.terminate()
    print(f"iter {i}: dehydrate_ms={dehydrate_ms[-1] if dehydrate_ms else float('nan'):.1f} "
          f"hydrate_ms={hydrate_ms[-1] if hydrate_ms else float('nan'):.1f}")


def stats(label, xs):
    if not xs:
        print(f"{label}: no samples")
        return
    xs = sorted(xs)
    def nth(p):
        import math
        r = max(1, math.ceil(p / 100.0 * len(xs)))
        return xs[r - 1]
    print(f"{label} (ms), N={len(xs)}: min={xs[0]:.1f} p50={nth(50):.1f} p95={nth(95):.1f} max={xs[-1]:.1f}")


stats("dehydrate", dehydrate_ms)
stats("hydrate", hydrate_ms)

# Report the observed store mode so the result file is unambiguous about which
# backend and encryption setting produced the numbers.
try:
    store = ws.store_mode()
except Exception:  # noqa: BLE001
    store = "unknown (record manually: node-cas|s3, encryption on|off)"
print(f"STORE_MODE {store}")
PY

echo
echo "Record these numbers in bench/results/ alongside the hardware, cluster,"
echo "store backend (node CAS or S3), and whether per-workspace encryption was"
echo "enabled. Numbers are reproducible only from this script: do not publish a"
echo "number that this script did not produce (CLAUDE.md operating principle 1)."
