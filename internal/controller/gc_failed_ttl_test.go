package controller_test

// Envtest coverage that early-Failed claims are TTL-eligible. The claim
// reconciler's early-failure paths (volume prep, secret resolution, fork) set
// Phase=Failed; they must also stamp Status.FinishedAt so a GC pass can TTL the
// claim instead of leaking it in etcd forever.

import (
	v1 "mitos.run/mitos/api/v1"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"mitos.run/mitos/internal/controller"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// waitClaimFailed polls until the named claim reaches the Failed phase and
// returns it, failing the test if it does not within the window.
func waitClaimFailed(t *testing.T, name string) *v1.Sandbox {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var got v1.Sandbox
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, &got); err == nil {
			if got.Status.Phase == v1.SandboxFailed {
				return &got
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("claim %s did not become Failed within 20s", name)
	return nil
}

func TestGCTTLsEarlyFailedClaim(t *testing.T) {
	stop, _, _, err := controller.StartFakeForkdNodeRecording(testRegistry, "ef-node-1", "ef-pool")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// A claim that references a secret that does not exist: secret resolution
	// fails, driving the reconciler down the early-failure path that sets
	// Phase=Failed. The node has a ready snapshot so selectNode succeeds and the
	// reconciler reaches resolveSecrets.
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "ef-pool", Namespace: "default"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}
	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "ef-claim", Namespace: "default"},
		Spec: v1.SandboxSpec{
			Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: "ef-pool"}},
			Secrets: []v1.SecretMount{{
				Name:      "missing",
				SecretRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "does-not-exist"}, Key: "K"},
			}},
		},
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

	failed := waitClaimFailed(t, "ef-claim")
	if failed.Status.FinishedAt == nil {
		t.Fatal("early-Failed claim has no FinishedAt; it would leak in etcd forever")
	}

	// Backdate FinishedAt well past a short TTL, then a GC pass must delete it.
	var got v1.Sandbox
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "ef-claim", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	ttl := int32(10)
	got.Spec.Lifetime = &v1.SandboxLifetime{TTLSecondsAfterFinished: &ttl}
	if err := k8sClient.Update(ctx, &got); err != nil {
		t.Fatal(err)
	}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "ef-claim", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	old := metav1.NewTime(time.Now().Add(-1 * time.Hour))
	got.Status.FinishedAt = &old
	if err := k8sClient.Status().Update(ctx, &got); err != nil {
		t.Fatal(err)
	}

	gc := &controller.GarbageCollector{Client: k8sClient, Registry: testRegistry}
	gc.RunOnce(ctx)

	waitClaimGone(t, "ef-claim")
}
