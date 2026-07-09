package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "mitos.run/mitos/api/v1"
)

// honestDrainScheme builds the scheme the re-pend unit needs (Sandbox + Pod).
func honestDrainScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

// activeRependFixture builds an active (Ready) husk claim, its backing pod, and a
// pool with the given drain policy, plus a reconciler wired with a fake recorder
// and a checkpointer that captures NOTHING (ok=false), the real-world state today
// since no live-VM checkpoint engine exists.
func activeRependFixture(t *testing.T, drain v1.HuskDrainPolicy) (*SandboxReconciler, *record.FakeRecorder, *v1.Sandbox, *v1.SandboxPool, *corev1.Pod) {
	t.Helper()
	scheme := honestDrainScheme(t)
	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "ckpt-claim", Namespace: "default"},
		Status: v1.SandboxStatus{
			Phase:     v1.SandboxReady,
			Node:      "n1",
			Endpoint:  "10.0.0.1:9091",
			SandboxID: "ckpt-pod",
			Conditions: []metav1.Condition{{
				Type: "Ready", Status: metav1.ConditionTrue, Reason: "HuskActivated",
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
	c := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1.Sandbox{}).
		WithObjects(claim).
		Build()
	rec := record.NewFakeRecorder(16)
	r := &SandboxReconciler{
		Client: c,
		Feed:   NewEmitFeed(rec, nil, nil),
		// A checkpointer that captures nothing: the honest state today.
		Checkpoint: func(context.Context, *v1.Sandbox, *corev1.Pod) (bool, error) {
			return false, nil
		},
	}
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "ckpt-pool", Namespace: "default"},
		Spec:       v1.SandboxPoolSpec{DrainPolicy: drain},
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "ckpt-pod", Namespace: "default"}}
	return r, rec, claim, pool, pod
}

func readyCondition(c *v1.Sandbox) *metav1.Condition {
	for i := range c.Status.Conditions {
		if c.Status.Conditions[i].Type == "Ready" {
			return &c.Status.Conditions[i]
		}
	}
	return nil
}

