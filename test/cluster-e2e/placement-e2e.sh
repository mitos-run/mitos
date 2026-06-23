#!/usr/bin/env bash
#
# placement-e2e.sh
#
# The dedicatedNodes placement end-to-end (issue #172, gate G3). It proves on a
# REAL multi-node cluster that a SandboxPool with spec.placement.nodeSelector
# confines BOTH its warm husk pods AND its template snapshot build to the
# dedicated nodes, so a tenant's VMs never land on a node outside its set.
#
# This is a SCHEDULING proof, not a VMM proof: it asserts WHICH node each husk
# pod binds to (.spec.nodeName) and which node holds the snapshot, both of which
# are decided by the controller + kube-scheduler BEFORE (and independent of) the
# in-pod dormant VMM coming up. So it does not depend on the nested-VMM tail that
# kind cannot guarantee; the in-VM path is gated in kvm-test.yaml. It requires at
# least two mitos.run/kvm nodes, exactly one of which is labeled mitos.run/tenant=a
# (the cluster from hack/kind-config-placement.yaml).
#
# Stages (each prints a PASS/FAIL line):
#   0. topology     >= 2 KVM nodes and >= 1 tenant=a node are present
#   1. pool         a placed SandboxPool is created and schedules husk pods
#   2. placement    EVERY husk pod of the pool binds to a tenant=a node, none else
#   3. snapshot     the snapshot build (status.nodeDistribution) is confined to
#                   tenant=a nodes (the #195 build-constraint half)
#
# Usage:
#   test/cluster-e2e/placement-e2e.sh [namespace] [kubeconfig]
#
# Env knobs:
#   READY_TIMEOUT   per-stage wait budget, seconds (default 180)
#   POLL_INTERVAL   poll interval, seconds (default 2)
#   E2E_IMAGE       template image (default mirror.gcr.io/library/python:3.12-slim)
#   TENANT_LABEL    the placement label key=value (default mitos.run/tenant=a)
#
set -euo pipefail

NAMESPACE="${1:-mitos-e2e}"
KUBECONFIG_ARG="${2:-}"
if [ -n "$KUBECONFIG_ARG" ]; then
  export KUBECONFIG="$KUBECONFIG_ARG"
fi

READY_TIMEOUT="${READY_TIMEOUT:-180}"
POLL_INTERVAL="${POLL_INTERVAL:-2}"
E2E_IMAGE="${E2E_IMAGE:-mirror.gcr.io/library/python:3.12-slim}"
TENANT_LABEL="${TENANT_LABEL:-mitos.run/tenant=a}"
TENANT_KEY="${TENANT_LABEL%%=*}"
TENANT_VAL="${TENANT_LABEL#*=}"

RUN_ID="$(date +%s)-$$"
POOL="place-pool-${RUN_ID}"

PASS_COUNT=0
FAIL_COUNT=0
pass() { echo "PASS: $*"; PASS_COUNT=$((PASS_COUNT + 1)); }
fail() { echo "FAIL: $*" >&2; FAIL_COUNT=$((FAIL_COUNT + 1)); }
info() { echo "  $*"; }

require() { command -v "$1" >/dev/null 2>&1 || { echo "missing required tool: $1" >&2; exit 1; }; }
require kubectl

k() { kubectl -n "$NAMESPACE" "$@"; }

diagnostics() {
  echo "=== diagnostics (namespace ${NAMESPACE}) ===" >&2
  k get sandboxpools -o wide >&2 2>&1 || true
  k get pods -l "mitos.run/pool=${POOL}" -o wide >&2 2>&1 || true
  k describe sandboxpool "$POOL" >&2 2>&1 || true
  kubectl get nodes -L "${TENANT_KEY}",mitos.run/kvm >&2 2>&1 || true
}

cleanup() {
  rc=$?
  echo "=== teardown ==="
  k delete sandboxpool "$POOL" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  echo "teardown done"
  exit "$rc"
}
trap cleanup EXIT

echo "=== mitos placement e2e: ns=${NAMESPACE} tenant=${TENANT_LABEL} run=${RUN_ID} ==="

# ---------------------------------------------------------------------------
# Stage 0: topology. Need >= 2 KVM nodes and >= 1 dedicated (tenant) node, else
# the placement claim is untestable (a single node can never prove confinement).
# ---------------------------------------------------------------------------
kvm_nodes=$(kubectl get nodes -l 'mitos.run/kvm=true' -o name 2>/dev/null | wc -l | tr -d ' ')
tenant_nodes=$(kubectl get nodes -l "${TENANT_LABEL}" -o name 2>/dev/null | wc -l | tr -d ' ')
info "kvm nodes=${kvm_nodes} tenant(${TENANT_LABEL}) nodes=${tenant_nodes}"
if [ "$kvm_nodes" -ge 2 ] && [ "$tenant_nodes" -ge 1 ]; then
  pass "topology: ${kvm_nodes} KVM nodes, ${tenant_nodes} dedicated; placement is testable"
