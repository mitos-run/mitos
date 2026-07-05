package controller_test

// Envtest coverage for issue #688 at the object level: a husk-backed claim
// whose TTL (Spec.Lifetime.TTL, maxLifetime) expires must reach Terminated
// AND its claimed husk pod must actually be deleted, not merely orphaned
// Running while the claim reads terminal. terminateLifetime and
// reapClaimHuskPods (internal/controller/sandboxclaim_controller.go) are
// unit and regression tested in isolation; this test drives the real husk
// pool reconciler and the suite's husk-mode claim reconciler (scoped via
// HuskTestClaimLabel) end to end, modeled on the setup sequence in
// husk_nodeloss_failover_envtest_test.go: stand up one warm husk pod, force
// it Running/Ready (envtest has no scheduler or kubelet), let the claim
// activate it via the stubbed activator, then let its short TTL fire and
// assert both halves of the acceptance: Terminated with reason
// MaxLifetimeExceeded, and the claimed pod object gone.

import (
	"context"
	"crypto/tls"
	"testing"
	"time"

	v1 "mitos.run/mitos/api/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"mitos.run/mitos/internal/controller"
	"mitos.run/mitos/internal/husk"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestHuskClaimLifetimeExpiryDeletesClaimedPod(t *testing.T) {
	poolName := uniqueName("lt688-pool")
	claimName := uniqueName("lt688-claim")
	const node = "lt688-node"

	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: poolName, Namespace: "default"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1, Max: 2, TargetPending: 0, CooldownSeconds: 0},
		},
	}
	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, pool)
		// envtest runs no garbage collector, so a deleted pool does not cascade to
		// its owner-ref'd husk pods. Delete this run's husk pods explicitly so they
		// do not leak into a later run's pool-label-keyed counts.
		var leftover corev1.PodList
		if err := k8sClient.List(ctx, &leftover, client.InNamespace("default"),
			client.MatchingLabels{"mitos.run/pool": poolName, "mitos.run/husk": "true"}); err == nil {
			for i := range leftover.Items {
				_ = k8sClient.Delete(ctx, &leftover.Items[i])
			}
		}
	})
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}, pool); err != nil {
		t.Fatalf("get pool for UID: %v", err)
	}

	// The husk pool reconciler, driven directly for the warm-pool create, mirrors
	// driveHuskPoolReconcile's use in husk_nodeloss_failover_envtest_test.go: its
	// own NodeRegistry/Demand are isolated from the suite manager's reconcilers,
	// so the desired dormant count is deterministic.
	frozen := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	poolR := &controller.SandboxPoolReconciler{
		Client:         k8sClient,
		NodeRegistry:   controller.NewNodeRegistry(),
		EnableHuskPods: true,
		HuskStubImage:  "mitos-husk-stub:test",
		Demand:         controller.NewPoolDemand(),
		Now:            func() time.Time { return frozen },
	}

	// The activate transport returns OK; the lifetime expiry loop is what is
	// under test, not the activation transport.
	setHuskTestActivator(func(_ context.Context, _ string, _ *tls.Config, _ husk.ActivateRequest) (husk.ActivateResult, error) {
		return husk.ActivateResult{OK: true, VsockPath: "/run/husk/vm/vsock", LatencyMs: 1.0}, nil
	})
	t.Cleanup(func() { setHuskTestActivator(nil) })

	// 1. The warm pool stands up one dormant slot; place it on a node and force it
	// Running/Ready the way scheduleAndReadyHuskPod does (envtest has no scheduler
	// or kubelet).
	driveHuskPoolReconcile(t, poolR, pool)
	slots := dormantHuskPods(t, poolName)
	if len(slots) != 1 {
		t.Fatalf("warm pool created %d dormant pods, want 1", len(slots))
	}
	warmPod := &slots[0]
	scheduleAndReadyHuskPod(t, warmPod, node, "10.31.0.1")

	// 2. A husk claim (labeled HuskTestClaimLabel so the suite's manager-driven
	// husk-mode reconciler owns it) activates the warm slot with a short TTL and
	// goes Ready.
	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claimName,
			Namespace: "default",
			Labels:    map[string]string{controller.HuskTestClaimLabel: "true"},
		},
		Spec: v1.SandboxSpec{
			Source:   v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: poolName}},
			Lifetime: &v1.SandboxLifetime{TTL: &metav1.Duration{Duration: 2 * time.Second}},
		},
	}
	if err := k8sClient.Create(ctx, claim); err != nil {
		t.Fatalf("create claim: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, claim) })

	ready := waitClaimPhase(t, claimName, func(c *v1.Sandbox) bool {
		return c.Status.Phase == v1.SandboxReady
	})
	if ready.Status.SandboxID != warmPod.Name {
		t.Fatalf("claim backed by %q, want the warm slot %q", ready.Status.SandboxID, warmPod.Name)
	}

	// 3. The TTL fires: the claim must reach Terminated with reason
	// MaxLifetimeExceeded.
	got := waitClaimTerminated(t, claimName)
	if r := terminatedReason(got); r != "MaxLifetimeExceeded" {
		t.Fatalf("terminated reason = %q, want MaxLifetimeExceeded", r)
	}

	// 4. The claimed pod must be GONE (not merely the claim stamped Terminated):
	// this is the issue #688 object-level gate. Before Task 1's fix, the claimed
	// husk pod is never deleted on lifetime expiry, so this poll would hang to the
	// deadline.
	deadline := time.Now().Add(20 * time.Second)
	for {
		var pods corev1.PodList
		if err := k8sClient.List(context.Background(), &pods,
			client.InNamespace("default"),
			client.MatchingLabels{"mitos.run/claim": claimName}); err != nil {
			t.Fatal(err)
		}
		if len(pods.Items) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("claimed husk pod still present %s after Terminated", pods.Items[0].Name)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