// drainEvents drains the fake recorder's buffered events without blocking.
func drainEvents(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// TestCheckpointDrainDegradeIsHonest is the issue #374 honesty unit: a pool with
// DrainPolicy Checkpoint whose live-VM checkpoint captures nothing (the only state
// today, no checkpoint engine) must NOT silently degrade to Kill. The re-pend must
// surface the limitation LOUDLY: a distinct CheckpointNotImplemented Ready reason
// AND a Warning event, while still re-pending safely (Kill semantics preserved).
func TestCheckpointDrainDegradeIsHonest(t *testing.T) {
	r, rec, claim, pool, pod := activeRependFixture(t, v1.DrainCheckpoint)
	ctx := context.Background()

	if _, err := r.rependOnHuskPodLost(ctx, claim, pool, pod); err != nil {
		t.Fatalf("rependOnHuskPodLost: %v", err)
	}

	// Kill semantics preserved: re-pended, endpoint/node/sandboxID cleared.
	if claim.Status.Phase != v1.SandboxPending {
		t.Errorf("phase = %s, want Pending (state still re-pends safely)", claim.Status.Phase)
	}
	if claim.Status.Endpoint != "" || claim.Status.Node != "" || claim.Status.SandboxID != "" {
		t.Errorf("re-pended claim still advertises endpoint/node/sandboxID: %+v", claim.Status)
	}

	// Honest, distinct signal: the Ready condition reason is CheckpointNotImplemented,
	// NOT the generic HuskPodLost a Kill pool sets.
	cond := readyCondition(claim)
	if cond == nil {
		t.Fatal("no Ready condition set on re-pend")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("Ready status = %s, want False", cond.Status)
	}
	if cond.Reason != "CheckpointNotImplemented" {
		t.Errorf("Ready reason = %q, want CheckpointNotImplemented (a Checkpoint pool must not masquerade as a successful drain)", cond.Reason)
	}

	// And a loud Warning event so an operator KNOWS, not just a buried message.
	events := drainEvents(rec)
	var found bool
	for _, e := range events {
		if strings.Contains(e, "Warning") && strings.Contains(e, "CheckpointNotImplemented") {
			found = true
		}
	}
	if !found {
		t.Errorf("no Warning CheckpointNotImplemented event recorded; got %v", events)
	}
}

// TestKillDrainUnchanged guards the Kill path: it keeps the HuskPodLost reason and
// emits no CheckpointNotImplemented signal (no false alarm on the default policy).
func TestKillDrainUnchanged(t *testing.T) {
	r, rec, claim, pool, pod := activeRependFixture(t, v1.DrainKill)
	ctx := context.Background()

	if _, err := r.rependOnHuskPodLost(ctx, claim, pool, pod); err != nil {
		t.Fatalf("rependOnHuskPodLost: %v", err)
	}

	if claim.Status.Phase != v1.SandboxPending {
		t.Errorf("phase = %s, want Pending", claim.Status.Phase)
	}
	cond := readyCondition(claim)
	if cond == nil || cond.Reason != "HuskPodLost" {
		t.Fatalf("Kill re-pend reason = %v, want HuskPodLost", cond)
	}
	for _, e := range drainEvents(rec) {
		if strings.Contains(e, "CheckpointNotImplemented") {
			t.Errorf("Kill policy emitted a CheckpointNotImplemented signal: %q", e)
		}
	}
}

// TestKillDrainRecordsADurableRestartSignal is the fix for the silent state loss
// (mitos-run/mitos#870).
//
// A Kill pool re-pends a Ready sandbox onto a replacement dormant slot and
// re-activates it FROM THE POOL TEMPLATE. That is the documented behavior, but the
// tenant's VM is destroyed and replaced: every process, every /tmp write, and the
// whole guest memory is gone. Before this, the only trace was a Ready=False condition
// that the very next reconcile overwrote with Ready=True, so a caller polling the API
// saw an unbroken healthy sandbox and its next run_code silently executed on a fresh
// guest (observed: `x = 12345` then `NameError: name 'x' is not defined`, with no
// exception raised).
//
// The condition is transient by design, so the durable signal has to live in status:
// a Restarts counter plus LastRestartTime, and a Warning event, exactly the way the
// Checkpoint degrade already surfaces itself.
func TestKillDrainRecordsADurableRestartSignal(t *testing.T) {
	r, rec, claim, pool, pod := activeRependFixture(t, v1.DrainKill)
	ctx := context.Background()

	if claim.Status.Restarts != 0 {
		t.Fatalf("fixture starts with Restarts = %d, want 0", claim.Status.Restarts)
	}

	if _, err := r.rependOnHuskPodLost(ctx, claim, pool, pod); err != nil {
		t.Fatalf("rependOnHuskPodLost: %v", err)
	}

	// DURABLE: survives the Ready=True the re-activation writes right after.
	if claim.Status.Restarts != 1 {
		t.Errorf("Restarts = %d after one pod loss, want 1: a caller must be able to detect that its guest was replaced", claim.Status.Restarts)
	}
	if claim.Status.LastRestartTime == nil {
		t.Error("LastRestartTime must be stamped so a caller can tell WHEN its guest was replaced")
	}

	// The message must say the state is GONE, not merely that we are re-pending.
	cond := readyCondition(claim)
	if cond == nil {
		t.Fatal("no Ready condition set on re-pend")
	}
	if cond.Reason != "HuskPodLost" {
		t.Errorf("Ready reason = %q, want HuskPodLost", cond.Reason)
	}
	if !strings.Contains(cond.Message, "in-guest state") {
		t.Errorf("Kill re-pend message must state that in-guest state was lost; got %q", cond.Message)
	}

	// LOUD: a Warning event, the durable operator-visible signal.
	var found bool
	for _, e := range drainEvents(rec) {
		if strings.Contains(e, "Warning") && strings.Contains(e, "SandboxRestarted") {
			found = true
		}
	}
	if !found {
		t.Errorf("no Warning SandboxRestarted event recorded; got %v", drainEvents(rec))
	}

	// A second loss increments again: the counter is monotonic, not a boolean.
	if _, err := r.rependOnHuskPodLost(ctx, claim, pool, pod); err != nil {
		t.Fatalf("second rependOnHuskPodLost: %v", err)
	}
	if claim.Status.Restarts != 2 {
		t.Errorf("Restarts = %d after two pod losses, want 2", claim.Status.Restarts)
	}
}
