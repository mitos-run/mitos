#!/usr/bin/env bash
#
# husk-network-e2e.sh
#
# Real-cluster husk EGRESS ISOLATION verification (the top untrusted-code
# blocker, docs/threat-model.md section 1 item 1). It drives a REAL KVM-capable
# Kubernetes node through a husk claim whose template has an egress allowlist,
# then asserts from INSIDE the sandbox (via the Python SDK exec over the pod
# network):
#
#   1. metadata block   curl 169.254.169.254 FAILS (no node IAM credential theft)
#   2. default-deny      curl a NON-allowlisted host FAILS
#   3. allowlist works   curl an ALLOWLISTED host SUCCEEDS (so the in-pod nft
#                        filter + DNS proxy + the controller-to-stub NIC binding
#                        are all correct)
#
# Each prints a PASS/FAIL line and a bash tally is the single source of truth for
# the exit code. Gated EXACTLY like husk-e2e.sh: it runs from inside the cluster
# (the self-hosted KVM runner's ServiceAccount) and reaches the per-claim sandbox
# HTTP API over the pod network via the Python SDK (in_cluster=True). It reuses
# the warm-dormant-pod wait and SDK-driver shape from husk-e2e.sh; the only new
# behavior is the three egress assertions.
#
# Usage:
#   test/cluster-e2e/husk-network-e2e.sh [namespace] [kubeconfig]
#
#   [namespace]   namespace to run the e2e in (default: mitos-e2e)
#   [kubeconfig]  optional kubeconfig path; omit to use the in-cluster SA
#
# Env knobs:
#   READY_TIMEOUT   per-stage wait budget, seconds (default 180)
#   POLL_INTERVAL   poll interval, seconds (default 1)
#   E2E_IMAGE       template image (default mirror.gcr.io/library/python:3.12-slim)
#   ALLOW_HOST      allowlisted host:port to prove reachable (default example.com:443)
#   DENY_HOST       non-allowlisted host to prove blocked (default 1.1.1.1)
#
set -euo pipefail

NAMESPACE="${1:-mitos-e2e}"
KUBECONFIG_ARG="${2:-}"
if [ -n "$KUBECONFIG_ARG" ]; then
  export KUBECONFIG="$KUBECONFIG_ARG"
fi

READY_TIMEOUT="${READY_TIMEOUT:-180}"
POLL_INTERVAL="${POLL_INTERVAL:-1}"
E2E_IMAGE="${E2E_IMAGE:-mirror.gcr.io/library/python:3.12-slim}"
ALLOW_HOST="${ALLOW_HOST:-example.com:443}"
DENY_HOST="${DENY_HOST:-1.1.1.1}"
ALLOW_NAME="${ALLOW_HOST%%:*}"

RUN_ID="$(date +%s)-$$"
POOL="e2e-net-pool-${RUN_ID}"
CLAIM="e2e-net-claim-${RUN_ID}"

PASS_COUNT=0
FAIL_COUNT=0

pass() { echo "PASS: $*"; PASS_COUNT=$((PASS_COUNT + 1)); }
fail() { echo "FAIL: $*" >&2; FAIL_COUNT=$((FAIL_COUNT + 1)); }
info() { echo "  $*"; }

require() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing required tool: $1" >&2; exit 1; }
}
require kubectl
require python3

# kubectl in a namespace shorthand.
k() { kubectl -n "$NAMESPACE" "$@"; }

diagnostics() {
  echo "=== diagnostics (namespace ${NAMESPACE}) ===" >&2
  k get sandboxpools,sandboxes,networkpolicies -o wide >&2 2>&1 || true
  k get pods -o wide >&2 2>&1 || true
  echo "--- sandbox describe ---" >&2
  k describe sandbox "$CLAIM" >&2 2>&1 || true
  echo "--- recent husk pod logs ---" >&2
  for p in $(k get pods -l 'mitos.run/husk=true' -o name 2>/dev/null | head -3); do
    echo "--- logs $p ---" >&2
    k logs "$p" --tail=60 >&2 2>&1 || true
  done
}

cleanup() {
  rc=$?
  echo "=== teardown ==="
  k delete sandbox "$CLAIM" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  k delete sandboxpool "$POOL" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  k delete sandboxes -l "mitos.run/e2e-run=${RUN_ID}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  echo "teardown done"
  exit "$rc"
}
trap cleanup EXIT

echo "=== mitos husk network-isolation e2e: ns=${NAMESPACE} image=${E2E_IMAGE} run=${RUN_ID} ==="
echo "    allow=${ALLOW_HOST} deny=${DENY_HOST}"

