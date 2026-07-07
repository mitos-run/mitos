package controller

// Coverage for issue #679: husk pods must be reapable after a template rebuild
// on EVERY creation path, not only the single-pinned-node digest path.
//
// Two mechanisms under test:
//   - the digest annotation is stamped whenever a digest is known, regardless
//     of how many snapshot-holder nodes the pod is pinned to, and the stale
//     check falls back to spec.nodeName when no node annotation was stamped;
//   - a monotonic pool build generation (Status.TemplateBuildGeneration),
//     stamped on every pool-owned husk pod, so pods created with NO digest at
//     all (the no-digest fallback that hit prod) are still reaped after a
//     rebuild. A missing generation annotation counts as stale once the pool
//     has rebuilt (generation > 0): those are exactly the legacy or fallback
//     pods whose rootfs CoW clone predates the current artifacts.

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "mitos.run/mitos/api/v1"
)

func genTestPool(name string, generation int64) *v1.SandboxPool {
	return &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: "pool-uid-gen"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
		Status: v1.SandboxPoolStatus{TemplateBuildGeneration: generation},
	}
}

func TestBuildHuskPodStampsDigestWithoutSingleSnapshotNode(t *testing.T) {
	const digest = "d1d1000000000000000000000000000000000000000000000000000000000000"
	pool := genTestPool("gen-pool-a", 0)
	r := &SandboxPoolReconciler{}

	for _, tc := range []struct {
		name  string
		nodes []string
	}{
		{"no snapshot nodes", nil},
		{"two snapshot nodes", []string{"node-a", "node-b"}},
	} {
		pod := r.buildHuskPod(pool, pool.Spec.Template, HuskPodOptions{
			StubImage:      "stub:test",
			ExpectedDigest: digest,
			SnapshotNodes:  tc.nodes,
		})
		if got := pod.Annotations[huskTemplateDigestAnnotation]; got != digest {
			t.Errorf("%s: digest annotation = %q, want %q (issue #679: stamp on ALL paths)", tc.name, got, digest)
		}
		if got := pod.Annotations[huskSnapshotNodeAnnotation]; got != "" {
			t.Errorf("%s: node annotation = %q, want empty (no single pinned node)", tc.name, got)
		}
	}

	// The single-pinned-node path keeps stamping both, unchanged.
	pod := r.buildHuskPod(pool, pool.Spec.Template, HuskPodOptions{
		StubImage:      "stub:test",
		ExpectedDigest: digest,
		SnapshotNodes:  []string{"node-a"},
	})
	if got := pod.Annotations[huskSnapshotNodeAnnotation]; got != "node-a" {
		t.Errorf("single node: node annotation = %q, want node-a", got)
	}
}

func TestBuildHuskPodStampsBuildGeneration(t *testing.T) {
	r := &SandboxPoolReconciler{}

	pod := r.buildHuskPod(genTestPool("gen-pool-b", 3), &v1.PoolTemplateSpec{Image: "img"}, HuskPodOptions{StubImage: "stub:test"})
	if got := pod.Annotations[huskBuildGenerationAnnotation]; got != "3" {
		t.Errorf("generation annotation = %q, want 3", got)
	}

	// Generation zero is stamped explicitly, so a pod created before any
	// rebuild is distinguishable from a legacy pod with no annotation at all.
	pod = r.buildHuskPod(genTestPool("gen-pool-c", 0), &v1.PoolTemplateSpec{Image: "img"}, HuskPodOptions{StubImage: "stub:test"})
	if got := pod.Annotations[huskBuildGenerationAnnotation]; got != "0" {
		t.Errorf("generation annotation at gen 0 = %q, want 0", got)
	}
}

