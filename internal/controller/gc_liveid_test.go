package controller_test

// Envtest coverage for the orphan-sweep liveID-by-name safety net. The
// controller uses the claim name AS the sandbox id, so a forkd sandbox whose id
// matches a non-terminal claim's name must NOT be swept even if that claim never
// wrote status.Node/status.SandboxID (e.g. wedged in Restoring or Pending past
// the grace). Once the claim object is deleted, the same sandbox becomes a
// genuine orphan and IS swept on the next pass.

import (
	v1 "mitos.run/mitos/api/v1"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"mitos.run/mitos/internal/controller"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestGCLiveClaimByNameNotSwept(t *testing.T) {
	stop, engine, _, err := controller.StartFakeForkdNodeRecording(testRegistry, "liveid-node-1", "liveid-other-pool")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// A claim that can never reach Ready: its pool's template has no ready
	// snapshot on any registered node, so selectNode keeps failing and the claim
	// stays in a non-terminal phase with no status.Node/status.SandboxID. This is
	// the stuck-Restoring window the safety net must cover.
	const claimName = "liveid-claim"
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "liveid-pool", Namespace: "default"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}
	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: "default"},
		Spec:       v1.SandboxSpec{Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: "liveid-pool"}}},
	}
	for _, obj := range []client.Object{pool, claim} {
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	cleaned := false
	t.Cleanup(func() {
		if !cleaned {
			_ = k8sClient.Delete(ctx, claim)
		}
		_ = k8sClient.Delete(ctx, pool)
	})

	// Confirm the claim is present and not in a terminal phase.
	var got v1.Sandbox
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: claimName, Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase == v1.SandboxTerminated || got.Status.Phase == v1.SandboxFailed {
		t.Fatalf("claim reached terminal phase %q unexpectedly; cannot exercise the liveID net", got.Status.Phase)
	}

	// The claim's VM, named after the claim, is live on a healthy node with an
	// uptime well past the grace. Its status was never written, so it is NOT in
	// the per-node desired set; only the liveID-by-name net keeps it alive.
	engine.InjectSandbox(claimName, time.Now().Add(-10*time.Minute))

	gc := &controller.GarbageCollector{
		Client:      k8sClient,
		Registry:    testRegistry,
		OrphanGrace: 60 * time.Second,
	}
	gc.RunOnce(ctx)

	for _, id := range engine.TerminatedIDs() {
		if id == claimName {
			t.Fatalf("GC swept VM %s while its claim object still exists", claimName)
		}
	}
	stillLive := false
	for _, r := range engine.ListSandboxes() {
		if r.ID == claimName {
			stillLive = true
		}
	}
	if !stillLive {
		t.Fatalf("VM %s disappeared though its claim is live", claimName)
	}

	// Delete the claim object. Once it is gone the VM is a genuine orphan and the
	// next pass must sweep it.
	if err := k8sClient.Delete(ctx, claim); err != nil {
		t.Fatal(err)
	}
	cleaned = true
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		err := k8sClient.Get(ctx, types.NamespacedName{Name: claimName, Namespace: "default"}, &got)
		if client.IgnoreNotFound(err) == nil && err != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Re-inject in case the (claim-less) sandbox was already swept; the point is
	// that with no claim object, the sweep reaps it.
	engine.InjectSandbox(claimName, time.Now().Add(-10*time.Minute))
	gc.RunOnce(ctx)

	swept := false
	for _, id := range engine.TerminatedIDs() {
		if id == claimName {
			swept = true
		}
	}
	if !swept {
		t.Fatalf("GC did not sweep VM %s after its claim object was deleted; terminated = %v", claimName, engine.TerminatedIDs())
	}
}
