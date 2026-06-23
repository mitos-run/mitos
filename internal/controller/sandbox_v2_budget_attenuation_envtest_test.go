package controller_test

// Depth-aggregate budget attenuation envtest for the v1alpha2 Sandbox kind
// (issue #25, the deferred core). The earlier sandbox_v2_budget_envtest_test.go
// proves DIRECT fork-children are bounded by the immediate parent's budget. This
// file proves the bound is TRANSITIVE: a fork-of-a-fork (grandchild) cannot
// bypass the ROOT budget, because each level records a STATUS effectiveBudget
// that is the intersection of its own request and the parent's
// effective-remaining, and the next level enforces against THAT.
//
// The gate runs before fork materialization, so no working forkd node is needed:
// an admitted child just needs to NOT be rejected at the gate, and an
// over-budget grandchild must reach a Failed/Ready=False BudgetExhausted
// condition.

import (
	v1 "mitos.run/mitos/api/v1"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// getSandbox fetches a Sandbox by name in the default namespace, or fails.
func getSandbox(t *testing.T, name string) *v1.Sandbox {
	t.Helper()
	var sb v1.Sandbox
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, &sb); err != nil {
		t.Fatalf("get sandbox %q: %v", name, err)
	}
	return &sb
}

// waitForEffectiveMaxForks polls until the named Sandbox's
// status.effectiveBudget.maxForks is set and returns it (or fails on timeout).
func waitForEffectiveMaxForks(t *testing.T, name string) int32 {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var sb v1.Sandbox
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, &sb); err == nil {
			if eb := sb.Status.EffectiveBudget; eb != nil && eb.MaxForks != nil {
				return *eb.MaxForks
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("sandbox %q never got status.effectiveBudget.maxForks", name)
	return -1
}

// waitForReadyReason polls until the named Sandbox's Ready condition reason
// equals want (or fails on timeout), returning the last observed reason.
func waitForReadyReason(t *testing.T, name, want string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		last = sandboxReadyConditionReason(t, name)
		if last == want {
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
	return last
}

// TestSandboxForkBudgetIsDepthAggregate proves a fork-of-a-fork cannot exceed
// the ROOT budget. Root R has maxForks=2. R forks C (admitted, R.spend.forks
// becomes 1). C's effective maxForks must be <= R's remaining (1). C then forks a
// grandchild GC0 (admitted against C's effective 1, C.spend becomes 1) and GC1
// (rejected BudgetExhausted, because C's effective budget is exhausted). So the
// subtree rooted at R holds at most R.maxForks descendants: the depth-aggregate
// bound holds and the grandchild cannot bypass the root.
func TestSandboxForkBudgetIsDepthAggregate(t *testing.T) {
	maxForks := int32(2)

	// Root R: a poolRef sandbox carrying the budget.
	root := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "agg-root", Namespace: "default"},
		Spec: v1.SandboxSpec{
			Source:   v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: "agg-pool"}},
			Replicas: 1,
			Budget:   &v1.SandboxBudget{MaxForks: &maxForks},
		},
	}
	if err := k8sClient.Create(ctx, root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, root) })

	// C: a fork of R. Admitted (R has room for 2).
	child := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "agg-child", Namespace: "default"},
		Spec: v1.SandboxSpec{
			Source:   v1.SandboxSource{FromSandbox: &v1.FromSandboxSource{Name: "agg-root"}},
			Replicas: 1,
		},
	}
	if err := k8sClient.Create(ctx, child); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, child) })

	// C must be admitted (not BudgetExhausted) and gain an effective budget that
	// is no wider than R's remaining. R started at 2 and spent 1 on C, so R's
	// remaining is 1; C's effective maxForks must be <= 1.
	if r := waitForReadyReason(t, "agg-child", "ForksPending"); r == "BudgetExhausted" {
		t.Fatalf("agg-child was BudgetExhausted but should be admitted; reason=%q", r)
	}
	cEff := waitForEffectiveMaxForks(t, "agg-child")
	if cEff > 1 {
		t.Fatalf("agg-child effective maxForks = %d, must be <= R remaining (1): a fork-child must never hold a budget wider than the parent has left", cEff)
	}

	// Two grandchildren GC0, GC1: forks of C, created sequentially so their
	// creationTimestamps order GC0 < GC1.
	gcNames := []string{"agg-grandchild-0", "agg-grandchild-1"}
	for _, n := range gcNames {
		gc := &v1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: "default"},
			Spec: v1.SandboxSpec{
				Source:   v1.SandboxSource{FromSandbox: &v1.FromSandboxSource{Name: "agg-child"}},
				Replicas: 1,
			},
		}
		if err := k8sClient.Create(ctx, gc); err != nil {
			t.Fatal(err)
		}
		g := gc
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, g) })
		time.Sleep(1100 * time.Millisecond)
	}

	// GC1 (the second grandchild) is over C's effective budget (C effective
	// maxForks is 1, GC0 took it) and must be rejected BudgetExhausted. This is
	// the load-bearing claim: the grandchild is bounded by the ROOT through C's
	// attenuated effective budget, so it cannot bypass R.maxForks.
	if r := waitForReadyReason(t, "agg-grandchild-1", "BudgetExhausted"); r != "BudgetExhausted" {
		t.Fatalf("agg-grandchild-1 Ready reason = %q, want BudgetExhausted (a fork-of-a-fork must not bypass the root budget)", r)
	}
	gc1 := getSandbox(t, "agg-grandchild-1")
	if gc1.Status.Phase != v1.SandboxFailed {
		t.Fatalf("agg-grandchild-1 phase = %q, want Failed", gc1.Status.Phase)
	}
	if c := apimeta.FindStatusCondition(gc1.Status.Conditions, "Ready"); c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("agg-grandchild-1 Ready condition = %+v, want Status False", c)
	}

	// GC0 must NOT be BudgetExhausted (it took C's one remaining fork).
	if r := sandboxReadyConditionReason(t, "agg-grandchild-0"); r == "BudgetExhausted" {
		t.Fatalf("agg-grandchild-0 was BudgetExhausted but should have been admitted")
	}
}

