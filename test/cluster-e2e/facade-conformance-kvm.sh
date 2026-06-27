#!/usr/bin/env bash
#
# facade-conformance-kvm.sh
#
# Predicate-level conformance for the agents.x-k8s.io facade (issue #357). The
# object-level facade-conformance kind job proves the facade bridges the upstream
# Sandbox to our run path on kind, but it CANNOT prove the upstream Ready
# predicate ("Pod is Ready; Service Exists") because no Firecracker VMM boots on a
# plain kind runner. This job closes that gap on a KVM-capable runner: it warms a
# real dormant-VMM husk pool, applies the UPSTREAM Sandbox UNCHANGED through the
# facade, and asserts the upstream Sandbox reaches the in-VM Ready predicate on a
# real booted microVM, plus exec-through liveness and the operatingMode resume
# tail.
#
# This is the predicate-level half of docs/facade-conformance.md: it flips the
# basic_test.go Ready predicate and the operatingMode in-VM resume tail from
# NEEDS-BARE-METAL to PROVEN-ON-KVM. Chrome/CDP-specific predicates stay
# NEEDS-BARE-METAL (workload specific, not run here).
#
# Stages (each prints a PASS/FAIL line):
#   0.  a KVM-capable node is present                                 [setup]
#   0b. the upstream agents.x-k8s.io Sandbox CRD is installed          [setup]
#   1.  a warm SandboxPool brings up dormant husk pods (real VMMs)
#   2.  the UPSTREAM Sandbox applies UNCHANGED through the facade
#   3.  the upstream Sandbox reaches Ready=True (the in-VM predicate)
#   4.  our bridged Sandbox reached Ready with a serving Endpoint
#   5.  exec THROUGH the bridged sandbox returns expected stdout (in-VM liveness)
#   6.  operatingMode Suspended releases, Running re-activates to Ready
#       (the in-VM resume tail)
#
# The facade binds an annotation-less upstream Sandbox to its --default-pool, so
# this script creates a WARM pool of that name (FACADE_DEFAULT_POOL, default
# "default") in NAMESPACE and the CI job deploys the facade with the matching
# --default-pool. The upstream manifest itself is applied UNCHANGED (only the
# ${IMAGE} placeholder is substituted on a COPY; the vendored file is never
# edited). The bridged guest image is the POOL's image (the upstream podTemplate
# image is a documented unmapped exception), so the pool image carries the shell
# the exec stage uses.
#
# Usage:
#   test/cluster-e2e/facade-conformance-kvm.sh [namespace] [kubeconfig]
#
# Env knobs:
#   READY_TIMEOUT          per-stage wait budget, seconds (default 300)
#   POLL_INTERVAL          poll interval, seconds (default 2)
#   E2E_IMAGE              pool/guest image (default mirror.gcr.io/library/python:3.12-slim)
#   FACADE_DEFAULT_POOL    pool name matching the facade --default-pool (default "default")
#   SANDBOX_NAME           upstream Sandbox name to apply (default "hello-world")
#   UPSTREAM_MANIFEST      path to the vendored upstream Sandbox example
set -euo pipefail

NAMESPACE="${1:-default}"
KUBECONFIG_ARG="${2:-}"
if [ -n "$KUBECONFIG_ARG" ]; then
  export KUBECONFIG="$KUBECONFIG_ARG"
fi

READY_TIMEOUT="${READY_TIMEOUT:-300}"
POLL_INTERVAL="${POLL_INTERVAL:-2}"
E2E_IMAGE="${E2E_IMAGE:-mirror.gcr.io/library/python:3.12-slim}"
FACADE_DEFAULT_POOL="${FACADE_DEFAULT_POOL:-default}"
SANDBOX_NAME="${SANDBOX_NAME:-hello-world}"
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
UPSTREAM_MANIFEST="${UPSTREAM_MANIFEST:-${REPO_ROOT}/third_party/agent-sandbox/examples/hello-world-sandbox/hello-world.yaml}"

RUN_ID="$(date +%s)-$$"

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

# kubectl in the conformance namespace shorthand.
k() { kubectl -n "$NAMESPACE" "$@"; }

# upstream Sandbox shorthands (the agents.x-k8s.io kind, distinct from our mitos.run kind).
up() { k get sandbox.agents.x-k8s.io "$SANDBOX_NAME" "$@"; }
ours() { k get sandbox.mitos.run "$SANDBOX_NAME" "$@"; }

upstream_ready() {
  up -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true
}

