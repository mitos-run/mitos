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