# ---------------------------------------------------------------------------
# Stage 0: KVM node present (the husk path needs a KVM-capable node).
# ---------------------------------------------------------------------------
if kubectl get nodes -l 'mitos.run/kvm=true' -o name 2>/dev/null | grep -q node; then
  pass "a KVM-capable node (mitos.run/kvm=true) is present"
else
  fail "no node labeled mitos.run/kvm=true; the husk path cannot warm pods"
  diagnostics
  exit 1
fi

# ---------------------------------------------------------------------------
# Stage 1: a pool warms a dormant husk pod from a template whose NetworkPolicy
# DENIES by default and allows ONLY ${ALLOW_HOST}.
# ---------------------------------------------------------------------------
echo "--- stage 1: pool warms a dormant husk pod (egress deny, allow ${ALLOW_HOST}) ---"
k apply -f - >/dev/null <<EOF
apiVersion: mitos.run/v1
kind: SandboxPool
metadata:
  name: ${POOL}
  labels:
    mitos.run/e2e-run: "${RUN_ID}"
spec:
  template:
    image: ${E2E_IMAGE}
    resources:
      cpu: "250m"
      memory: "512Mi"
    network:
      egress: deny
      allow:
        - "${ALLOW_HOST}"
  snapshots:
    replicasPerNode: 1
EOF

# Wait for at least one dormant warm pod: husk=true, Running, no claim label.
warm_deadline=$(( $(date +%s) + READY_TIMEOUT ))
warm_ok=""
while [ "$(date +%s)" -lt "$warm_deadline" ]; do
  dormant="$(k get pods -l 'mitos.run/husk=true,!mitos.run/claim' \
    --field-selector=status.phase=Running -o name 2>/dev/null | head -1 || true)"
  if [ -n "$dormant" ]; then warm_ok="yes"; break; fi
  sleep "$POLL_INTERVAL"
done
if [ -n "$warm_ok" ]; then
  pass "pool ${POOL} warmed at least one dormant husk pod"
else
  fail "pool ${POOL} did not warm a dormant husk pod within ${READY_TIMEOUT}s"
  diagnostics
  exit 1
fi

# The best-effort NetworkPolicy object should exist for this pool (defense in
# depth; the in-pod nft filter is the guarantee). Non-fatal: a CNI without
# NetworkPolicy support still relies on the in-pod filter, which the egress
# assertions below prove.
if k get networkpolicy "${POOL}-husk-egress" >/dev/null 2>&1; then
  pass "best-effort husk NetworkPolicy ${POOL}-husk-egress exists"
else
  info "husk NetworkPolicy ${POOL}-husk-egress not found (non-fatal; the in-pod nft filter is the guarantee)"
fi

# ---------------------------------------------------------------------------
# Stage 2: the three egress assertions, run as exec inside a claimed sandbox via
# the SDK over the pod network. The driver prints one
# RESULT:<check>:<PASS|FAIL>:<detail> line per check on stdout; the bash layer
# folds those into the tally so it stays the single source of truth.
# ---------------------------------------------------------------------------
echo "--- stage 2: egress assertions inside the sandbox (SDK driver) ---"

driver_out="$(mktemp)"
set +e

# Install the SDK from the CHECKED-OUT commit into a fresh venv (the same pattern
# as husk-e2e.sh), so the e2e always tests THIS commit's SDK.
DRIVER_PY="python3"
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
if [ -d "${REPO_ROOT}/sdk/python" ]; then
  echo "--- installing the checked-out SDK into a fresh venv ---"
  python3 -m venv /tmp/e2e-net-sdk-venv
  /tmp/e2e-net-sdk-venv/bin/pip install --quiet --upgrade pip >/dev/null 2>&1 || true
  /tmp/e2e-net-sdk-venv/bin/pip install --quiet "${REPO_ROOT}/sdk/python"
  DRIVER_PY="/tmp/e2e-net-sdk-venv/bin/python"
fi

INCLUSTER="false"
[ -z "${KUBECONFIG:-}" ] && INCLUSTER="true"
MITOS_NS="$NAMESPACE" MITOS_POOL="$POOL" MITOS_CLAIM="$CLAIM" \
MITOS_INCLUSTER="$INCLUSTER" MITOS_READY_TIMEOUT="$READY_TIMEOUT" \
MITOS_ALLOW_NAME="$ALLOW_NAME" MITOS_DENY_HOST="$DENY_HOST" \
"$DRIVER_PY" - <<'PYEOF' | tee "$driver_out"
import os
import sys

from mitos import AgentRun

NS = os.environ["MITOS_NS"]
POOL = os.environ["MITOS_POOL"]
CLAIM = os.environ["MITOS_CLAIM"]
INCLUSTER = os.environ.get("MITOS_INCLUSTER", "true") == "true"
READY_TIMEOUT = float(os.environ.get("MITOS_READY_TIMEOUT", "180"))
ALLOW_NAME = os.environ["MITOS_ALLOW_NAME"]
DENY_HOST = os.environ["MITOS_DENY_HOST"]


