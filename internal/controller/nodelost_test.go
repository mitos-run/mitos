package controller_test

// Envtest coverage for the NodeLost transition. A Ready claim whose node has
// left the registry (or gone unhealthy) is driven to a terminal Failed phase
// with a NodeLost condition by a GC pass; a claim on a still-healthy node is
// untouched. The orphan sweep and NodeLost do not fight: the sweep only visits
// healthy nodes, so a claim on a lost node is never swept.

import (
	v1 "mitos.run/mitos/api/v1"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"mitos.run/mitos/internal/controller"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func makeReadyClaim(t *testing.T, prefix, node string) *v1.Sandbox {
	t.Helper()
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: prefix + "-pool", Namespace: "default"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}
	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: prefix + "-claim", Namespace: "default"},
		Spec:       v1.SandboxSpec{Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: prefix + "-pool"}}},
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
	return waitClaimReady(t, prefix+"-claim")
}

func TestGCMarksNodeLost(t *testing.T) {
	stop, _, _, err := controller.StartFakeForkdNodeRecording(testRegistry, "nl-node-1", "nl1-pool")
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			stop()
		}
	}()

	makeReadyClaim(t, "nl1", "nl-node-1")

	// The node leaves the registry: the VM died with it. terminateOnNode is
	// never called (nothing to reach); the claim must become NodeLost.
	stop()
	stopped = true

	gc := &controller.GarbageCollector{Client: k8sClient, Registry: testRegistry}
	gc.RunOnce(ctx)

	var got v1.Sandbox
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "nl1-claim", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != v1.SandboxFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	c := meta.FindStatusCondition(got.Status.Conditions, "Ready")
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "NodeLost" {
		t.Fatalf("Ready condition = %+v, want Status=False Reason=NodeLost", c)
	}
	if got.Status.FinishedAt == nil {
		t.Fatal("FinishedAt not stamped on NodeLost claim")
	}
	if got.Status.FinishedAt.Time.After(time.Now().Add(time.Second)) {
		t.Fatalf("FinishedAt %v is in the future", got.Status.FinishedAt)
	}
}

// TestGCInHuskModeDoesNotFailNodeLostClaim asserts that in husk mode the GC
// does NOT terminally-fail a Ready claim whose node is lost. Husk node-loss is
// owned by checkHuskPodLost + the husk pod watch, which RE-PEND the claim onto a
// replacement dormant slot. A GC pass winning the race and flipping the claim to
// terminal Failed/NodeLost would defeat the husk self-heal, so the GC skips the
// node-lost-fail entirely when EnableHuskPods is set.
func TestGCInHuskModeDoesNotFailNodeLostClaim(t *testing.T) {
	stop, _, _, err := controller.StartFakeForkdNodeRecording(testRegistry, "nl-node-3", "nl3-pool")
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			stop()
		}
	}()

	makeReadyClaim(t, "nl3", "nl-node-3")

	// The node leaves the registry.
	stop()
	stopped = true

	// A GC in husk mode must leave the claim Ready: the husk re-pend path owns
	// node-loss recovery, not the GC.
	gc := &controller.GarbageCollector{Client: k8sClient, Registry: testRegistry, EnableHuskPods: true}
	gc.RunOnce(ctx)

	var got v1.Sandbox
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "nl3-claim", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase == v1.SandboxFailed {
		t.Fatalf("husk-mode GC must NOT flip a node-lost claim to Failed; phase = %q", got.Status.Phase)
	}
}

// TestGCRawForkdClaimReforksOntoSurvivingHolder is the end-to-end proof of
// raw-forkd claim auto-replacement after node loss (issue #372): a Ready claim
// whose node is lost is re-forked, by the live raw reconciler, onto a SURVIVING
// node that holds the same pool template snapshot, instead of failing terminally.
//
// Two forkd nodes hold the pool's template snapshot; the claim is pinned to
// node-1 so the test controls which node to lose. After node-1 leaves the
// registry, a raw GC pass re-pends the claim (a surviving holder exists), and the
// live reconciler re-issues the fork onto node-2, returning the claim to Ready
// there. This is the GC-decision plus reconciler-placement loop the unit test
// TestRawForkdClaimAutoReplacementAfterNodeLossOpen proves at the GC level.
func TestGCRawForkdClaimReforksOntoSurvivingHolder(t *testing.T) {
	// node-1 and node-2 both hold the pool's template snapshot (template id ==
	// pool name for an inline template).
	stop1, _, _, err := controller.StartFakeForkdNodeRecording(testRegistry, "nlrf-node-1", "nlrf-pool")
	if err != nil {
		t.Fatal(err)
	}
	stopped1 := false
	defer func() {
		if !stopped1 {
			stop1()
		}
	}()
	stop2, _, _, err := controller.StartFakeForkdNodeRecording(testRegistry, "nlrf-node-2", "nlrf-pool")
	if err != nil {
		t.Fatal(err)
	}
	defer stop2()

	// A pool plus a claim PINNED to node-1 via spec.nodeName, so the placed node
	// is deterministic and the test loses a known node.
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "nlrf-pool", Namespace: "default"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}
	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "nlrf-claim", Namespace: "default"},
		Spec: v1.SandboxSpec{
			Source:   v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: "nlrf-pool"}},
			NodeName: "nlrf-node-1",
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

	ready := waitClaimReady(t, "nlrf-claim")
	if ready.Status.Node != "nlrf-node-1" {
		t.Fatalf("claim placed on %q, want nlrf-node-1 (pinned)", ready.Status.Node)
	}

	// Lose node-1: the VM died with it. node-2 survives and holds the snapshot.
	stop1()
	stopped1 = true

	// A raw GC pass re-pends the claim (a surviving holder exists) instead of
	// failing it; the live raw reconciler then re-forks it onto node-2.
	gc := &controller.GarbageCollector{Client: k8sClient, Registry: testRegistry}
	gc.RunOnce(ctx)

	got := waitClaimPhase(t, "nlrf-claim", func(s *v1.Sandbox) bool {
		return s.Status.Phase == v1.SandboxReady && s.Status.Node == "nlrf-node-2"
	})
	if got.Status.Node != "nlrf-node-2" {
		t.Fatalf("claim re-forked onto %q, want nlrf-node-2 (the surviving holder)", got.Status.Node)
	}
}

func TestGCLeavesHealthyNodeClaim(t *testing.T) {
	stop, _, _, err := controller.StartFakeForkdNodeRecording(testRegistry, "nl-node-2", "nl2-pool")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	makeReadyClaim(t, "nl2", "nl-node-2")

	gc := &controller.GarbageCollector{Client: k8sClient, Registry: testRegistry}
	gc.RunOnce(ctx)

	var got v1.Sandbox
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "nl2-claim", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != v1.SandboxReady {
		t.Fatalf("phase = %q, want Ready (claim on healthy node must be untouched)", got.Status.Phase)
	}
}
