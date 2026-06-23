package controller_test

// Envtest coverage for the GC orphan sweep. A forkd sandbox with no backing
// Ready claim and an uptime past the grace is terminated by a GC pass; a
// sandbox WITH a backing Ready claim is left alone; and a fresh orphan (uptime
// under the grace) survives so a just-forked VM whose claim status has not
// landed yet is never killed.

import (
	v1 "mitos.run/mitos/api/v1"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"mitos.run/mitos/internal/controller"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestGCSweepsOrphanVMs(t *testing.T) {
	stop, engine, _, err := controller.StartFakeForkdNodeRecording(testRegistry, "gc-node-1", "gc1-pool")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// A claim that reaches Ready: its backing VM must NOT be swept.
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "gc1-pool", Namespace: "default"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}
	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "gc1-claim", Namespace: "default"},
		Spec:       v1.SandboxSpec{Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: "gc1-pool"}}},
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

	ready := waitClaimReady(t, "gc1-claim")
	backedID := ready.Status.SandboxID
	if backedID == "" {
		t.Fatal("ready claim has empty sandbox id")
	}

	// Inject an orphan VM (no backing claim) old enough to exceed the grace.
	const orphanID = "orphan-old"
	engine.InjectSandbox(orphanID, time.Now().Add(-10*time.Minute))

	// Inject a FRESH orphan (no backing claim) under the grace: must survive.
	const freshID = "orphan-fresh"
	engine.InjectSandbox(freshID, time.Now())

	gc := &controller.GarbageCollector{
		Client:      k8sClient,
		Registry:    testRegistry,
		OrphanGrace: 60 * time.Second,
	}
	gc.RunOnce(ctx)

	// The old orphan was terminated.
	terminated := false
	for _, id := range engine.TerminatedIDs() {
		if id == orphanID {
			terminated = true
		}
		if id == backedID {
			t.Fatalf("GC terminated the backed claim's sandbox %s", backedID)
		}
		if id == freshID {
			t.Fatalf("GC terminated a fresh orphan %s under the grace", freshID)
		}
	}
	if !terminated {
		t.Fatalf("GC did not terminate orphan %s; terminated = %v", orphanID, engine.TerminatedIDs())
	}

	// And the orphan is gone from the live listing while the others remain.
	for _, r := range engine.ListSandboxes() {
		if r.ID == orphanID {
			t.Fatalf("orphan %s still live after GC sweep", orphanID)
		}
	}
}
