package controller_test

// Envtest coverage for the husk node-loss self-heal loop, END TO END (issue #373).
//
// The pieces of husk node-loss recovery are unit/envtest covered in isolation
// (checkHuskPodLost re-pend in husk_repend_stamp_test.go, the GC no-op in husk mode
// in nodelost_test.go, the warm-pool refill in warmpool_autoscale_envtest_test.go),
// but the FULL loop a real multi-node KVM cluster exercises in
// test/cluster-e2e/chaos-e2e.sh stage 5 (cordon a claim's node, delete its husk
// pods, assert the claim recovers on ANOTHER node) was only proven at cluster-e2e
// level. This test mirrors that loop at envtest level with the suite's swappable
// fakes:
//
//  1. a Ready husk-backed claim on a warm slot (the "to-be-lost" node);
//  2. its backing husk pod is deleted (a drain, an eviction, a spot reclaim);
//  3. checkHuskPodLost (via the husk pod watch) detects the loss and the claim
//     RE-PENDS, clearing the dead endpoint/node (no stuck claim on a dead endpoint);
//  4. the warm pool REFILLS the drained slot (the husk pool reconciler);
//  5. the claim RE-ACTIVATES on a SURVIVING node;
//  6. NO ORPHAN remains: the lost pod is gone and exactly one husk pod (the refilled
//     slot) backs the claim, carrying this claim's label.
//
// envtest runs no scheduler and no kubelet, so scheduleAndReadyHuskPod is the
// stand-in for the scheduler binding the refilled warm slot onto a surviving node
// and the kubelet bringing its dormant VMM up; the activate transport is the
// suite's fake activator. Everything else (the loss detection, the re-pend, the
// refill, the re-activation, the orphan accounting) is the real controller code.