diagnostics() {
  echo "=== facade-conformance-kvm diagnostics (namespace ${NAMESPACE}) ===" >&2
  k get sandbox.agents.x-k8s.io -o wide >&2 2>&1 || true
  k get sandboxpools.mitos.run,sandbox.mitos.run -o wide >&2 2>&1 || true
  k get pods -o wide >&2 2>&1 || true
  echo "--- upstream Sandbox describe ---" >&2
  k describe sandbox.agents.x-k8s.io "$SANDBOX_NAME" >&2 2>&1 || true
  echo "--- bridged Sandbox describe ---" >&2
  k describe sandbox.mitos.run "$SANDBOX_NAME" >&2 2>&1 || true
  echo "--- facade logs ---" >&2
  kubectl -n mitos logs -l app.kubernetes.io/component=facade --tail=80 >&2 2>&1 || true
  for p in $(k get pods -l 'mitos.run/husk=true' -o name 2>/dev/null | head -3); do
    echo "--- logs $p ---" >&2
    k logs "$p" --tail=40 >&2 2>&1 || true
  done
}

cleanup() {
  rc=$?
  echo "=== teardown ==="
  # Delete the upstream Sandbox first (the owner-reference cascade reaps our
  # bridged Sandbox), then the pool. Best effort; never mask the real exit code.
  k delete sandbox.agents.x-k8s.io "$SANDBOX_NAME" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  k delete sandboxpool.mitos.run "$FACADE_DEFAULT_POOL" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  echo "teardown done"
  exit "$rc"
}
trap cleanup EXIT

echo "=== mitos facade predicate-level KVM conformance: ns=${NAMESPACE} pool=${FACADE_DEFAULT_POOL} image=${E2E_IMAGE} run=${RUN_ID} ==="

# ---------------------------------------------------------------------------
# Stage 0: a KVM-capable node is present (the in-VM predicate needs a real VMM).
# A missing KVM node is a SETUP failure, not a conformance gap.
# ---------------------------------------------------------------------------
if kubectl get nodes -l 'mitos.run/kvm=true' -o name 2>/dev/null | grep -q node; then
  pass "a KVM-capable node (mitos.run/kvm=true) is present"
else
  fail "SETUP: no node labeled mitos.run/kvm=true; the predicate-level path cannot boot a VMM"
  diagnostics
  exit 1
fi

# ---------------------------------------------------------------------------
# Stage 0b: the upstream agents.x-k8s.io Sandbox CRD is installed (the CI job
# installs the vendored CRDs unchanged and pins conversion to None). A missing
# CRD is a SETUP failure.
# ---------------------------------------------------------------------------
if kubectl get crd sandboxes.agents.x-k8s.io >/dev/null 2>&1; then
  pass "the upstream agents.x-k8s.io Sandbox CRD is installed"
else
  fail "SETUP: the upstream agents.x-k8s.io Sandbox CRD is not installed"
  diagnostics
  exit 1
fi

# ---------------------------------------------------------------------------
# Stage 1: a warm pool brings up dormant husk pods (real dormant VMMs). The
# facade binds the annotation-less upstream Sandbox to this pool (its
# --default-pool), so it must exist and be warm before the Sandbox is applied.
# ---------------------------------------------------------------------------
echo "--- stage 1: warm pool ${FACADE_DEFAULT_POOL} brings up dormant husk pods ---"
k apply -f - >/dev/null <<EOF
apiVersion: mitos.run/v1
kind: SandboxPool
metadata:
  name: ${FACADE_DEFAULT_POOL}
  labels:
    mitos.run/e2e-run: "${RUN_ID}"
spec:
  template:
    image: ${E2E_IMAGE}
    resources:
      cpu: "250m"
      memory: "512Mi"
  snapshots:
    replicasPerNode: 1
EOF

warm_deadline=$(( $(date +%s) + READY_TIMEOUT ))
warm_ok=""
while [ "$(date +%s)" -lt "$warm_deadline" ]; do
  dormant="$(k get pods -l 'mitos.run/husk=true,!mitos.run/claim' \
    --field-selector=status.phase=Running -o name 2>/dev/null | head -1 || true)"
  if [ -n "$dormant" ]; then warm_ok="yes"; break; fi
  sleep "$POLL_INTERVAL"
done
if [ -n "$warm_ok" ]; then
  pass "pool ${FACADE_DEFAULT_POOL} warmed at least one dormant husk pod"
else
  fail "pool ${FACADE_DEFAULT_POOL} did not warm a dormant husk pod within ${READY_TIMEOUT}s"
  diagnostics
  exit 1
