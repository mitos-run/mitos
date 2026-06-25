package controller_test

// Envtest chaos coverage for the failure/GC G2 gate (issue #163): the
// controller-restart-under-a-claim-storm leg, the part of the chaos acceptance
// bar reachable on the mock engine without KVM.
//
// The cluster chaos suite (test/cluster-e2e/chaos-e2e.sh stage 6) SIGKILLs the
// real controller and forkd processes mid-storm on a KVM cluster. That exact
// process kill needs a real cluster and KVM and cannot run here. What CAN be
// proven on darwin is the INVARIANT that crash recovery rests on: the GC holds
// no in-memory desired state, so a freshly-constructed GarbageCollector (the
// state a restarted controller starts from) reconciles a storm's worth of forkd
// VMs purely from CRD state, with ZERO ORPHANS and ZERO PERMANENTLY-STUCK
// CLAIMS:
//
//   - while the storm's claims are alive and Ready, a fresh GC pass reaps NONE
//     of their backing VMs (no false-positive sweep that would strand a claim);
//   - once the storm subsides (all claims deleted), a fresh GC pass drives the
//     node to zero orphan VMs (every now-backing-less VM reaped within one
//     interval past the grace).
//
// This is the controller-crash + mock-engine half of stage 6. The guest-agent
// in-VM crash and the real forkd-with-VMs crash remain KVM-gated and are covered
// only by the cluster suite.

import (
	"fmt"
	v1 "mitos.run/mitos/api/v1"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"mitos.run/mitos/internal/controller"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestGCChaosStormNoOrphansNoStuckClaims(t *testing.T) {
	stop, engine, _, err := controller.StartFakeForkdNodeRecording(testRegistry, "chaos-storm-node-1", "chaos-storm-pool")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "chaos-storm-pool", Namespace: "default"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}
	for _, obj := range []client.Object{pool} {
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, pool)
	})

	// Launch a storm of claims and drive every one to Ready (the suite's raw-forkd
	// claim reconciler forks each onto the fake node).
	const stormSize = 6
	names := make([]string, 0, stormSize)
	backedIDs := make(map[string]bool, stormSize)
	for i := 0; i < stormSize; i++ {
		name := fmt.Sprintf("chaos-storm-claim-%d", i)
		names = append(names, name)
		claim := &v1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       v1.SandboxSpec{Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: "chaos-storm-pool"}}},
		}
		if err := k8sClient.Create(ctx, claim); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, name := range names {
			c := &v1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
			_ = k8sClient.Delete(ctx, c)
		}
	})
	for _, name := range names {
		ready := waitClaimReady(t, name)
		if ready.Status.SandboxID == "" {
			t.Fatalf("ready storm claim %s has empty sandbox id", name)
		}
		backedIDs[ready.Status.SandboxID] = true
	}
	if len(backedIDs) != stormSize {
		t.Fatalf("expected %d distinct backing VMs, got %d", stormSize, len(backedIDs))
	}

	// Simulate a CONTROLLER CRASH AND RESTART mid-storm: a brand-new GC with no
	// in-memory state, exactly what a restarted controller begins from. Its first
	// pass must rebuild desired state from CRDs alone and reap NONE of the storm's
	// backing VMs (a false sweep here would strand a Ready claim with a dead VM).
	freshGC := func() *controller.GarbageCollector {
		return &controller.GarbageCollector{
			Client:            k8sClient,
			Registry:          testRegistry,
			OrphanGrace:       0, // no grace: prove the live-claim net alone protects backed VMs
			DefaultTTLSeconds: 3600,
		}
	}
	freshGC().RunOnce(ctx)

	for _, id := range engine.TerminatedIDs() {
		if backedIDs[id] {
			t.Fatalf("controller-restart GC pass reaped a live storm claim's VM %s (zero-orphan invariant broken the wrong way)", id)
		}
	}
	// Every storm claim is still Ready: none was stranded.
	for _, name := range names {
		var got v1.Sandbox
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, &got); err != nil {
			t.Fatal(err)
		}
		if got.Status.Phase != v1.SandboxReady {
			t.Fatalf("storm claim %s no longer Ready after the restart GC pass: phase=%q (stuck claim)", name, got.Status.Phase)
		}
	}

	// The storm subsides: delete every claim. The finalizer reap terminates each
	// backing VM as the object is removed, so after deletion the node should hold
	// no storm VMs. Then a fresh GC pass (another simulated restart) must leave
	// ZERO orphans regardless of whether the finalizer already reaped them: any
	// VM whose claim object is now gone, past the (zero) grace, is swept.
	for _, name := range names {
		c := &v1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
		if err := k8sClient.Delete(ctx, c); err != nil && client.IgnoreNotFound(err) != nil {
			t.Fatal(err)
		}
	}
	// Wait for the claim objects to be gone (finalizer reap completed).
	deadline := time.Now().Add(20 * time.Second)
	for {
		var remaining int
		for _, name := range names {
			var got v1.Sandbox
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, &got); err == nil {
				remaining++
			}
		}
		if remaining == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d storm claims still present 20s after delete (finalizer wedged)", remaining)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Re-inject every backing VM as a lingering orphan (the finalizer may already
	// have reaped them; this asserts the GC reaps any that linger past a crash
	// that interrupted the finalizer). A fresh GC pass must drive the node to zero
	// storm VMs.
	for id := range backedIDs {
		engine.InjectSandbox(id, time.Now().Add(-10*time.Minute))
	}
	freshGC().RunOnce(ctx)

	for _, r := range engine.ListSandboxes() {
		if backedIDs[r.ID] {
			t.Fatalf("orphan VM %s survived the post-storm GC pass (zero-orphan invariant broken)", r.ID)
		}
	}
}
