#!/usr/bin/env bash
set -euo pipefail

# End-to-end test using kind and mock mode.
# Tests the full CRD lifecycle: Pool -> Sandbox (pool claim, sandbox fork)
#
# Prerequisites: kind, kubectl
# No KVM required; runs in mock mode.

CLUSTER_NAME="${CLUSTER_NAME:-sandbox-e2e}"
PASSED=0
FAILED=0

log() { echo "==> $*"; }
pass() { echo "  PASS: $*"; PASSED=$((PASSED + 1)); }
fail() { echo "  FAIL: $*"; FAILED=$((FAILED + 1)); }

cleanup() {
    log "Cleaning up..."
    kind delete cluster --name "$CLUSTER_NAME" 2>/dev/null || true
}
trap cleanup EXIT

# --- Setup ---
log "Creating kind cluster: $CLUSTER_NAME"
cat <<EOF | kind create cluster --name "$CLUSTER_NAME" --config=- --wait 60s
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
    labels:
      mitos.run/kvm: "true"
EOF

log "Installing CRDs"
kubectl apply -f deploy/crds/

# Wait for CRDs to be established
sleep 3

# --- Test 1: CRDs are installed ---
log "Test 1: CRDs installed"
for crd in sandboxpools sandboxes workspaces workspacerevisions; do
    if kubectl get crd "${crd}.mitos.run" &>/dev/null; then
        pass "CRD ${crd}.mitos.run exists"
    else
        fail "CRD ${crd}.mitos.run not found"
    fi
done

# --- Test 2: Create SandboxPool with inline template ---
log "Test 2: Create SandboxPool with inline template"
cat <<EOF | kubectl apply -f -
apiVersion: mitos.run/v1
kind: SandboxPool
metadata:
  name: test-pool
spec:
  template:
    image: python:3.12-slim
    init:
      - "echo ready"
    resources:
      cpu: "1"
      memory: "512Mi"
    volumes:
      - name: workspace
        mountPath: /workspace
        size: 1Gi
        forkPolicy: Snapshot
      - name: scratch
        mountPath: /tmp
        size: 512Mi
        forkPolicy: Fresh
  snapshots:
    replicasPerNode: 3
    snapshotAfter: Ready
    scaleDownAfterSnapshot: true
EOF

if kubectl get sandboxpool test-pool -o name &>/dev/null; then
    pass "SandboxPool created"
else
    fail "SandboxPool creation failed"
fi

# Verify pool spec
REPLICAS=$(kubectl get sandboxpool test-pool -o jsonpath='{.spec.snapshots.replicasPerNode}')
if [ "$REPLICAS" = "3" ]; then
    pass "Pool replicasPerNode = 3"
else
    fail "Pool replicasPerNode expected 3, got $REPLICAS"
fi

# --- Test 3: Create Sandbox from pool ---
log "Test 3: Create Sandbox from pool"
cat <<EOF | kubectl apply -f -
apiVersion: mitos.run/v1
kind: Sandbox
metadata:
  name: test-sandbox
spec:
  source:
    poolRef:
      name: test-pool
  env:
    - name: SESSION_ID
      value: "e2e-test"
  lifetime:
    ttl: 10m
EOF

if kubectl get sandbox test-sandbox -o name &>/dev/null; then
    pass "Sandbox created"
else
    fail "Sandbox creation failed"
fi

# Verify sandbox spec
POOL_REF=$(kubectl get sandbox test-sandbox -o jsonpath='{.spec.source.poolRef.name}')
if [ "$POOL_REF" = "test-pool" ]; then
    pass "Sandbox references correct pool"
else
    fail "Sandbox pool ref expected test-pool, got $POOL_REF"
fi

# --- Test 4: Create Sandbox fork ---
log "Test 4: Create Sandbox fork"
cat <<EOF | kubectl apply -f -
apiVersion: mitos.run/v1
kind: Sandbox
metadata:
  name: test-fork
spec:
  source:
    fromSandbox:
      name: test-sandbox
  replicas: 2
EOF

if kubectl get sandbox test-fork -o name &>/dev/null; then
    pass "Sandbox fork created"
else
    fail "Sandbox fork creation failed"
fi

FORK_REPLICAS=$(kubectl get sandbox test-fork -o jsonpath='{.spec.replicas}')
if [ "$FORK_REPLICAS" = "2" ]; then
    pass "Fork replicas = 2"
else
    fail "Fork replicas expected 2, got $FORK_REPLICAS"
fi

# --- Test 5: Verify volume fork policies in pool template ---
log "Test 5: Volume fork policies"
WS_POLICY=$(kubectl get sandboxpool test-pool -o jsonpath='{.spec.template.volumes[0].forkPolicy}')
SCRATCH_POLICY=$(kubectl get sandboxpool test-pool -o jsonpath='{.spec.template.volumes[1].forkPolicy}')

if [ "$WS_POLICY" = "Snapshot" ]; then
    pass "Workspace volume forkPolicy = Snapshot"
else
    fail "Workspace forkPolicy expected Snapshot, got $WS_POLICY"
fi

if [ "$SCRATCH_POLICY" = "Fresh" ]; then
    pass "Scratch volume forkPolicy = Fresh"
else
    fail "Scratch forkPolicy expected Fresh, got $SCRATCH_POLICY"
fi

# --- Test 6: Cleanup ---
log "Test 6: Resource deletion"
kubectl delete sandbox test-fork
kubectl delete sandbox test-sandbox
kubectl delete sandboxpool test-pool

for resource in sandbox/test-fork sandbox/test-sandbox sandboxpool/test-pool; do
    if kubectl get "$resource" &>/dev/null 2>&1; then
        fail "Resource $resource still exists after deletion"
    else
        pass "Resource $resource deleted"
    fi
done

# --- Summary ---
echo ""
echo "================================"
echo "  Results: $PASSED passed, $FAILED failed"
echo "================================"

if [ "$FAILED" -gt 0 ]; then
    exit 1
fi
