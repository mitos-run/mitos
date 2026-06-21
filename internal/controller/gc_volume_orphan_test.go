package controller_test

// Envtest coverage for the GC volume-orphan sweep. A per-sandbox volume backing
// dir with no backing claim and an age past the grace is reclaimed by a GC pass;
// a backing dir WITH a backing Ready claim (same name) is left alone; and a
// fresh backing (age under the grace) survives so a just-prepared volume whose
// claim status has not landed yet is never reclaimed. This mirrors the VM-orphan
// sweep one-to-one, since a volume backing is keyed by the same sandbox id the
// controller uses for the VM (the claim name).

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1alpha1 "mitos.run/mitos/api/v1alpha1"
	"mitos.run/mitos/internal/controller"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestGCSweepsOrphanVolumes(t *testing.T) {
	stop, engine, _, err := controller.StartFakeForkdNodeRecording(testRegistry, "volgc-node-1", "volgc-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// A claim that reaches Ready: its volume backing (keyed by the claim name)
	// must NOT be reclaimed.
	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "volgc-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "volgc-pool", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "volgc-tmpl"},
			Replicas:    1,
		},
	}
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "volgc-claim", Namespace: "default"},
		Spec:       v1alpha1.SandboxClaimSpec{PoolRef: v1alpha1.LocalObjectReference{Name: "volgc-pool"}},
	}
	for _, obj := range []client.Object{template, pool, claim} {
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, claim)
		_ = k8sClient.Delete(ctx, pool)
		_ = k8sClient.Delete(ctx, template)
	})

	ready := waitClaimReady(t, "volgc-claim")
	backedID := ready.Status.SandboxID
	if backedID == "" {
		t.Fatal("ready claim has empty sandbox id")
	}
	// Seed a volume backing for the Ready claim: it must survive the sweep.
	engine.InjectVolume(backedID, time.Now())

	// Inject an orphan volume (no backing claim) old enough to exceed the grace.
	const orphanID = "vol-orphan-old"
	engine.InjectVolume(orphanID, time.Now().Add(-10*time.Minute))

	// Inject a FRESH orphan volume (no backing claim) under the grace: must survive.
	const freshID = "vol-orphan-fresh"
	engine.InjectVolume(freshID, time.Now())

	gc := &controller.GarbageCollector{
		Client:      k8sClient,
		Registry:    testRegistry,
		OrphanGrace: 60 * time.Second,
	}
	gc.RunOnce(ctx)

	reclaimed := false
	for _, id := range engine.ReclaimedVolumeIDs() {
		if id == orphanID {
			reclaimed = true
		}
		if id == backedID {
			t.Fatalf("GC reclaimed the backed claim's volume %s", backedID)
		}
		if id == freshID {
			t.Fatalf("GC reclaimed a fresh orphan volume %s under the grace", freshID)
		}
	}
	if !reclaimed {
		t.Fatalf("GC did not reclaim orphan volume %s; reclaimed = %v", orphanID, engine.ReclaimedVolumeIDs())
	}

	// And the orphan volume is gone from the live listing while the others remain.
	for _, r := range engine.ListVolumes() {
		if r.SandboxID == orphanID {
			t.Fatalf("orphan volume %s still present after GC sweep", orphanID)
		}
	}
}
