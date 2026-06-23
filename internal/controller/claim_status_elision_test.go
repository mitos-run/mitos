package controller

import (
	"context"
	v1 "mitos.run/mitos/api/v1"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestWriteClaimStatusIfChangedElidesNoOp proves the claim-side status-write
// elision (issue #163, status-update rate-limiting under churn): a no-op re-pend
// (status identical to the stored object) does NOT write, so a stuck-pending
// claim re-reconciling every 1-5s does not churn etcd or re-trigger its own
// watch; a real status change writes.
func TestWriteClaimStatusIfChangedElidesNoOp(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"},
		Status: v1.SandboxStatus{
			Phase: v1.SandboxPending,
			Conditions: []metav1.Condition{{
				Type: "Ready", Status: metav1.ConditionFalse, Reason: "NoCapacity",
				Message: "no node had capacity; the claim will retry", LastTransitionTime: metav1.Now(),
			}},
		},
	}
	c := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1.Sandbox{}).
		WithObjects(claim).
		Build()
	r := &SandboxReconciler{Client: c}
	ctx := context.Background()

	var live v1.Sandbox
	if err := c.Get(ctx, client.ObjectKeyFromObject(claim), &live); err != nil {
		t.Fatal(err)
	}
	rv0 := live.ResourceVersion

	// No-op re-pend: the reconciler re-asserts the identical Phase/condition.
	// setCondition carries the LastTransitionTime forward, so the status is
	// byte-identical and the write must be skipped.
	before := live.Status.DeepCopy()
	setCondition(&live.Status.Conditions, metav1.Condition{
		Type: "Ready", Status: metav1.ConditionFalse, Reason: "NoCapacity",
		Message: "no node had capacity; the claim will retry", LastTransitionTime: metav1.Now(),
	})
	if err := r.writeClaimStatusIfChanged(ctx, &live, before); err != nil {
		t.Fatalf("writeClaimStatusIfChanged (no-op): %v", err)
	}
	var afterNoop v1.Sandbox
	if err := c.Get(ctx, client.ObjectKeyFromObject(claim), &afterNoop); err != nil {
		t.Fatal(err)
	}
	if afterNoop.ResourceVersion != rv0 {
		t.Fatalf("no-op re-pend wrote status: resourceVersion %s -> %s", rv0, afterNoop.ResourceVersion)
	}

	// Real change: the condition message moves, so the status must be written.
	before2 := live.Status.DeepCopy()
	setCondition(&live.Status.Conditions, metav1.Condition{
		Type: "Ready", Status: metav1.ConditionFalse, Reason: "NoCapacity",
		Message: "no node had capacity (waited 12s of 30s); the claim will retry", LastTransitionTime: metav1.Now(),
	})
	if err := r.writeClaimStatusIfChanged(ctx, &live, before2); err != nil {
		t.Fatalf("writeClaimStatusIfChanged (change): %v", err)
	}
	var afterChange v1.Sandbox
	if err := c.Get(ctx, client.ObjectKeyFromObject(claim), &afterChange); err != nil {
		t.Fatal(err)
	}
	if afterChange.ResourceVersion == rv0 {
		t.Fatal("a real status change did not write (resourceVersion unchanged)")
	}
}
