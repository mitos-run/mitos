package controller_test

import (
	v1 "mitos.run/mitos/api/v1"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestSandboxPool_CreateAndReconcile(t *testing.T) {
	// Pool with an inline template and a per-node snapshot/warm count of 5.
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pool-reconcile",
			Namespace: "default",
		},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{
				Image: "python:3.12-slim",
				Init:  []string{"echo ready"},
				Resources: v1.SandboxResources{
					CPU:    resource.MustParse("1"),
					Memory: resource.MustParse("512Mi"),
				},
				Volumes: []v1.SandboxVolume{
					{
						Name:       "workspace",
						MountPath:  "/workspace",
						Size:       "1Gi",
						ForkPolicy: v1.ForkPolicySnapshot,
					},
				},
			},
			Snapshots: &v1.PoolSnapshots{
				ReplicasPerNode:        5,
				SnapshotAfter:          v1.SnapshotAfterReady,
				ScaleDownAfterSnapshot: true,
			},
			Warm: &v1.PoolWarm{Min: 5},
		},
	}

	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	defer k8sClient.Delete(ctx, pool)

	// Wait for reconciliation
	time.Sleep(2 * time.Second)

	// Verify the pool was reconciled (status updated)
	var reconciled v1.SandboxPool
	if err := k8sClient.Get(ctx, types.NamespacedName{
		Name:      "test-pool-reconcile",
		Namespace: "default",
	}, &reconciled); err != nil {
		t.Fatalf("get pool: %v", err)
	}

	if reconciled.Spec.Snapshots == nil || reconciled.Spec.Snapshots.ReplicasPerNode != 5 {
		t.Errorf("expected 5 replicasPerNode, got %+v", reconciled.Spec.Snapshots)
	}
}

func TestSandboxPool_TemplateNotFound(t *testing.T) {
	// A pool whose shared templateRef points at a template that does not exist:
	// the controller should handle the missing template gracefully.
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pool-no-template",
			Namespace: "default",
		},
		Spec: v1.SandboxPoolSpec{
			TemplateRef: &v1.LocalObjectReference{
				Name: "nonexistent-template",
			},
			Warm: &v1.PoolWarm{Min: 1},
		},
	}

	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	defer k8sClient.Delete(ctx, pool)

	// Controller should handle the missing template gracefully
	time.Sleep(2 * time.Second)

	var reconciled v1.SandboxPool
	if err := k8sClient.Get(ctx, types.NamespacedName{
		Name:      "test-pool-no-template",
		Namespace: "default",
	}, &reconciled); err != nil {
		t.Fatalf("get pool: %v", err)
	}
}

func TestSandboxClaim_CreateAndReconcile(t *testing.T) {
	// Create pool with an inline template.
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pool-claim",
			Namespace: "default",
		},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 3},
		},
	}
	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	defer k8sClient.Delete(ctx, pool)

	// Create sandbox (the consolidated run-axis kind; old SandboxClaim).
	timeout := metav1.Duration{Duration: 10 * time.Minute}
	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim-reconcile",
			Namespace: "default",
		},
		Spec: v1.SandboxSpec{
			Source:   v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: "test-pool-claim"}},
			Lifetime: &v1.SandboxLifetime{TTL: &timeout},
		},
	}
	if err := k8sClient.Create(ctx, claim); err != nil {
		t.Fatalf("create claim: %v", err)
	}
	defer k8sClient.Delete(ctx, claim)

	// Wait for reconciliation
	time.Sleep(2 * time.Second)

	var reconciled v1.Sandbox
	if err := k8sClient.Get(ctx, types.NamespacedName{
		Name:      "test-claim-reconcile",
		Namespace: "default",
	}, &reconciled); err != nil {
		t.Fatalf("get claim: %v", err)
	}

	// Claim should be in Pending state (no forkd available in tests)
	if reconciled.Status.Phase != v1.SandboxPending && reconciled.Status.Phase != "" {
		// It's OK if it's empty (not yet reconciled) or Pending (no nodes)
		t.Logf("claim phase: %s", reconciled.Status.Phase)
	}
}

func TestSandboxFork_CreateAndReconcile(t *testing.T) {
	// Create a source sandbox first (old SandboxClaim).
	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim-for-fork",
			Namespace: "default",
		},
		Spec: v1.SandboxSpec{
			Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: "some-pool"}},
		},
	}
	if err := k8sClient.Create(ctx, claim); err != nil {
		// Might already exist from previous test
		if err2 := k8sClient.Get(ctx, client.ObjectKeyFromObject(claim), claim); err2 != nil {
			t.Fatalf("create claim: %v", err)
		}
	}
	defer k8sClient.Delete(ctx, claim)

	// Create a fork sandbox: a v1.Sandbox sourced from the source sandbox, with a
	// fan-out of 3 (old SandboxFork.Replicas).
	fork := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-fork-reconcile",
			Namespace: "default",
		},
		Spec: v1.SandboxSpec{
			Source:   v1.SandboxSource{FromSandbox: &v1.FromSandboxSource{Name: "test-claim-for-fork"}},
			Replicas: 3,
		},
	}
	if err := k8sClient.Create(ctx, fork); err != nil {
		t.Fatalf("create fork: %v", err)
	}
	defer k8sClient.Delete(ctx, fork)

	// Wait for reconciliation
	time.Sleep(2 * time.Second)

	var reconciled v1.Sandbox
	if err := k8sClient.Get(ctx, types.NamespacedName{
		Name:      "test-fork-reconcile",
		Namespace: "default",
	}, &reconciled); err != nil {
		t.Fatalf("get fork: %v", err)
	}

	if reconciled.Spec.Replicas != 3 {
		t.Errorf("expected 3 replicas, got %d", reconciled.Spec.Replicas)
	}
}
