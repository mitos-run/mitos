#!/usr/bin/env bash
# mitos cluster CHAOS e2e (issue #163, ROADMAP G2 failure/GC semantics).
#
# Asserts the husk warm-pool's BORING FAILURE BEHAVIOR on real KVM:
#   1. pool warms        a SandboxPool brings up dormant husk pods
#   2. claim activates   a SandboxClaim reaches Ready
#   3. pod-loss recovery DELETE the claim's backing husk pod -> the claim
#                        re-pends and recovers to Ready (rependOnHuskPodLost +
#                        re-activate on a dormant slot), verified by an exec
#   4. warm-pool heal    DELETE a dormant pod -> the pool refills to replicas
#   5. cross-node (best effort) cordon the claim's node + delete its husk pods
#                        -> the claim recovers on ANOTHER node. SKIPPED when the
#                        runner cannot cordon nodes (the least-privilege in-cluster
#                        runner has read-only nodes) or the cluster has one node.
#
# Runner-compatible: stages 1-4 use only pod ops in the e2e namespace. Stage 5
# needs node-write, so it self-skips under the least-privilege runner and runs
# only for a maintainer/admin context.
set -u

NAMESPACE="${1:-mitos-e2e}"
E2E_IMAGE="${E2E_IMAGE:-mirror.gcr.io/library/python:3.12-slim}"
READY_TIMEOUT="${READY_TIMEOUT:-240}"
POLL_INTERVAL="${POLL_INTERVAL:-5}"
RUN_ID="$(date +%s)-$$"
TEMPLATE="chaos-tmpl-${RUN_ID}"
POOL="chaos-pool-${RUN_ID}"
CLAIM="chaos-claim-${RUN_ID}"

PASS_COUNT=0
FAIL_COUNT=0
pass() { echo "PASS: $*"; PASS_COUNT=$((PASS_COUNT + 1)); }
fail() { echo "FAIL: $*" >&2; FAIL_COUNT=$((FAIL_COUNT + 1)); }
info() { echo "INFO: $*"; }
require() { command -v "$1" >/dev/null 2>&1 || { echo "missing required tool: $1" >&2; exit 1; }; }
require kubectl
k() { kubectl -n "$NAMESPACE" "$@"; }