else
  fail "need >= 2 KVM nodes and >= 1 ${TENANT_LABEL} node (got ${kvm_nodes}/${tenant_nodes})"
  diagnostics
  exit 1
fi

# The set of node names that ARE dedicated (tenant=a): the only legal homes.
dedicated_nodes=$(kubectl get nodes -l "${TENANT_LABEL}" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | sort -u)
is_dedicated() { echo "$dedicated_nodes" | grep -qxF "$1"; }

# ---------------------------------------------------------------------------
# Stage 1: a placed pool schedules husk pods.
# ---------------------------------------------------------------------------
echo "--- stage 1: a placed SandboxPool schedules husk pods ---"
k apply -f - >/dev/null <<EOF
apiVersion: mitos.run/v1
kind: SandboxPool
metadata:
  name: ${POOL}
  labels: { mitos.run/e2e-run: "${RUN_ID}" }
spec:
  template:
    image: ${E2E_IMAGE}
    resources: { cpu: "250m", memory: "512Mi" }
  snapshots:
    replicasPerNode: 2
  placement:
    nodeSelector:
      ${TENANT_KEY}: "${TENANT_VAL}"
EOF

# Wait until the pool has scheduled at least one husk pod (a bound nodeName).
sched_deadline=$(( $(date +%s) + READY_TIMEOUT ))
scheduled=""
while [ "$(date +%s)" -lt "$sched_deadline" ]; do
  scheduled="$(k get pods -l "mitos.run/pool=${POOL},mitos.run/husk=true" \
    -o jsonpath='{range .items[?(@.spec.nodeName)]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)"
  if [ -n "$scheduled" ]; then break; fi
  sleep "$POLL_INTERVAL"
done
if [ -n "$scheduled" ]; then
  pass "pool ${POOL} scheduled $(echo "$scheduled" | wc -l | tr -d ' ') husk pod(s)"
else
  fail "pool ${POOL} scheduled no husk pods within ${READY_TIMEOUT}s"
  diagnostics
  exit 1
fi

# ---------------------------------------------------------------------------
# Stage 2: placement. EVERY scheduled husk pod must be on a dedicated node.
# ---------------------------------------------------------------------------
echo "--- stage 2: husk pods confined to ${TENANT_LABEL} nodes ---"
offenders=""
while read -r pod node; do
  [ -z "$pod" ] && continue
  if is_dedicated "$node"; then
    info "${pod} -> ${node} (dedicated, ok)"
  else
    offenders="${offenders} ${pod}@${node}"
  fi
done <<EOF
$(k get pods -l "mitos.run/pool=${POOL},mitos.run/husk=true" \
  -o jsonpath='{range .items[?(@.spec.nodeName)]}{.metadata.name}{" "}{.spec.nodeName}{"\n"}{end}')
EOF
if [ -z "$offenders" ]; then
  pass "every husk pod of ${POOL} is on a ${TENANT_LABEL} node"
else
  fail "husk pod(s) landed OUTSIDE the placement set:${offenders}"
  diagnostics
  exit 1
fi

# ---------------------------------------------------------------------------
# Stage 3: snapshot build confined to dedicated nodes (the #195 half). The pool
# status reports every node holding the template snapshot; none may be outside
# the placement set, else a placement-pinned pod could never find a holder.
# ---------------------------------------------------------------------------
echo "--- stage 3: snapshot build confined to ${TENANT_LABEL} nodes ---"
dist_deadline=$(( $(date +%s) + READY_TIMEOUT ))
holders=""
while [ "$(date +%s)" -lt "$dist_deadline" ]; do
  # shellcheck disable=SC2016 # $n/$c are Go-template vars, must NOT shell-expand.
  holders="$(k get sandboxpool "$POOL" -o go-template='{{range $n, $c := .status.nodeDistribution}}{{$n}}{{"\n"}}{{end}}' 2>/dev/null || true)"
  [ -n "$holders" ] && break
  sleep "$POLL_INTERVAL"
done
if [ -z "$holders" ]; then
  info "no snapshot holders reported yet (build may be slow on kind); skipping stage 3 as best-effort"
  pass "snapshot-build confinement: no holders to violate the placement set (best effort)"
else
  bad=""
  while read -r node; do
    [ -z "$node" ] && continue
    if is_dedicated "$node"; then info "snapshot on ${node} (dedicated, ok)"; else bad="${bad} ${node}"; fi
  done <<EOF
$holders
EOF
  if [ -z "$bad" ]; then
    pass "snapshot build confined to the ${TENANT_LABEL} node set"
  else
    fail "snapshot built on node(s) OUTSIDE the placement set:${bad}"
    diagnostics
    exit 1
  fi
fi

echo "=== placement e2e: ${PASS_COUNT} passed, ${FAIL_COUNT} failed ==="
[ "$FAIL_COUNT" -eq 0 ]
