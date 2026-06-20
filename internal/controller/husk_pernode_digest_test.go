package controller_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"mitos.run/mitos/api/v1alpha1"
	"mitos.run/mitos/internal/controller"
)

// TestHuskPodsUsePerNodeDigest is the regression for issue #175. Each forkd node
// builds its template snapshot INDEPENDENTLY, so the recorded content-addressed
// digests differ per node. The controller must pin each husk pod to one snapshot
// node and pass THAT node's digest as --expected-digest; handing every pod a
// single cluster-wide digest makes pods that land on any other node fail
// prepare-time snapshot verification ("read recorded manifest: no such file").
func TestHuskPodsUsePerNodeDigest(t *testing.T) {
	c := k8sClient
	const (
		tmpl     = "pernode-tmpl"
		poolName = "pernode-pool"
		digestA  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		digestB  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)

	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: tmpl, Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: poolName, Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: tmpl},
			Replicas:    2,
		},
	}
	for _, obj := range []client.Object{template, pool} {
		if err := c.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, p := range listHuskPods(t, c, poolName) {
			_ = c.Delete(ctx, &p)
		}
		_ = c.Delete(ctx, pool)
		_ = c.Delete(ctx, template)
	})

	// Two healthy holders of the SAME template with DIFFERENT recorded digests.
	reg := controller.NewNodeRegistry()
	reg.Register(&controller.NodeInfo{
		Name:            "node-a",
		TemplateIDs:     []string{tmpl},
		TemplateDigests: map[string]string{tmpl: digestA},
	})
	reg.Register(&controller.NodeInfo{
		Name:            "node-b",
		TemplateIDs:     []string{tmpl},
		TemplateDigests: map[string]string{tmpl: digestB},
	})

	r := &controller.SandboxPoolReconciler{
		Client:          c,
		NodeRegistry:    reg,
		EnableHuskPods:  true,
		HuskStubImage:   "mitos-husk-stub:test",
		KVMResourceName: "mitos.run/kvm",
	}
	if _, err := r.ReconcileHuskPodsForTest(ctx, pool, template); err != nil {
		t.Fatalf("reconcileHuskPods: %v", err)
	}

	pods := listHuskPods(t, c, poolName)
	if len(pods) != 2 {
		t.Fatalf("want 2 husk pods, got %d", len(pods))
	}

	// Each pod must be pinned to exactly one node and carry THAT node's digest.
	wantDigestForNode := map[string]string{"node-a": digestA, "node-b": digestB}
	seenNode := map[string]bool{}
	for _, p := range pods {
		node := pinnedNode(t, &p)
		digest := argValue(p.Spec.Containers[0].Args, "--expected-digest")
		if want := wantDigestForNode[node]; digest != want {
			t.Errorf("pod %s pinned to %q has --expected-digest %q, want %q (the node-local digest)", p.Name, node, digest, want)
		}
		seenNode[node] = true
	}
	// Both nodes must be used (the bug pinned both pods to one digest/node).
	if !seenNode["node-a"] || !seenNode["node-b"] {
		t.Errorf("husk pods did not spread across both snapshot nodes: %v", seenNode)
	}
}

// pinnedNode returns the single hostname a husk pod is pinned to via required
// node affinity, or "" if it is not pinned to exactly one.
func pinnedNode(t *testing.T, p *corev1.Pod) string {
	t.Helper()
	aff := p.Spec.Affinity
	if aff == nil || aff.NodeAffinity == nil || aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		t.Fatalf("pod %s has no required node affinity (not pinned to a snapshot node)", p.Name)
	}
	terms := aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	for _, term := range terms {
		for _, expr := range term.MatchExpressions {
			if expr.Key == "kubernetes.io/hostname" && len(expr.Values) == 1 {
				return expr.Values[0]
			}
		}
	}
	t.Fatalf("pod %s not pinned to exactly one hostname: %+v", p.Name, terms)
	return ""
}

// argValue returns the value following flag in args, or "" if absent.
func argValue(args []string, flag string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}