def result(check, ok, detail=""):
    print(f"RESULT:{check}:{'PASS' if ok else 'FAIL'}:{detail}", flush=True)


run = AgentRun(namespace=NS, in_cluster=INCLUSTER)

# Claim a sandbox and wait for Ready before probing egress.
try:
    sb = run.sandbox(pool=POOL, name=CLAIM)
    sb.wait_until_ready(timeout=READY_TIMEOUT)
except Exception as exc:  # noqa: BLE001
    result("claim", False, f"{type(exc).__name__}: {exc}")
    sys.exit(0)
result("claim", True, "sandbox Ready")


def connect_exit(host, port, timeout=5):
    # Probe TCP connectivity from INSIDE the sandbox with Python: the base image
    # (python:3.x) ships python3 but NOT curl, so a curl probe returns exit 127
    # (command-not-found) for EVERY target and silently turns the egress checks
    # into false verdicts. socket.create_connection also performs the DNS resolve,
    # so a name target exercises the in-pod DNS proxy + pin path. A non-zero exit
    # means the connection was blocked/refused/timed out by the in-pod egress
    # filter; 0 means it opened. The timeout bounds a silent default-deny drop.
    cmd = f"""python3 -c 'import socket; socket.create_connection(("{host}", {port}), timeout={timeout}).close()'; echo EXIT:$?"""
    res = sb.exec(cmd, timeout=timeout + 15)
    out = (res.stdout or "")
    for line in out.splitlines():
        if line.startswith("EXIT:"):
            try:
                return int(line.split(":", 1)[1].strip())
            except ValueError:
                return 1
    # No EXIT marker: treat the exec result code as the verdict.
    return res.exit_code if res.exit_code is not None else 1


# 1. Metadata MUST be blocked (the connect must FAIL: non-zero exit).
try:
    rc = connect_exit("169.254.169.254", 80)
    if rc != 0:
        result("metadata-blocked", True, f"connect exit={rc} (blocked, no IAM theft)")
    else:
        result("metadata-blocked", False, "169.254.169.254 REACHABLE from the sandbox (IAM theft possible)")
except Exception as exc:  # noqa: BLE001
    result("metadata-blocked", False, f"{type(exc).__name__}: {exc}")

# 2. A non-allowlisted host MUST be blocked (the connect must FAIL).
try:
    rc = connect_exit(DENY_HOST, 443)
    if rc != 0:
        result("default-deny", True, f"connect exit={rc} (non-allowlisted host blocked)")
    else:
        result("default-deny", False, f"non-allowlisted host {DENY_HOST} REACHABLE (default-deny not enforced)")
except Exception as exc:  # noqa: BLE001
    result("default-deny", False, f"{type(exc).__name__}: {exc}")

# 3. The allowlisted host MUST be reachable (name-based egress via the in-pod DNS
#    proxy + pin, proving the NIC binding and allowlist threading are correct).
try:
    rc = connect_exit(ALLOW_NAME, 443, timeout=10)
    if rc == 0:
        result("allowlist-works", True, f"allowlisted host {ALLOW_NAME} reachable (connect exit=0)")
    else:
        result("allowlist-works", False, f"allowlisted host {ALLOW_NAME} NOT reachable (connect exit={rc})")
except Exception as exc:  # noqa: BLE001
    result("allowlist-works", False, f"{type(exc).__name__}: {exc}")
PYEOF
driver_rc=$?
set -e

# Fold the driver RESULT lines into the bash tally. ALL three egress checks are
# REQUIRED (this is the blocker the maintainer KVM-verifies).
for check in claim metadata-blocked default-deny allowlist-works; do
  line="$(grep "^RESULT:${check}:" "$driver_out" | tail -1 || true)"
  if [ -z "$line" ]; then
    fail "check ${check}: driver produced no result (driver_rc=${driver_rc})"
    continue
  fi
  verdict="$(printf '%s' "$line" | cut -d: -f3)"
  detail="$(printf '%s' "$line" | cut -d: -f4-)"
  if [ "$verdict" = "PASS" ]; then
    pass "check ${check}: ${detail}"
  else
    fail "check ${check}: ${detail}"
  fi
done
rm -f "$driver_out"

# ---------------------------------------------------------------------------
# Verdict.
# ---------------------------------------------------------------------------
echo
echo "=== summary: ${PASS_COUNT} passed, ${FAIL_COUNT} failed ==="
if [ "$FAIL_COUNT" -gt 0 ]; then
  diagnostics
  exit 1
fi
echo "ALL PASS: husk egress isolation enforced (metadata blocked, default-deny, allowlist works)"