// TestSandboxEffectiveBudgetNeverWidens proves a child's status.effectiveBudget
// is never wider than the parent's effective-remaining on any dimension, even
// when the child's spec requests a WIDER budget than the parent has left. Root R
// has maxForks=2; child C requests maxForks=100. After R spends 1 on C, R's
// remaining is 1, so C's effective maxForks must be clamped to 1, never 100.
func TestSandboxEffectiveBudgetNeverWidens(t *testing.T) {
	rootMax := int32(2)
	childRequest := int32(100)

	root := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "widen-root", Namespace: "default"},
		Spec: v1.SandboxSpec{
			Source:   v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: "widen-pool"}},
			Replicas: 1,
			Budget:   &v1.SandboxBudget{MaxForks: &rootMax},
		},
	}
	if err := k8sClient.Create(ctx, root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, root) })

	child := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "widen-child", Namespace: "default"},
		Spec: v1.SandboxSpec{
			Source:   v1.SandboxSource{FromSandbox: &v1.FromSandboxSource{Name: "widen-root"}},
			Replicas: 1,
			// The child asks for a far wider budget than the root will have left.
			Budget: &v1.SandboxBudget{MaxForks: &childRequest},
		},
	}
	if err := k8sClient.Create(ctx, child); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, child) })

	cEff := waitForEffectiveMaxForks(t, "widen-child")
	// R remaining after spending 1 on C is 1; the child's wider request (100) must
	// be clamped down to at most 1.
	if cEff > 1 {
		t.Fatalf("widen-child effective maxForks = %d, must be clamped to R remaining (<=1): a wider child request must never widen the parent budget", cEff)
	}
}