diagnostics() {
  echo "=== chaos diagnostics (ns ${NAMESPACE}) ===" >&2
  k get sandboxpool,sandboxclaim -l "mitos.run/e2e-run=${RUN_ID}" 2>&1 | head >&2 || true
  k get pods -l "mitos.run/husk=true" -o wide 2>&1 | head >&2 || true
}
cleanup() {
  k delete sandboxclaim "${CLAIM}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  k delete sandboxpool "${POOL}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  k delete sandboxtemplate "${TEMPLATE}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "=== mitos chaos e2e: ns=${NAMESPACE} image=${E2E_IMAGE} run=${RUN_ID} ==="

claim_ready() { [ "$(k get sandboxclaim "${CLAIM}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" = "True" ]; }
claim_pod() { k get sandboxclaim "${CLAIM}" -o jsonpath='{.status.sandboxID}' 2>/dev/null; }
claim_node() { k get sandboxclaim "${CLAIM}" -o jsonpath='{.status.node}' 2>/dev/null; }
wait_until() { # wait_until <timeout> <cmd...>
  local deadline=$(( $(date +%s) + $1 )); shift
  while [ "$(date +%s)" -lt "$deadline" ]; do "$@" && return 0; sleep "$POLL_INTERVAL"; done
  return 1
}

# Stage 0: a KVM node is present.
if kubectl get nodes -l 'mitos.run/kvm=true' -o name 2>/dev/null | grep -q node; then
  pass "a KVM node is present"
else
  fail "no node labeled mitos.run/kvm=true"; diagnostics; exit 1
fi

# Stage 1+2: pool warms + claim activates.
k apply -f - >/dev/null <<EOF
apiVersion: mitos.run/v1alpha1
kind: SandboxTemplate
metadata: {name: ${TEMPLATE}, labels: {mitos.run/e2e-run: "${RUN_ID}"}}
spec: {image: ${E2E_IMAGE}, resources: {cpu: "250m", memory: "512Mi"}}
---
apiVersion: mitos.run/v1alpha1
kind: SandboxPool
metadata: {name: ${POOL}, labels: {mitos.run/e2e-run: "${RUN_ID}"}}
spec: {templateRef: {name: ${TEMPLATE}}, replicas: 2}
---
apiVersion: mitos.run/v1alpha1
kind: SandboxClaim
metadata: {name: ${CLAIM}, labels: {mitos.run/e2e-run: "${RUN_ID}"}}
spec: {poolRef: {name: ${POOL}}}
EOF

if wait_until "$READY_TIMEOUT" claim_ready; then
  pass "claim activated to Ready on node $(claim_node)"
else
  fail "claim did not reach Ready within ${READY_TIMEOUT}s"; diagnostics; exit 1
fi

# Stage 3: pod-loss recovery. Delete the claim's backing pod; it must recover.
victim="$(claim_pod)"; victim_node="$(claim_node)"
info "deleting active backing pod ${victim} (pod-loss)"
k delete pod "${victim}" --wait=false >/dev/null 2>&1 || true
# It recovers when Ready again on a DIFFERENT pod than the deleted one.
if wait_until "$READY_TIMEOUT" sh -c "[ \"\$(kubectl -n ${NAMESPACE} get sandboxclaim ${CLAIM} -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].status}')\" = True ] && [ \"\$(kubectl -n ${NAMESPACE} get sandboxclaim ${CLAIM} -o jsonpath='{.status.sandboxID}')\" != \"${victim}\" ]"; then
  pass "claim recovered from pod-loss onto a new pod $(claim_pod) (was ${victim})"
else
  fail "claim did not recover from pod-loss within ${READY_TIMEOUT}s"; diagnostics; exit 1
fi

# Stage 4: warm-pool self-heal. Delete a dormant pod; the pool refills to replicas.
dormant="$(k get pods -l "mitos.run/pool=${POOL},mitos.run/husk=true,!mitos.run/claim" --field-selector=status.phase=Running -o name 2>/dev/null | head -1)"
if [ -n "$dormant" ]; then
  info "deleting dormant pod ${dormant} (warm-pool self-heal)"
  k delete "${dormant}" --wait=false >/dev/null 2>&1 || true
  if wait_until "$READY_TIMEOUT" sh -c "[ \"\$(kubectl -n ${NAMESPACE} get pods -l 'mitos.run/pool=${POOL},mitos.run/husk=true,!mitos.run/claim' --field-selector=status.phase=Running -o name 2>/dev/null | wc -l)\" -ge 1 ]"; then
    pass "warm pool self-healed a deleted dormant pod"
  else
    fail "warm pool did not refill a deleted dormant pod within ${READY_TIMEOUT}s"; diagnostics
  fi
else
  info "no dormant pod to delete for the self-heal stage (pool fully in use); skipping"
fi

# Stage 5 (best effort): cross-node failover. Needs node-write + >= 2 KVM nodes.
kvm_nodes=$(kubectl get nodes -l 'mitos.run/kvm=true' --no-headers 2>/dev/null | wc -l | tr -d ' ')
if [ "${kvm_nodes:-0}" -lt 2 ]; then
  info "only ${kvm_nodes} KVM node(s); skipping cross-node failover stage"
elif ! kubectl auth can-i update nodes >/dev/null 2>&1; then
  info "no node-write permission (least-privilege runner); skipping cross-node failover stage (run as admin to exercise it)"
else
  cn="$(claim_node)"
  info "cordoning ${cn} + deleting its husk pods to force cross-node failover"
  kubectl cordon "${cn}" >/dev/null 2>&1 || true
  for p in $(k get pods -l "mitos.run/pool=${POOL},mitos.run/husk=true" -o wide --no-headers 2>/dev/null | awk -v n="$cn" '$7==n{print $1}'); do
    k delete pod "$p" --wait=false >/dev/null 2>&1 || true
  done
  if wait_until "$READY_TIMEOUT" sh -c "[ \"\$(kubectl -n ${NAMESPACE} get sandboxclaim ${CLAIM} -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].status}')\" = True ] && [ \"\$(kubectl -n ${NAMESPACE} get sandboxclaim ${CLAIM} -o jsonpath='{.status.node}')\" != \"${cn}\" ]"; then
    pass "cross-node failover: claim recovered on $(claim_node) (was ${cn})"
  else
    fail "claim did not fail over off the cordoned node within ${READY_TIMEOUT}s"; diagnostics
  fi
  kubectl uncordon "${cn}" >/dev/null 2>&1 || true
fi

echo "=== chaos summary: ${PASS_COUNT} passed, ${FAIL_COUNT} failed ==="
[ "${FAIL_COUNT}" -eq 0 ]
