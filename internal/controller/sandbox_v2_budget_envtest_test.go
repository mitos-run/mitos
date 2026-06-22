package controller_test

// Budget-enforcement envtest for the v1alpha2 consolidated Sandbox kind (issue
// #25): when a self-initiated fork (source.fromSandbox = P) is created, the
// controller enforces P's capability budget. Forks are admitted up to
// P.spec.budget.maxForks; the ones beyond are rejected with a typed
// BudgetExhausted condition (the LLM-legible apierr.CodeBudgetExhausted
// remediation), and P.status.budgetSpend.forks records the admitted count.
//
// The gate runs BEFORE fork materialization, so this test needs no working
// forkd node: the admitted children just need to NOT be rejected at the gate,
// and the over-budget child must reach a Failed/Ready=False BudgetExhausted
// condition.

import (
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	v1alpha1 "mitos.run/mitos/api/v1alpha1"
	v1alpha2 "mitos.run/mitos/api/v1alpha2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// sandboxReadyConditionReason returns the Reason of the named Sandbox's Ready
// condition, or "" when the Sandbox or its condition is absent.
func sandboxReadyConditionReason(t *testing.T, name string) string {
	t.Helper()
	var sb v1alpha2.Sandbox
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, &sb); err != nil {
		return ""
	}
	if c := apimeta.FindStatusCondition(sb.Status.Conditions, "Ready"); c != nil {
		return c.Reason
	}
	return ""
}

// TestSandboxForkBudgetAdmitsUpToMaxAndRejectsBeyond proves the controller-side
// capability-budget enforcement (issue #25): a parent P with budget.maxForks=2
// admits the first two fork-children (deterministically ranked by
// creationTimestamp then name) and rejects the third with a BudgetExhausted
// condition, recording P.status.budgetSpend.forks == 2.
func TestSandboxForkBudgetAdmitsUpToMaxAndRejectsBeyond(t *testing.T) {
	maxForks := int32(2)

	parent := &v1alpha2.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "budget-parent", Namespace: "default"},
		Spec: v1alpha2.SandboxSpec{
			Source:   v1alpha2.SandboxSource{PoolRef: &v1alpha1.LocalObjectReference{Name: "budget-pool"}},
			Replicas: 1,
			Budget:   &v1alpha2.SandboxBudget{MaxForks: &maxForks},
		},
	}
	if err := k8sClient.Create(ctx, parent); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, parent) })

	// Three fork-children of the parent, created sequentially so their
	// creationTimestamps order child-0 < child-1 < child-2 (the name tiebreak
	// settles any tie at second granularity).
	names := []string{"budget-child-0", "budget-child-1", "budget-child-2"}
	for _, n := range names {
		child := &v1alpha2.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: "default"},
			Spec: v1alpha2.SandboxSpec{
				Source:   v1alpha2.SandboxSource{FromSandbox: &v1alpha2.FromSandboxSource{Name: "budget-parent"}},
				Replicas: 1,
			},
		}
		if err := k8sClient.Create(ctx, child); err != nil {
			t.Fatal(err)
		}
		c := child
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, c) })
		// Sequential creation with a small gap so the timestamp ordering is stable.
		time.Sleep(1100 * time.Millisecond)
	}

	// child-2 (the over-budget one) must reach a BudgetExhausted Ready=False
	// condition.
	deadline := time.Now().Add(20 * time.Second)
	var rejectedReason string
	for time.Now().Before(deadline) {
		rejectedReason = sandboxReadyConditionReason(t, "budget-child-2")
		if rejectedReason == "BudgetExhausted" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if rejectedReason != "BudgetExhausted" {
		t.Fatalf("budget-child-2 Ready reason = %q, want BudgetExhausted", rejectedReason)
	}

	var rejected v1alpha2.Sandbox
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "budget-child-2", Namespace: "default"}, &rejected); err != nil {
		t.Fatal(err)
	}
	if rejected.Status.Phase != v1alpha1.SandboxFailed {
		t.Fatalf("budget-child-2 phase = %q, want Failed", rejected.Status.Phase)
	}
	if c := apimeta.FindStatusCondition(rejected.Status.Conditions, "Ready"); c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("budget-child-2 Ready condition = %+v, want Status False", c)
	}

	// The two admitted children must NOT be BudgetExhausted (they pass the gate).
	for _, n := range []string{"budget-child-0", "budget-child-1"} {
		if r := sandboxReadyConditionReason(t, n); r == "BudgetExhausted" {
			t.Fatalf("%s was BudgetExhausted but should have been admitted", n)
		}
	}

	// P.status.budgetSpend.forks records the admitted count, capped at the limit.
	deadline = time.Now().Add(20 * time.Second)
	var spend int32 = -1
	for time.Now().Before(deadline) {
		var p v1alpha2.Sandbox
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "budget-parent", Namespace: "default"}, &p); err == nil {
			if p.Status.BudgetSpend != nil {
				spend = p.Status.BudgetSpend.Forks
				if spend == maxForks {
					break
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if spend != maxForks {
		t.Fatalf("budget-parent status.budgetSpend.forks = %d, want %d", spend, maxForks)
	}
}

// ensure client import stays used even if the rest of the file changes shape.
var _ client.Object = (*v1alpha2.Sandbox)(nil)