fi

# ---------------------------------------------------------------------------
# Stage 2: the UPSTREAM Sandbox applies UNCHANGED through the facade. Only the
# ${IMAGE} placeholder is substituted on a COPY; the vendored file is not edited.
# ---------------------------------------------------------------------------
echo "--- stage 2: apply the upstream Sandbox UNCHANGED ---"
manifest_copy="$(mktemp)"
cp "$UPSTREAM_MANIFEST" "$manifest_copy"
sed -i.bak "s#\${IMAGE}#${E2E_IMAGE}#g" "$manifest_copy" && rm -f "${manifest_copy}.bak"
echo "=== applied upstream manifest (image placeholder substituted on a copy) ==="
cat "$manifest_copy"
if k apply -f "$manifest_copy" >/dev/null; then
  pass "the upstream agents.x-k8s.io Sandbox applied UNCHANGED"
else
  fail "the upstream Sandbox was rejected (conformance: not admitted by the upstream CRD)"
  rm -f "$manifest_copy"
  diagnostics
  exit 1
fi
rm -f "$manifest_copy"

# ---------------------------------------------------------------------------
# Stage 3: the upstream Sandbox reaches Ready=True. This is the in-VM predicate
# (the "Pod is Ready; Service Exists" analog): the facade mirrors our bridged
# claim's Ready, which goes True only after the husk pod activates and the
# dormant Firecracker VMM boots and the guest agent answers.
# ---------------------------------------------------------------------------
echo "--- stage 3: upstream Sandbox reaches the in-VM Ready predicate ---"
if k wait --for=condition=Ready "sandbox.agents.x-k8s.io/${SANDBOX_NAME}" \
  --timeout="${READY_TIMEOUT}s" >/dev/null 2>&1; then
  pass "upstream Sandbox reached Ready=True on a real booted VMM (the in-VM predicate)"
else
  fail "upstream Sandbox did not reach Ready=True within ${READY_TIMEOUT}s (in-VM predicate not met)"
  diagnostics
  exit 1
fi

# ---------------------------------------------------------------------------
# Stage 4: our bridged Sandbox reached Ready with a serving Endpoint. Independent
# confirmation that the VMM booted and the guest is serving (Ready + a non-empty
# endpoint is set only after husk activation reaches the guest).
# ---------------------------------------------------------------------------
echo "--- stage 4: bridged Sandbox Ready with a serving Endpoint ---"
endpoint="$(ours -o jsonpath='{.status.endpoint}' 2>/dev/null || true)"
ours_ready="$(ours -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)"
if [ "$ours_ready" = "True" ] && [ -n "$endpoint" ]; then
  pass "bridged Sandbox is Ready with endpoint ${endpoint}"
else
  fail "bridged Sandbox Ready='${ours_ready}' endpoint='${endpoint}' (expected Ready=True + non-empty endpoint)"
  diagnostics
  exit 1
fi

# ---------------------------------------------------------------------------
# Stage 5: exec THROUGH the bridged sandbox returns expected stdout. The Python
# SDK attaches to the facade-created bridged Sandbox by name (from_name) and
# reaches its sandbox HTTP API over the pod network; a correct echo proves the
# in-VM workload is live (the running-sandbox liveness predicate). Tests THIS
# commit's SDK from a fresh venv, so the runner image never needs a rebuild.
# ---------------------------------------------------------------------------
echo "--- stage 5: exec through the bridged sandbox (in-VM liveness) ---"
DRIVER_PY="python3"
if [ -d "${REPO_ROOT}/sdk/python" ]; then
  echo "--- installing the checked-out SDK into a fresh venv ---"
  python3 -m venv /tmp/facade-kvm-venv
  /tmp/facade-kvm-venv/bin/pip install --quiet --upgrade pip >/dev/null 2>&1 || true
  /tmp/facade-kvm-venv/bin/pip install --quiet "${REPO_ROOT}/sdk/python"
  DRIVER_PY="/tmp/facade-kvm-venv/bin/python"
fi

INCLUSTER="false"
[ -z "${KUBECONFIG:-}" ] && INCLUSTER="true"

driver_out="$(mktemp)"
set +e
MITOS_NS="$NAMESPACE" MITOS_NAME="$SANDBOX_NAME" \
MITOS_INCLUSTER="$INCLUSTER" MITOS_READY_TIMEOUT="$READY_TIMEOUT" \
"$DRIVER_PY" - <<'PYEOF' | tee "$driver_out"
import os
import sys

