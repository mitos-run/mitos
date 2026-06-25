package controller_test

import (
	v1 "mitos.run/mitos/api/v1"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"mitos.run/mitos/internal/controller"
)

func TestClaimReachesReadyEndToEnd(t *testing.T) {
	stop, err := controller.StartFakeForkdNode(testRegistry, "e2e-node-1", "e2e-pool")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-pool", Namespace: "default"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}
	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-claim", Namespace: "default"},
		Spec: v1.SandboxSpec{
			Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: "e2e-pool"}},
		},
	}
	if err := k8sClient.Create(ctx, claim); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, claim)
		_ = k8sClient.Delete(ctx, pool)
	})

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var got v1.Sandbox
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "e2e-claim", Namespace: "default"}, &got); err == nil {
			if got.Status.Phase == v1.SandboxReady {
				if got.Status.Endpoint == "" {
					t.Fatal("ready claim has empty endpoint")
				}
				if got.Status.SandboxID == "" {
					t.Fatal("ready claim has empty sandboxID")
				}
				if got.Status.Node != "e2e-node-1" {
					t.Fatalf("node = %q, want e2e-node-1", got.Status.Node)
				}
				return
			}
			if got.Status.Phase == v1.SandboxFailed {
				t.Fatalf("claim failed: %+v", got.Status)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("claim did not become Ready within 15s")
}
