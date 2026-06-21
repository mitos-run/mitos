package controller_test

// Conformance envtest for the v1alpha2 consolidated Sandbox kind (issue #23,
// ADR 0007): the three-noun API expresses everything the four-noun API did.
//
//   - A Sandbox{source.poolRef} reaches Ready, driving the same fork-from-pool
//     path a SandboxClaim drives (the CLAIM equivalent). Ported from the
//     claim-ready assertions.
//   - A Sandbox{source.fromSandbox, replicas:N} produces N children, driving the
//     live-fork path a SandboxFork drives (the FORK equivalent). Ported from the
//     fork-ready assertions.
//
// Both run against the MOCK engine (the fake forkd node), additively: the v2
// Sandbox reconciler owns a SandboxClaim / SandboxFork that the existing raw
// reconcilers drive, and mirrors the child status back onto the Sandbox.

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	v1alpha1 "mitos.run/mitos/api/v1alpha1"
	v1alpha2 "mitos.run/mitos/api/v1alpha2"
	"mitos.run/mitos/internal/controller"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// waitSandboxReady polls until the named v1alpha2 Sandbox reaches Ready and
// returns it, failing the test otherwise.
func waitSandboxReady(t *testing.T, name string) *v1alpha2.Sandbox {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last v1alpha2.Sandbox
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, &last); err == nil {
			if last.Status.Phase == v1alpha1.SandboxReady {
				return &last
			}
			if last.Status.Phase == v1alpha1.SandboxFailed {
				t.Fatalf("sandbox %s failed: %+v", name, last.Status)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("sandbox %s did not become Ready within 20s; last status: %+v", name, last.Status)
	return nil
}

// TestSandboxV2PoolRefReachesReady is the CLAIM-equivalent conformance: a
// Sandbox with source.poolRef and the default replicas 1 reaches Ready, with an
// endpoint and pod mirrored back from the owned claim.
func TestSandboxV2PoolRefReachesReady(t *testing.T) {
	stop, err := controller.StartFakeForkdNode(testRegistry, "v2pool-node-1", "v2pool-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "v2pool-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "v2pool-pool", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "v2pool-tmpl"},
			Replicas:    1,
		},
	}
	sb := &v1alpha2.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "v2pool-sandbox", Namespace: "default"},
		Spec: v1alpha2.SandboxSpec{
			Source:   v1alpha2.SandboxSource{PoolRef: &v1alpha1.LocalObjectReference{Name: "v2pool-pool"}},
			Replicas: 1,
		},
	}
	for _, obj := range []client.Object{template, pool, sb} {
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, sb)
		_ = k8sClient.Delete(ctx, pool)
		_ = k8sClient.Delete(ctx, template)
	})

	ready := waitSandboxReady(t, "v2pool-sandbox")
	if ready.Status.Endpoint == "" {
		t.Fatal("Ready sandbox has no endpoint mirrored from the claim")
	}
	if ready.Status.Pod == "" {
		t.Fatal("Ready sandbox has no pod mirrored from the claim")
	}

	// The owned SandboxClaim exists, is named after the sandbox, and is
	// owner-referenced to it (so it GCs with the sandbox). This proves the v2
	// surface drives the SAME engine path the four-noun claim drives.
	var claim v1alpha1.SandboxClaim
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "v2pool-sandbox", Namespace: "default"}, &claim); err != nil {
		t.Fatalf("owned claim not found: %v", err)
	}
	owner := metav1.GetControllerOf(&claim)
	if owner == nil || owner.Kind != "Sandbox" || owner.Name != "v2pool-sandbox" {
		t.Fatalf("claim controller owner = %+v, want Sandbox v2pool-sandbox", owner)
	}
}

// TestSandboxV2FromSandboxProducesNChildren is the FORK-equivalent conformance:
// a Sandbox with source.fromSandbox and replicas N produces N ready children,
// each mirrored into status.children, and reports readyReplicas == N.
func TestSandboxV2FromSandboxProducesNChildren(t *testing.T) {
	stop, err := controller.StartFakeForkdNode(testRegistry, "v2fork-node-1", "v2fork-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "v2fork-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "v2fork-pool", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "v2fork-tmpl"},
			Replicas:    1,
		},
	}
	// The source is itself a v2 Sandbox{poolRef}: it reaches Ready first, then a
	// second Sandbox forks it.
	source := &v1alpha2.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "v2fork-source", Namespace: "default"},
		Spec: v1alpha2.SandboxSpec{
			Source:   v1alpha2.SandboxSource{PoolRef: &v1alpha1.LocalObjectReference{Name: "v2fork-pool"}},
			Replicas: 1,
		},
	}
	for _, obj := range []client.Object{template, pool, source} {
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, source)
		_ = k8sClient.Delete(ctx, pool)
		_ = k8sClient.Delete(ctx, template)
	})

	// The owned source claim must be Ready before the fork can fan out (the fork
	// reconciler waits on the source's Ready phase).
	waitSandboxReady(t, "v2fork-source")
	// The fork reconciler forks the SOURCE CLAIM (the owned child, named after the
	// source sandbox). The v2 fan-out Sandbox names its fromSandbox by that claim.
	waitClaimReady(t, "v2fork-source")

	const replicas = 3
	fanout := &v1alpha2.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "v2fork-fanout", Namespace: "default"},
		Spec: v1alpha2.SandboxSpec{
			Source:   v1alpha2.SandboxSource{FromSandbox: &v1alpha2.FromSandboxSource{Name: "v2fork-source"}},
			Replicas: replicas,
		},
	}
	if err := k8sClient.Create(ctx, fanout); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, fanout) })

	ready := waitSandboxReady(t, "v2fork-fanout")
	if ready.Status.ReadyReplicas != replicas {
		t.Fatalf("readyReplicas = %d, want %d", ready.Status.ReadyReplicas, replicas)
	}
	if len(ready.Status.Children) != replicas {
		t.Fatalf("children = %d, want %d: %+v", len(ready.Status.Children), replicas, ready.Status.Children)
	}
	for i, c := range ready.Status.Children {
		if c.Name == "" || c.Endpoint == "" || c.SandboxID == "" {
			t.Fatalf("child %d incomplete: %+v", i, c)
		}
		if c.Phase != v1alpha1.SandboxReady {
			t.Fatalf("child %d phase = %q, want Ready", i, c.Phase)
		}
	}

	// The owned SandboxFork exists and is owner-referenced to the fan-out Sandbox.
	var fork v1alpha1.SandboxFork
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "v2fork-fanout", Namespace: "default"}, &fork); err != nil {
		t.Fatalf("owned fork not found: %v", err)
	}
	if owner := metav1.GetControllerOf(&fork); owner == nil || owner.Kind != "Sandbox" {
		t.Fatalf("fork controller owner = %+v, want Sandbox", owner)
	}
	if fork.Spec.Replicas != replicas {
		t.Fatalf("owned fork replicas = %d, want %d", fork.Spec.Replicas, replicas)
	}
}
