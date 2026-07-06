package controller_test

import (
	"fmt"
	v1 "mitos.run/mitos/api/v1"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"mitos.run/mitos/internal/controller"
	"mitos.run/mitos/internal/fork"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const gib = int64(1024 * 1024 * 1024)

// makeCapacityFixture creates an inline-template pool and a Sandbox wired
// together for a capacity-admission test and registers a cleanup. The caller has
// already started a fake forkd node holding the pool's snapshot (keyed by the
// pool name, the inline-template snapshot id).
func makeCapacityFixture(t *testing.T, name string) {
	t.Helper()
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-pool", Namespace: "default"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}
	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-claim", Namespace: "default"},
		Spec: v1.SandboxSpec{
			Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: name + "-pool"}},
		},
	}
	if err := k8sClient.Create(ctx, claim); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, claim)
		_ = k8sClient.Delete(ctx, pool)
	})
}

func getClaim(t *testing.T, name string) v1.Sandbox {
	t.Helper()
	var got v1.Sandbox
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name + "-claim", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

// waitForPhase polls the claim until it reaches want or the deadline; it fails
// the test if the claim reaches a different terminal phase or times out.
func waitForPhase(t *testing.T, name string, want v1.SandboxPhase, timeout time.Duration) v1.Sandbox {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last v1.Sandbox
	for time.Now().Before(deadline) {
		last = getClaim(t, name)
		if last.Status.Phase == want {
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("claim %s phase = %q, want %q (status %+v)", name, last.Status.Phase, want, last.Status)
	return last
}

// TestClaimPendsThenReadyOnFreedCapacity drives the capacity-aware admission
// path: the only node reports a full memory budget, so the claim pends with a
// NoCapacity condition (not Ready, not Failed); freeing the node lets the claim
// place and reach Ready.
func TestClaimPendsThenReadyOnFreedCapacity(t *testing.T) {
	stop, err := controller.StartFakeForkdNode(testRegistry, "cap-node-1", "cap1-pool")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// Make the node read as FULL: a 2 GiB budget already entirely used, so no
	// projected fork cost fits under the (default 1.0) overcommit factor.
	testRegistry.SetNodeMemory("cap-node-1", 2*gib, 2*gib)

	pendingBefore := counterValue(t, "mitos_claim_pending_total", nil)

	makeCapacityFixture(t, "cap1")

	// The claim must pend (not Ready, not Failed) while capacity is exhausted.
	pending := waitForPhase(t, "cap1", v1.SandboxPending, 15*time.Second)
	// The claim can transiently pend with PoolNotFound before its pool is visible
	// to the reconciler cache; wait for the reason to SETTLE to NoCapacity, then
	// re-fetch, so the assertions below never race a transient reason.
	waitForReadyReason(t, "cap1", "NoCapacity")
	pending = getClaim(t, "cap1")
	if got := counterValue(t, "mitos_claim_pending_total", nil); got <= pendingBefore {
		t.Fatalf("claim_pending_total = %v, want > %v", got, pendingBefore)
	}
	cond := meta.FindStatusCondition(pending.Status.Conditions, "Ready")
	if cond == nil || cond.Reason != "NoCapacity" {
		t.Fatalf("Ready condition = %+v, want reason NoCapacity", cond)
	}
	if cond.Status != metav1.ConditionFalse {
		t.Fatalf("NoCapacity Ready condition status = %q, want False", cond.Status)
	}
	if pending.Annotations[capacityPendingSinceKey] == "" {
		t.Fatal("expected capacity-pending-since annotation to be stamped")
	}

	// Free the node: usage drops to 0, so the projected fork now fits.
	testRegistry.SetNodeMemory("cap-node-1", 2*gib, 0)

	ready := waitForPhase(t, "cap1", v1.SandboxReady, 15*time.Second)
	if ready.Status.Node != "cap-node-1" {
		t.Fatalf("ready node = %q, want cap-node-1", ready.Status.Node)
	}
	// The pending stamp is cleared on successful placement.
	if ready.Annotations[capacityPendingSinceKey] != "" {
		t.Fatalf("capacity-pending-since annotation should be cleared after placement, got %q", ready.Annotations[capacityPendingSinceKey])
	}
}

// TestClaimFailsAfterBoundedPendingWait drives the bounded-fail path: a claim
// that has been capacity-pending longer than the max-pending duration fails
// with an actionable capacity-exhaustion message. The wait is simulated by
// backdating the pending-since annotation past the default bound so the test
// does not sleep for minutes.
func TestClaimFailsAfterBoundedPendingWait(t *testing.T) {
	stop, err := controller.StartFakeForkdNode(testRegistry, "cap-node-2", "cap2-pool")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	testRegistry.SetNodeMemory("cap-node-2", 2*gib, 2*gib) // full

	errBefore := counterValue(t, "mitos_claim_errors_total", map[string]string{"pool": "cap2-pool", "reason": "capacity"})

	makeCapacityFixture(t, "cap2")

	// First, confirm it pends and the stamp lands.
	pending := waitForPhase(t, "cap2", v1.SandboxPending, 15*time.Second)
	if pending.Annotations[capacityPendingSinceKey] == "" {
		t.Fatal("expected capacity-pending-since annotation to be stamped")
	}

	// Backdate the pending-since stamp well past the default 5m bound so the
	// next reconcile sees the bounded wait exceeded and fails the claim. A merge
	// patch carries no resourceVersion precondition, so a concurrent reconcile
	// requeue cannot cause an optimistic-lock conflict (a full Update would).
	patch := client.RawPatch(types.MergePatchType, []byte(fmt.Sprintf(
		`{"metadata":{"annotations":{%q:%q}}}`,
		capacityPendingSinceKey,
		time.Now().Add(-10*time.Minute).Format(time.RFC3339),
	)))
	target := &v1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "cap2-claim", Namespace: "default"}}
	if err := k8sClient.Patch(ctx, target, patch); err != nil {
		t.Fatalf("backdate pending annotation: %v", err)
	}

	failed := waitForPhase(t, "cap2", v1.SandboxFailed, 15*time.Second)
	if failed.Status.FinishedAt == nil {
		t.Fatal("failed claim must stamp FinishedAt for GC TTL reaping")
	}
	cond := meta.FindStatusCondition(failed.Status.Conditions, "Ready")
	if cond == nil || cond.Reason != "CapacityExhausted" {
		t.Fatalf("Ready condition = %+v, want reason CapacityExhausted", cond)
	}
	if got := counterValue(t, "mitos_claim_errors_total", map[string]string{"pool": "cap2-pool", "reason": "capacity"}); got <= errBefore {
		t.Fatalf("claim_errors_total{pool=cap2-pool,reason=capacity} = %v, want > %v", got, errBefore)
	}
}

// TestClaimRePendsOnForkdResourceExhausted drives the schedule-time race: the
// node admits the fork (ample memory headroom) and SelectNode picks it, but the
// forkd Fork RPC rejects with ResourceExhausted (the node filled to its
// MaxSandboxes between selection and the RPC, PR #110). The claim must RE-PEND
// with a NoCapacity condition (bounded retry), NOT fail terminally: another
// node, or this one once it drains, can still take the fork.
func TestClaimRePendsOnForkdResourceExhausted(t *testing.T) {
	stop, engine, _, err := controller.StartFakeForkdNodeRecording(testRegistry, "cap-node-3", "cap3-pool")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// The node has memory headroom so admits() selects it; the forkd Fork RPC is
	// what rejects, exactly the race the schedule-time count check cannot close.
	testRegistry.SetNodeMemory("cap-node-3", 16*gib, 0)
	engine.ForkErr = fork.ErrAtCapacity // -> gRPC ResourceExhausted

	makeCapacityFixture(t, "cap3")

	pending := waitForPhase(t, "cap3", v1.SandboxPending, 15*time.Second)
	// The claim can transiently pend with PoolNotFound before its pool is visible
	// to the reconciler cache; wait for the reason to SETTLE to NoCapacity, then
	// re-fetch, so the assertions below never race a transient reason.
	waitForReadyReason(t, "cap3", "NoCapacity")
	pending = getClaim(t, "cap3")
	cond := meta.FindStatusCondition(pending.Status.Conditions, "Ready")
	if cond == nil || cond.Reason != "NoCapacity" {
		t.Fatalf("Ready condition = %+v, want reason NoCapacity (re-pend, not terminal)", cond)
	}
	if cond.Status != metav1.ConditionFalse {
		t.Fatalf("NoCapacity Ready condition status = %q, want False", cond.Status)
	}
	if pending.Status.Phase == v1.SandboxFailed {
		t.Fatal("claim must NOT be Failed on a forkd ResourceExhausted reject")
	}
	// The message must reflect the count-ceiling cause, NOT the memory-overcommit
	// cause (issue #28: accurate, actionable remediation per cause).
	if strings.Contains(cond.Message, "memory capacity") {
		t.Fatalf("count-ceiling re-pend message must not claim memory capacity: %q", cond.Message)
	}
	if !strings.Contains(cond.Message, "sandbox-count") {
		t.Fatalf("count-ceiling re-pend message must name the per-node sandbox-count limit: %q", cond.Message)
	}

	// Clearing the reject lets a later reconcile place the claim and go Ready,
	// proving the re-pend was recoverable (not a dead end).
	engine.ForkErr = nil
	ready := waitForPhase(t, "cap3", v1.SandboxReady, 15*time.Second)
	if ready.Status.Node != "cap-node-3" {
		t.Fatalf("ready node = %q, want cap-node-3", ready.Status.Node)
	}
}

// TestClaimRePendsOnForkdUnavailable drives the node-died-mid-fork race: the
// forkd Fork RPC fails with Unavailable (the node went away between selection
// and the RPC). Like ResourceExhausted, the claim must RE-PEND (NoCapacity), not
// fail terminally, so it retries on a healthy node.
func TestClaimRePendsOnForkdUnavailable(t *testing.T) {
	stop, engine, _, err := controller.StartFakeForkdNodeRecording(testRegistry, "cap-node-4", "cap4-pool")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	testRegistry.SetNodeMemory("cap-node-4", 16*gib, 0)
	engine.ForkErr = status.Error(codes.Unavailable, "node draining")

	makeCapacityFixture(t, "cap4")

	pending := waitForPhase(t, "cap4", v1.SandboxPending, 15*time.Second)
	// The claim can transiently pend with PoolNotFound before its pool is visible
	// to the reconciler cache; wait for the reason to SETTLE to NoCapacity, then
	// re-fetch, so the assertions below never race a transient reason.
	waitForReadyReason(t, "cap4", "NoCapacity")
	pending = getClaim(t, "cap4")
	cond := meta.FindStatusCondition(pending.Status.Conditions, "Ready")
	if cond == nil || cond.Reason != "NoCapacity" {
		t.Fatalf("Ready condition = %+v, want reason NoCapacity (re-pend, not terminal)", cond)
	}
	if pending.Status.Phase == v1.SandboxFailed {
		t.Fatal("claim must NOT be Failed on a forkd Unavailable reject")
	}
	// The message must reflect the node-unreachable cause, NOT the
	// memory-overcommit cause (issue #28).
	if strings.Contains(cond.Message, "memory capacity") {
		t.Fatalf("node-unreachable re-pend message must not claim memory capacity: %q", cond.Message)
	}
	if !strings.Contains(cond.Message, "unreachable") {
		t.Fatalf("node-unreachable re-pend message must name the unreachable node: %q", cond.Message)
	}
}

// capacityPendingSinceKey mirrors the unexported annotation key the reconciler
// stamps; kept in the external test package as a literal so a rename is caught.
const capacityPendingSinceKey = "mitos.run/capacity-pending-since"
