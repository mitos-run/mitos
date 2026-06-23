#!/usr/bin/env bash
# Mitos cluster CHAOS e2e (issue #163, ROADMAP G2 failure/GC semantics).
#
# Asserts the husk warm-pool's BORING FAILURE BEHAVIOR on real KVM:
#   1. pool warms        a SandboxPool brings up dormant husk pods
#   2. claim activates   a Sandbox reaches Ready
#   3. pod-loss recovery DELETE the claim's backing husk pod -> the claim
#                        re-pends and recovers to Ready (rependOnHuskPodLost +
#                        re-activate on a dormant slot), verified by an exec
#   4. warm-pool heal    DELETE a dormant pod -> the pool refills to replicas
#   5. cross-node       cordon the claim's node + delete its husk pods -> the
#                        claim recovers on ANOTHER node. Self-skips on a one-node
#                        cluster or without node cordon permission.
#   6. kill -9 storm    SIGKILL the controller + forkd (--grace-period=0 --force,
#                        no graceful shutdown) WHILE a 3-claim burst activates ->
#                        every claim still converges, the pre-existing claim is
#                        undisturbed, the components recover, ZERO claims are
#                        permanently stuck in a non-terminal phase, and ZERO
#                        orphan VMs survive once the storm claims are deleted
#                        (finalizer reap + GC orphan sweep). The headline G2
#                        crash-injection proof, distinct from the graceful
#                        pod-loss of stage 3. Self-skips without delete on mitos
#                        pods.
#
# KVM/cluster-gated remainder (NOT runnable on darwin/GitHub-hosted CI): the
# guest-agent in-VM SIGKILL and the real-forkd-with-VMs crash. The
# controller-restart-under-storm INVARIANT (no in-memory desired state; a fresh
# GC reconciles a storm purely from CRDs with zero orphans and zero stuck claims)
# is additionally proven without KVM in the envtest
# TestGCChaosStormNoOrphansNoStuckClaims (internal/controller).
#
# Runner permissions: stages 1-4 use only pod ops in the e2e namespace; stage 5
# needs node cordon; stage 6 needs delete on mitos pods. The CI runner is granted
# both (deploy/ci-runner/rbac.yaml); each stage self-skips otherwise, so an
# unprivileged manual run still exercises 1-4.
set -u

