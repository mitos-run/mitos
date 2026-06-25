package controller_test

// Envtest coverage for the typed condition the GC stamps when its orphan sweep
// terminates a VM that a still-present claim object once backed. This is the
// re-adopted-orphan case: a claim reached a terminal phase but its backing VM
// lingered (a terminate that crashed or was missed, then the VM re-adopted by a
// restarted forkd). The sweep reaps the lingering VM and, because the terminal
// claim object is still in the API, surfaces a typed OrphanReaped condition on
// it so an operator/SDK can see the GC, not a graceful terminate, removed the VM.

import (
	v1 "mitos.run/mitos/api/v1"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"mitos.run/mitos/internal/controller"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestGCStampsOrphanReapedConditionOnTerminalClaim(t *testing.T) {
	stop, engine, _, err := controller.StartFakeForkdNodeRecording(testRegistry, "orphancond-node-1", "orphancond-pool")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "orphancond-pool", Namespace: "default"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}
	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "orphancond-claim", Namespace: "default"},
		Spec:       v1.SandboxSpec{Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: "orphancond-pool"}}},
	}
	for _, obj := range []client.Object{pool, claim} {
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, claim)
		_ = k8sClient.Delete(ctx, pool)
	})

	ready := waitClaimReady(t, "orphancond-claim")
	backedID := ready.Status.SandboxID
	if backedID == "" {
		t.Fatal("ready claim has empty sandbox id")
	}

	// Drive the claim to a terminal phase while its VM keeps living: the
	// lingering-VM-past-terminal case. A terminal claim is excluded from the
	// liveID net, so its VM is a sweep candidate; the sweep must reap it AND
	// stamp the condition on the still-present claim.
	finished := metav1.NewTime(time.Now().Add(-30 * time.Minute))
	ready.Status.Phase = v1.SandboxFailed
	ready.Status.FinishedAt = &finished
	if err := k8sClient.Status().Update(ctx, ready); err != nil {
		t.Fatal(err)
	}

	// Make the live VM old enough to exceed the orphan grace.
	engine.InjectSandbox(backedID, time.Now().Add(-10*time.Minute))

	gc := &controller.GarbageCollector{
		Client:      k8sClient,
		Registry:    testRegistry,
		OrphanGrace: 60 * time.Second,
		// A long TTL so ttlFinished does not delete the terminal claim before we
		// can read the condition the sweep stamped.
		DefaultTTLSeconds: 3600,
	}
	gc.RunOnce(ctx)

	// The lingering VM was reaped.
	swept := false
	for _, id := range engine.TerminatedIDs() {
		if id == backedID {
			swept = true
		}
	}
	if !swept {
		t.Fatalf("GC did not sweep lingering VM %s; terminated = %v", backedID, engine.TerminatedIDs())
	}

	// The still-present terminal claim carries the typed OrphanReaped condition.
	var got v1.Sandbox
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "orphancond-claim", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	var cond *metav1.Condition
	for i := range got.Status.Conditions {
		if got.Status.Conditions[i].Reason == "OrphanReaped" {
			cond = &got.Status.Conditions[i]
		}
	}
	if cond == nil {
		t.Fatalf("claim missing OrphanReaped condition after sweep; conditions = %+v", got.Status.Conditions)
	}
	if cond.Status != metav1.ConditionFalse {
		t.Fatalf("OrphanReaped condition Status = %q, want False", cond.Status)
	}
	if cond.Message == "" {
		t.Fatal("OrphanReaped condition has empty message; it must carry operator-legible remediation")
	}
}