func TestHuskPodStaleByGeneration(t *testing.T) {
	pod := func(genAnno string, claimed bool) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name:        "husk-x",
			Annotations: map[string]string{},
			Labels:      map[string]string{},
		}}
		if genAnno != "" {
			p.Annotations[huskBuildGenerationAnnotation] = genAnno
		}
		if claimed {
			p.Labels[huskClaimLabel] = "claim-1"
		}
		return p
	}

	for _, tc := range []struct {
		name     string
		podGen   string
		claimed  bool
		poolGen  int64
		want     bool
	}{
		{"older generation is stale", "2", false, 3, true},
		{"current generation is not stale", "3", false, 3, false},
		{"missing annotation before any rebuild is not stale", "", false, 0, false},
		{"missing annotation after a rebuild is stale (the #679 prod fleet)", "", false, 2, true},
		{"claimed pod is never stale", "1", true, 2, false},
	} {
		got := huskPodStaleByGeneration(pod(tc.podGen, tc.claimed), tc.poolGen)
		if got != tc.want {
			t.Errorf("%s: huskPodStaleByGeneration = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestHuskPodHasStaleDigestFallsBackToSpecNodeName(t *testing.T) {
	const tmpl = "gen-tmpl"
	const oldDigest = "aaaa000000000000000000000000000000000000000000000000000000000000"
	const newDigest = "bbbb000000000000000000000000000000000000000000000000000000000000"

	registry := NewNodeRegistry()
	registry.Register(&NodeInfo{Name: "node-a"})
	registry.AddTemplateWithDigest("node-a", tmpl, newDigest)

	r := &SandboxPoolReconciler{NodeRegistry: registry}

	// A pod stamped with the old digest but WITHOUT a node annotation (the
	// multi-node or fallback path) must still be recognized as stale via the
	// node the scheduler actually placed it on.
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "husk-y",
			Annotations: map[string]string{huskTemplateDigestAnnotation: oldDigest},
		},
		Spec: corev1.PodSpec{NodeName: "node-a"},
	}
	if !r.huskPodHasStaleDigest(p, tmpl) {
		t.Error("stale digest not detected via spec.nodeName fallback")
	}

	// Same pod on the CURRENT digest is not stale.
	p.Annotations[huskTemplateDigestAnnotation] = newDigest
	if r.huskPodHasStaleDigest(p, tmpl) {
		t.Error("current digest flagged stale via spec.nodeName fallback")
	}

	// Unscheduled pod with no node annotation: undecidable, never stale.
	p.Annotations[huskTemplateDigestAnnotation] = oldDigest
	p.Spec.NodeName = ""
	if r.huskPodHasStaleDigest(p, tmpl) {
		t.Error("pod with no node information must not be reaped")
	}
}

// TestBuildHuskPodMultiVMArgAndLabel proves the L1.7d capability wiring: with
// HuskPodOptions.MultiVM the built husk pod runs the stub with --multi-vm and
// carries the mitos.run/multi-vm label huskPodMultiVMCapable reads, and without
// it neither is present (single-VM default, unchanged).
func TestBuildHuskPodMultiVMArgAndLabel(t *testing.T) {
	r := &SandboxPoolReconciler{}
	pool := genTestPool("mvm-cap", 1)

	on := r.buildHuskPod(pool, pool.Spec.Template, HuskPodOptions{MultiVM: true})
	if on.Labels[huskMultiVMLabel] != huskMultiVMLabelValue {
		t.Errorf("MultiVM pod missing %s label, got %v", huskMultiVMLabel, on.Labels)
	}
	if !huskPodMultiVMCapable(on) {
		t.Error("MultiVM pod must be huskPodMultiVMCapable")
	}
	if !hasArg(on, "--multi-vm") {
		t.Errorf("MultiVM pod stub args must include --multi-vm, got %v", huskStubArgs(on))
	}

	off := r.buildHuskPod(pool, pool.Spec.Template, HuskPodOptions{})
	if _, ok := off.Labels[huskMultiVMLabel]; ok {
		t.Error("single-VM pod must not carry the multi-vm label")
	}
	if huskPodMultiVMCapable(off) {
		t.Error("single-VM pod must not be huskPodMultiVMCapable")
	}
	if hasArg(off, "--multi-vm") {
		t.Error("single-VM pod stub args must not include --multi-vm")
	}
}

func huskStubArgs(pod *corev1.Pod) []string {
	for _, c := range pod.Spec.Containers {
		if c.Name == huskContainerName {
			return c.Args
		}
	}
	return nil
}

func hasArg(pod *corev1.Pod, arg string) bool {
	for _, a := range huskStubArgs(pod) {
		if a == arg {
			return true
		}
	}
	return false
}
