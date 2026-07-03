package controller_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/controller"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// poolMissingSinceKey mirrors the unexported annotation key the reconciler
// stamps when it first observes the referenced pool missing; kept in the
// external test package as a literal so a rename is caught.
const poolMissingSinceKey = "mitos.run/pool-missing-since"

// makeOrphanClaim creates a Sandbox whose poolRef names a pool that does NOT
// exist (the caller decides whether to create it later) and registers a
// cleanup. Names follow the <name>-claim / <name>-pool convention so the
// shared getClaim/waitForPhase helpers work.
func makeOrphanClaim(t *testing.T, name string) {
	t.Helper()
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
	})
}

// TestClaimFailsTerminallyWhenPoolNeverAppears drives the boring-failure path
// for a Sandbox referencing a nonexistent pool (issue #630): the claim pends
// with an actionable PoolNotFound condition while inside the bounded grace
// period, and once the grace period is exceeded (simulated by backdating the
// pool-missing-since annotation, the same technique as the CapacityExhausted
// test) it fails TERMINALLY: phase Failed, FinishedAt stamped so the GC TTL
// pass reaps it like every other Failed sandbox, and no further steady-state
// requeues. Previously this reconcile errored forever (~15m backoff, no
// terminal phase, no condition a client could act on).
func TestClaimFailsTerminallyWhenPoolNeverAppears(t *testing.T) {
	errBefore := counterValue(t, "mitos_claim_errors_total", map[string]string{"pool": "orphan1-pool", "reason": "pool"})

	makeOrphanClaim(t, "orphan1")

	// Inside the grace period: Pending (not Failed) with an actionable
	// PoolNotFound condition and the missing-since stamp.
	pending := waitForPhase(t, "orphan1", v1.SandboxPending, 15*time.Second)
	cond := meta.FindStatusCondition(pending.Status.Conditions, "Ready")
	if cond == nil || cond.Reason != "PoolNotFound" {
		t.Fatalf("Ready condition = %+v, want reason PoolNotFound", cond)
	}
	if cond.Status != metav1.ConditionFalse {
		t.Fatalf("PoolNotFound Ready condition status = %q, want False", cond.Status)
	}
	if !strings.Contains(cond.Message, "orphan1-pool") {
		t.Fatalf("PoolNotFound message must name the missing pool: %q", cond.Message)
	}
	if pending.Annotations[poolMissingSinceKey] == "" {
		t.Fatal("expected pool-missing-since annotation to be stamped")
	}

	// Backdate the missing-since stamp past the default 5m bound so the next
	// reconcile sees the grace period exceeded and fails the claim terminally. A
	// merge patch carries no resourceVersion precondition, so a concurrent
	// reconcile requeue cannot cause an optimistic-lock conflict.
	patch := client.RawPatch(types.MergePatchType, []byte(fmt.Sprintf(
		`{"metadata":{"annotations":{%q:%q}}}`,
		poolMissingSinceKey,
		time.Now().Add(-10*time.Minute).Format(time.RFC3339),
	)))
	target := &v1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "orphan1-claim", Namespace: "default"}}
	if err := k8sClient.Patch(ctx, target, patch); err != nil {
		t.Fatalf("backdate pool-missing annotation: %v", err)
	}

	failed := waitForPhase(t, "orphan1", v1.SandboxFailed, 15*time.Second)
	if failed.Status.FinishedAt == nil {
		t.Fatal("failed claim must stamp FinishedAt for GC TTL reaping")
	}
	cond = meta.FindStatusCondition(failed.Status.Conditions, "Ready")
	if cond == nil || cond.Reason != "PoolNotFound" {
		t.Fatalf("Ready condition = %+v, want reason PoolNotFound", cond)
	}
	// The message must be actionable (issue #28: LLM-legible errors): name the
	// missing pool and carry the create-or-delete remediation.
	if !strings.Contains(cond.Message, "orphan1-pool") {
		t.Fatalf("terminal PoolNotFound message must name the missing pool: %q", cond.Message)
	}
	if !strings.Contains(cond.Message, "create the pool") || !strings.Contains(cond.Message, "delete this sandbox") {
		t.Fatalf("terminal PoolNotFound message must carry the create-or-delete remediation: %q", cond.Message)
	}
	if got := counterValue(t, "mitos_claim_errors_total", map[string]string{"pool": "orphan1-pool", "reason": "pool"}); got <= errBefore {
		t.Fatalf("claim_errors_total{pool=orphan1-pool,reason=pool} = %v, want > %v", got, errBefore)
	}

	// Terminal means terminal: the claim settles in Failed (the reconciler's
	// terminal early-return, no steady-state requeue keeps mutating it).
	time.Sleep(2 * time.Second)
	still := getClaim(t, "orphan1")
	if still.Status.Phase != v1.SandboxFailed {
		t.Fatalf("claim left the terminal Failed phase: %q", still.Status.Phase)
	}
}

// TestClaimProceedsWhenPoolAppearsWithinGrace proves the grace period is not a
// dead end: a Sandbox created BEFORE its pool (a manifest ordering race) pends
// with PoolNotFound, and once the pool appears within the grace period the
// claim clears the missing-since stamp and proceeds to Ready on the normal
// placement path.
func TestClaimProceedsWhenPoolAppearsWithinGrace(t *testing.T) {
	stop, err := controller.StartFakeForkdNode(testRegistry, "pnf-node-1", "late1-pool")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	testRegistry.SetNodeMemory("pnf-node-1", 16*gib, 0) // ample headroom

	makeOrphanClaim(t, "late1")

	pending := waitForPhase(t, "late1", v1.SandboxPending, 15*time.Second)
	cond := meta.FindStatusCondition(pending.Status.Conditions, "Ready")
	if cond == nil || cond.Reason != "PoolNotFound" {
		t.Fatalf("Ready condition = %+v, want reason PoolNotFound", cond)
	}
	if pending.Annotations[poolMissingSinceKey] == "" {
		t.Fatal("expected pool-missing-since annotation to be stamped")
	}

	// The pool arrives within the grace period.
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "late1-pool", Namespace: "default"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}
	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, pool) })

	ready := waitForPhase(t, "late1", v1.SandboxReady, 30*time.Second)
	if ready.Status.Node != "pnf-node-1" {
		t.Fatalf("ready node = %q, want pnf-node-1", ready.Status.Node)
	}
	// The missing-since stamp is cleared once the pool exists, so a later pool
	// deletion starts a fresh grace clock.
	if ready.Annotations[poolMissingSinceKey] != "" {
		t.Fatalf("pool-missing-since annotation should be cleared once the pool exists, got %q", ready.Annotations[poolMissingSinceKey])
	}
}