from mitos import AgentRun

NS = os.environ["MITOS_NS"]
NAME = os.environ["MITOS_NAME"]
INCLUSTER = os.environ.get("MITOS_INCLUSTER", "true") == "true"
READY_TIMEOUT = float(os.environ.get("MITOS_READY_TIMEOUT", "300"))


def result(ok, detail=""):
    print(f"RESULT:facade-exec:{'PASS' if ok else 'FAIL'}:{detail}", flush=True)


try:
    run = AgentRun(namespace=NS, in_cluster=INCLUSTER)
    # Attach to the bridged Sandbox the facade created from the upstream object.
    sb = run.from_name(NAME)
    sb.wait_until_ready(timeout=READY_TIMEOUT)
    res = sb.exec("echo facade-kvm-ok", timeout=30)
    out = (res.stdout or "").strip()
    if res.exit_code == 0 and out == "facade-kvm-ok":
        result(True, f"exit=0 stdout={out!r}")
    else:
        result(False, f"exit={res.exit_code} stdout={out!r} stderr={res.stderr!r}")
except Exception as exc:  # noqa: BLE001
    result(False, f"{type(exc).__name__}: {exc}")
    sys.exit(0)
PYEOF
driver_rc=$?
set -e

exec_line="$(grep '^RESULT:facade-exec:' "$driver_out" | tail -1 || true)"
rm -f "$driver_out"
if [ -z "$exec_line" ]; then
  fail "exec-through: the SDK driver produced no result (driver_rc=${driver_rc})"
  diagnostics
  exit 1
fi
exec_verdict="$(printf '%s' "$exec_line" | cut -d: -f3)"
exec_detail="$(printf '%s' "$exec_line" | cut -d: -f4-)"
if [ "$exec_verdict" = "PASS" ]; then
  pass "exec through the bridged sandbox: ${exec_detail}"
else
  fail "exec through the bridged sandbox: ${exec_detail}"
  diagnostics
  exit 1
fi

# ---------------------------------------------------------------------------
# Stage 6: operatingMode Suspended releases the run path; Running re-activates it
# to Ready. This is the in-VM resume tail (the bare-metal half of the object-level
# operatingMode resume the kind facade-conformance job asserts).
# ---------------------------------------------------------------------------
echo "--- stage 6: operatingMode Suspended -> Running re-activates to Ready ---"
k patch sandbox.agents.x-k8s.io "$SANDBOX_NAME" --type=merge \
  -p '{"spec":{"operatingMode":"Suspended"}}' >/dev/null

# The bridged Sandbox is released (deleted) on Suspended, or at least the upstream
# Ready leaves True.
susp_deadline=$(( $(date +%s) + READY_TIMEOUT ))
released=""
while [ "$(date +%s)" -lt "$susp_deadline" ]; do
  if ! ours >/dev/null 2>&1; then released="yes"; break; fi
  if [ "$(upstream_ready)" != "True" ]; then released="yes"; break; fi
  sleep "$POLL_INTERVAL"
done
if [ -n "$released" ]; then
  pass "operatingMode=Suspended released the run path (pause)"
else
  fail "operatingMode=Suspended did not release the run path within ${READY_TIMEOUT}s"
  diagnostics
  exit 1
fi

k patch sandbox.agents.x-k8s.io "$SANDBOX_NAME" --type=merge \
  -p '{"spec":{"operatingMode":"Running"}}' >/dev/null
if k wait --for=condition=Ready "sandbox.agents.x-k8s.io/${SANDBOX_NAME}" \
  --timeout="${READY_TIMEOUT}s" >/dev/null 2>&1; then
  pass "operatingMode=Running re-activated to Ready=True (the in-VM resume tail)"
else
  fail "operatingMode=Running did not re-activate to Ready within ${READY_TIMEOUT}s"
  diagnostics
  exit 1
fi

# ---------------------------------------------------------------------------
# Verdict.
# ---------------------------------------------------------------------------
echo
echo "=== summary: ${PASS_COUNT} passed, ${FAIL_COUNT} failed ==="
if [ "$FAIL_COUNT" -gt 0 ]; then
  diagnostics
  exit 1
fi
echo "FACADE-CONFORMANCE-KVM OK: the upstream agents.x-k8s.io Sandbox reaches the in-VM Ready predicate through the facade on a real booted VMM, exec-through is live, and the operatingMode resume tail re-activates"
echo "ALL CHECKS PASSED"