import (
	"context"
	"crypto/tls"
	v1 "mitos.run/mitos/api/v1"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"mitos.run/mitos/internal/controller"
	"mitos.run/mitos/internal/husk"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// driveHuskPoolReconcile runs a husk-enabled SandboxPool reconcile once, retrying on
// the benign status conflict the suite's own (husk-OFF) manager pool reconciler can
// race in. It mirrors the directly-driven reconcile in
// warmpool_autoscale_envtest_test.go: the manager runs a husk-OFF pool reconciler,
// so a husk-mode warm-pool create/refill is driven explicitly here.
func driveHuskPoolReconcile(t *testing.T, r *controller.SandboxPoolReconciler, pool *v1.SandboxPool) {
	t.Helper()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}}
	var err error
	for i := 0; i < 10; i++ {
		if _, err = r.Reconcile(ctx, req); err == nil {
			return
		}
		if !apierrors.IsConflict(err) {
			t.Fatalf("husk pool reconcile: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("husk pool reconcile still conflicting after retries: %v", err)
}

// dormantHuskPods lists the pool's dormant (unclaimed, non-terminating) husk pods.
func dormantHuskPods(t *testing.T, poolName string) []corev1.Pod {
	t.Helper()
	var pods corev1.PodList
	if err := k8sClient.List(ctx, &pods, client.InNamespace("default"),
		client.MatchingLabels{"mitos.run/pool": poolName, "mitos.run/husk": "true"}); err != nil {
		t.Fatalf("list husk pods: %v", err)
	}
	var out []corev1.Pod
	for i := range pods.Items {
		p := pods.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		if _, claimed := p.Labels["mitos.run/claim"]; claimed {
			continue
		}
		out = append(out, p)
	}
	return out
}

// scheduleAndReadyHuskPod binds a freshly-created (unscheduled) husk pod onto node
// and forces it Running+Ready with a PodIP. envtest runs no scheduler and no
// kubelet, so this is the stand-in for the scheduler placing the warm slot on node
// and the kubelet bringing its dormant VMM up so the slot is selectable.
func scheduleAndReadyHuskPod(t *testing.T, pod *corev1.Pod, node, podIP string) {
	t.Helper()
	binding := &corev1.Binding{
		ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
		Target:     corev1.ObjectReference{Kind: "Node", Name: node},
	}
	if err := k8sClient.SubResource("binding").Create(ctx, pod, binding); err != nil {
		t.Fatalf("bind husk pod %s to node %s: %v", pod.Name, node, err)
	}
	var got corev1.Pod
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, &got); err != nil {
		t.Fatalf("get husk pod %s: %v", pod.Name, err)
	}
	got.Status.Phase = corev1.PodRunning
	got.Status.PodIP = podIP
	got.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := k8sClient.Status().Update(ctx, &got); err != nil {
		t.Fatalf("force husk pod %s ready: %v", pod.Name, err)
	}
}

// TestHuskNodeLossSelfHealsEndToEnd drives the husk node-loss recovery loop end to
// end and asserts the two acceptance properties: NO stuck claim (it returns to
// Ready, re-activated on a SURVIVING node) and NO orphan (the lost pod is gone and
// exactly one husk pod backs the claim). It is the envtest mirror of chaos-e2e.sh
// stage 5 (cross-node failover), which needs a real multi-node KVM cluster.
func TestHuskNodeLossSelfHealsEndToEnd(t *testing.T) {
	// Per-run unique names so repeated runs on the shared apiserver never collide
	// and the pool-label-keyed pod helpers only ever see THIS run's husk pods.
	poolName := uniqueName("hnl-pool")
	claimName := uniqueName("hnl-claim")
	const lostNode = "hnl-lost-node"
	const survivorNode = "hnl-survivor-node"

	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: poolName, Namespace: "default"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			// Min 1 so the drained slot refills; Max 2 with TargetPending 0 keeps the
			// desired dormant count at exactly 1, so the create and the refill each
			// produce a single deterministic slot.
			Warm: &v1.PoolWarm{Min: 1, Max: 2, TargetPending: 0, CooldownSeconds: 0},
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

	// The husk pool reconciler, driven directly for the warm-pool create + refill.
	// Its own NodeRegistry/Demand are isolated from the suite manager's reconcilers,
	// so the desired dormant count is deterministic (no cross-test arrival demand).
	frozen := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	poolR := &controller.SandboxPoolReconciler{
		Client:         k8sClient,
		NodeRegistry:   controller.NewNodeRegistry(),
		EnableHuskPods: true,
		HuskStubImage:  "mitos-husk-stub:test",
		Demand:         controller.NewPoolDemand(),
		Now:            func() time.Time { return frozen },
	}

	// The activate transport returns OK for BOTH the initial activation and the
	// post-loss re-activation; the loop is what is under test, not the transport.
	setHuskTestActivator(func(_ context.Context, _ string, _ *tls.Config, _ husk.ActivateRequest) (husk.ActivateResult, error) {
		return husk.ActivateResult{OK: true, VsockPath: "/run/husk/vm/vsock", LatencyMs: 1.0}, nil
	})
	t.Cleanup(func() { setHuskTestActivator(nil) })

	// 1. The warm pool stands up one dormant slot; place it on the to-be-lost node.
	driveHuskPoolReconcile(t, poolR, pool)
	slots := dormantHuskPods(t, poolName)
	if len(slots) != 1 {
		t.Fatalf("warm pool created %d dormant pods, want 1", len(slots))
	}
	p1 := &slots[0]
	scheduleAndReadyHuskPod(t, p1, lostNode, "10.30.0.1")

	// 2. A husk claim activates the warm slot and goes Ready on the to-be-lost node.
	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claimName,
			Namespace: "default",
			Labels:    map[string]string{controller.HuskTestClaimLabel: "true"},
		},
		Spec: v1.SandboxSpec{Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: poolName}}},
	}
	if err := k8sClient.Create(ctx, claim); err != nil {
		t.Fatalf("create claim: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, claim) })

	ready := waitClaimPhase(t, claimName, func(c *v1.Sandbox) bool {
		return c.Status.Phase == v1.SandboxReady
	})
	if ready.Status.Node != lostNode {
		t.Fatalf("claim placed on %q, want the warm-slot node %q", ready.Status.Node, lostNode)
	}
	if ready.Status.SandboxID != p1.Name {
		t.Fatalf("claim backed by %q, want the warm slot %q", ready.Status.SandboxID, p1.Name)
	}
	if ready.Status.Endpoint == "" {
		t.Fatal("Ready claim has no endpoint")
	}

	// 3. NODE LOSS: the backing husk pod is deleted (a drain, an eviction, a spot
	// reclaim, an operator kubectl delete). The husk pod watch maps the delete to
	// the claim.
	if err := k8sClient.Delete(ctx, p1); err != nil {
		t.Fatalf("delete backing husk pod: %v", err)
	}

	// 4. checkHuskPodLost detects the loss and the claim RE-PENDS: it must leave
	// Ready and clear the dead endpoint/node, never keep advertising the dead pod.
	// This is the "no stuck claim on a dead endpoint" half of the acceptance.
	_ = waitClaimPhase(t, claimName, func(c *v1.Sandbox) bool {
		return c.Status.Phase == v1.SandboxPending &&
			c.Status.Endpoint == "" && c.Status.Node == "" && c.Status.SandboxID == ""
	})

	// 5. WARM POOL REFILL: the pool refills the drained slot. Place the fresh slot on
	// a SURVIVING node (the cross-node-failover target).
	driveHuskPoolReconcile(t, poolR, pool)
	refill := dormantHuskPods(t, poolName)
	if len(refill) != 1 {
		t.Fatalf("warm pool refilled %d dormant pods, want 1 (the drained slot)", len(refill))
	}
	p2 := &refill[0]
	if p2.Name == p1.Name {
		t.Fatalf("refill reused the lost pod object %q", p1.Name)
	}
	scheduleAndReadyHuskPod(t, p2, survivorNode, "10.30.0.2")

	// 6. The claim RE-ACTIVATES on the SURVIVING node: it returns to Ready, off the
	// lost node. This is the "no stuck claim" half: it reaches Ready again.
	healed := waitClaimPhase(t, claimName, func(c *v1.Sandbox) bool {
		return c.Status.Phase == v1.SandboxReady && c.Status.Node == survivorNode
	})
	if healed.Status.Node == lostNode {
		t.Fatalf("claim re-activated on the LOST node %q; it must fail over to a surviving node", lostNode)
	}
	if healed.Status.SandboxID != p2.Name {
		t.Fatalf("healed claim backed by %q, want the refilled slot %q", healed.Status.SandboxID, p2.Name)
	}
	if healed.Status.SandboxID == p1.Name {
		t.Fatalf("healed claim still points at the lost pod %q", p1.Name)
	}
	if healed.Status.Endpoint == "" {
		t.Fatal("healed claim has no endpoint")
	}

	// 7. NO ORPHAN: the lost pod is reaped, and exactly one husk pod (the refilled
	// slot, on the surviving node) carries this claim's label. No leaked pod, no
	// pod claimed by some other/absent claim.
	//
	// "Reaped" means NotFound OR Terminating (DeletionTimestamp set). envtest runs
	// no kubelet, so a gracefully-deleted Running pod (the drain/evict shape) enters
	// Terminating and lingers as an object instead of vanishing; on a real cluster
	// the kubelet completes the deletion. A Terminating pod is exactly what
	// checkHuskPodLost treats as lost, so for the orphan accounting it is gone: the
	// claim no longer references it and it carries no live warm-slot role.
	var goneP1 corev1.Pod
	switch err := k8sClient.Get(ctx, types.NamespacedName{Name: p1.Name, Namespace: "default"}, &goneP1); {
	case apierrors.IsNotFound(err):
		// fully reaped: ideal
	case err != nil:
		t.Fatalf("get lost husk pod %q: %v", p1.Name, err)
	case goneP1.DeletionTimestamp == nil:
		t.Fatalf("lost husk pod %q is still live (orphan): it is neither gone nor terminating", p1.Name)
	}
	var allPods corev1.PodList
	if err := k8sClient.List(ctx, &allPods, client.InNamespace("default"),
		client.MatchingLabels{"mitos.run/pool": poolName, "mitos.run/husk": "true"}); err != nil {
		t.Fatalf("list husk pods: %v", err)
	}
	claimed := 0
	for i := range allPods.Items {
		p := allPods.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		cn, ok := p.Labels["mitos.run/claim"]
		if !ok {
			continue
		}
		claimed++
		if cn != claimName {
			t.Fatalf("husk pod %q carries an orphan claim label %q (want %q)", p.Name, cn, claimName)
		}
		if p.Name != healed.Status.SandboxID {
			t.Fatalf("a husk pod other than the backing slot carries the claim label: %q", p.Name)
		}
		if p.Spec.NodeName != survivorNode {
			t.Fatalf("backing husk pod on %q, want the surviving node %q", p.Spec.NodeName, survivorNode)
		}
	}
	if claimed != 1 {
		t.Fatalf("expected exactly 1 claimed husk pod after self-heal, got %d", claimed)
	}
}