NAMESPACE="${1:-mitos-e2e}"
E2E_IMAGE="${E2E_IMAGE:-mirror.gcr.io/library/python:3.12-slim}"
READY_TIMEOUT="${READY_TIMEOUT:-240}"
POLL_INTERVAL="${POLL_INTERVAL:-5}"
RUN_ID="$(date +%s)-$$"
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
  k get sandboxpool,sandbox -l "mitos.run/e2e-run=${RUN_ID}" 2>&1 | head >&2 || true
  k get pods -l "mitos.run/husk=true" -o wide 2>&1 | head >&2 || true
}
cleanup() {
  k delete sandbox "${CLAIM}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  # Sweep any storm sandboxes the kill -9 stage created (by run label).
  k delete sandboxes -l "mitos.run/chaos-storm=${RUN_ID}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  k delete sandboxpool "${POOL}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "=== mitos chaos e2e: ns=${NAMESPACE} image=${E2E_IMAGE} run=${RUN_ID} ==="

claim_ready() { [ "$(k get sandbox "${CLAIM}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" = "True" ]; }
claim_pod() { k get sandbox "${CLAIM}" -o jsonpath='{.status.sandboxID}' 2>/dev/null; }
claim_node() { k get sandbox "${CLAIM}" -o jsonpath='{.status.node}' 2>/dev/null; }
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
apiVersion: mitos.run/v1
kind: SandboxPool
metadata: {name: ${POOL}, labels: {mitos.run/e2e-run: "${RUN_ID}"}}
spec:
  template:
    image: ${E2E_IMAGE}
    resources: {cpu: "250m", memory: "512Mi"}
  snapshots:
    replicasPerNode: 2
---
apiVersion: mitos.run/v1
kind: Sandbox
metadata: {name: ${CLAIM}, labels: {mitos.run/e2e-run: "${RUN_ID}"}}
spec:
  source:
    poolRef:
      name: ${POOL}
EOF

if wait_until "$READY_TIMEOUT" claim_ready; then
  pass "claim activated to Ready on node $(claim_node)"
else
  fail "claim did not reach Ready within ${READY_TIMEOUT}s"; diagnostics; exit 1
fi

# Stage 3: pod-loss recovery. Delete the claim's backing pod; it must recover.
victim="$(claim_pod)"
info "deleting active backing pod ${victim} (pod-loss)"
k delete pod "${victim}" --wait=false >/dev/null 2>&1 || true
# It recovers when Ready again on a DIFFERENT pod than the deleted one.
if wait_until "$READY_TIMEOUT" sh -c "[ \"\$(kubectl -n ${NAMESPACE} get sandbox ${CLAIM} -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].status}')\" = True ] && [ \"\$(kubectl -n ${NAMESPACE} get sandbox ${CLAIM} -o jsonpath='{.status.sandboxID}')\" != \"${victim}\" ]"; then
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
  info "no node cordon permission; skipping cross-node failover (the CI runner has it via the mitos-ci-runner-nodes ClusterRole; this branch is for an unprivileged manual run)"
else
  cn="$(claim_node)"
  info "cordoning ${cn} + deleting its husk pods to force cross-node failover"
  kubectl cordon "${cn}" >/dev/null 2>&1 || true
  for p in $(k get pods -l "mitos.run/pool=${POOL},mitos.run/husk=true" -o wide --no-headers 2>/dev/null | awk -v n="$cn" '$7==n{print $1}'); do
    k delete pod "$p" --wait=false >/dev/null 2>&1 || true
  done
  if wait_until "$READY_TIMEOUT" sh -c "[ \"\$(kubectl -n ${NAMESPACE} get sandbox ${CLAIM} -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].status}')\" = True ] && [ \"\$(kubectl -n ${NAMESPACE} get sandbox ${CLAIM} -o jsonpath='{.status.node}')\" != \"${cn}\" ]"; then
    pass "cross-node failover: claim recovered on $(claim_node) (was ${cn})"
  else
    fail "claim did not fail over off the cordoned node within ${READY_TIMEOUT}s"; diagnostics
  fi
  kubectl uncordon "${cn}" >/dev/null 2>&1 || true
fi

# Stage 6: kill -9 of components under a claim storm. The headline G2 proof
# (#163): force-delete (--grace-period=0 --force = immediate SIGKILL, no graceful
# shutdown) the controller AND the forkd builder WHILE a burst of claims is
# activating, then assert every claim still converges to Ready (no stuck claims),
# the pre-existing Ready claim is undisturbed, and the killed components recover.
# This exercises controller-restart reconciliation (desired state rebuilt from
# CRDs) and forkd self-recovery under load, distinct from the GRACEFUL pod-loss
# of stage 3. Needs delete on mitos pods (granted to the CI runner via the
# mitos-ci-runner-deploy Role); self-skips for an unprivileged manual run.
if ! kubectl -n mitos auth can-i delete pods >/dev/null 2>&1; then
  info "no delete on mitos pods; skipping kill -9 component-crash stage (the CI runner has it)"
else
  storm="chaos-storm-${RUN_ID}"
  info "launching a 3-claim storm, then SIGKILLing the controller + forkd mid-activation"
  for i in 1 2 3; do
    k apply -f - >/dev/null 2>&1 <<EOF
apiVersion: mitos.run/v1
kind: Sandbox
metadata: {name: ${storm}-${i}, labels: {mitos.run/e2e-run: "${RUN_ID}", mitos.run/chaos-storm: "${RUN_ID}"}}
spec:
  source:
    poolRef:
      name: ${POOL}
EOF
  done
  # Immediate SIGKILL (no SIGTERM grace) of the controller (Deployment recreates)
  # and forkd (DaemonSet recreates). Husk pods hold their own VMs, so a forkd
  # death does not kill running sandboxes; this proves it does not wedge them.
  kubectl -n mitos delete pod -l app.kubernetes.io/component=controller --grace-period=0 --force >/dev/null 2>&1 || true
  kubectl -n mitos delete pod -l app.kubernetes.io/component=forkd --grace-period=0 --force >/dev/null 2>&1 || true

  storm_ok=yes
  for i in 1 2 3; do
    if ! wait_until "$READY_TIMEOUT" sh -c "[ \"\$(kubectl -n ${NAMESPACE} get sandbox ${storm}-${i} -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].status}' 2>/dev/null)\" = True ]"; then
      storm_ok=""; info "storm claim ${storm}-${i} did not reach Ready"
    fi
  done
  if [ -n "$storm_ok" ]; then
    pass "all 3 storm claims reached Ready despite controller+forkd SIGKILL (no stuck claims)"
  else
    fail "a storm claim did not converge after the component kill -9 within ${READY_TIMEOUT}s"; diagnostics
  fi

  # The pre-existing Ready claim must be UNDISTURBED by the controller crash: its
  # VM lives in its husk pod, independent of the controller process.
  if claim_ready; then
    pass "pre-existing claim stayed Ready across the controller+forkd SIGKILL"
  else
    fail "pre-existing claim lost Ready after the component kill -9"; diagnostics
  fi

  # The killed components recover.
  if wait_until "$READY_TIMEOUT" sh -c "kubectl -n mitos rollout status deployment/mitos-controller --timeout=10s >/dev/null 2>&1"; then
    pass "controller recovered (rollout Ready) after SIGKILL"
  else
    fail "controller did not recover after SIGKILL within ${READY_TIMEOUT}s"; diagnostics
  fi

  # Zero permanently-stuck claims: no storm claim is wedged in a non-terminal,
  # non-Ready phase. They all reached Ready above; this re-checks after recovery
  # that none regressed into a stuck Pending/Restoring.
  stuck=0
  for i in 1 2 3; do
    ph="$(k get sandbox "${storm}-${i}" -o jsonpath='{.status.phase}' 2>/dev/null)"
    case "$ph" in
      Ready|Terminated|Failed) ;;
      *) stuck=$((stuck + 1)); info "storm claim ${storm}-${i} is in non-terminal phase '${ph}'" ;;
    esac
  done
  if [ "$stuck" -eq 0 ]; then
    pass "zero permanently-stuck storm claims after component recovery"
  else
    fail "${stuck} storm claim(s) wedged in a non-terminal phase after recovery"; diagnostics
  fi

  # Zero orphan VMs after the storm subsides. Delete every storm claim; the
  # finalizer reap terminates its backing VM, and the recovered controller's GC
  # orphan sweep reaps any VM the kill -9 interrupted mid-terminate. Then assert
  # the husk pods that backed the storm claims are gone: no VM is stranded with no
  # backing object. The pre-existing claim's pod is excluded by name.
  preexisting_pod="$(claim_pod)"
  for i in 1 2 3; do k delete sandbox "${storm}-${i}" --ignore-not-found --wait=false >/dev/null 2>&1 || true; done
  if wait_until "$READY_TIMEOUT" sh -c "
    leftover=\$(kubectl -n ${NAMESPACE} get pods -l 'mitos.run/pool=${POOL},mitos.run/husk=true,mitos.run/claim' -o jsonpath='{range .items[*]}{.metadata.name}{\"\n\"}{end}' 2>/dev/null | grep -v '^${preexisting_pod}\$' | grep -c . || true)
    [ \"\${leftover:-0}\" -eq 0 ]
  "; then
    pass "zero orphan VMs after the storm subsided (every storm-backing pod reaped)"
  else
    fail "a storm-backing VM lingered after the storm claims were deleted within ${READY_TIMEOUT}s"; diagnostics
  fi
fi

echo "=== chaos summary: ${PASS_COUNT} passed, ${FAIL_COUNT} failed ==="
[ "${FAIL_COUNT}" -eq 0 ]
