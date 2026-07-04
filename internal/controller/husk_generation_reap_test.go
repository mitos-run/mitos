package controller_test

// Issue #679, second half: WHO bumps the pool's TemplateBuildGeneration, and
// the reap actually firing on a generation mismatch. The prod incident: a
// no-digest fallback fleet survived a template rebuild, so claims activated a
// NEW mem snapshot against OLD rootfs CoW clones. The generation is the
// digest-free rebuild signal: bumped only when artifacts are rebuilt IN PLACE
// (content-change rebuild, forced rebuild, automatic restore-failure rebuild),
// never on first builds or deficit copies.

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/controller"
	client "sigs.k8s.io/controller-runtime/pkg/client"
)

// waitForSingleHuskAnnotation polls until exactly one live husk pod exists and
// it carries annotation key=want. Mirrors waitForSingleHuskDigest; envtest has
// no kubelet, so deletions finalize asynchronously.
func waitForSingleHuskAnnotation(t *testing.T, c client.Client, poolName, key, want string) {
	t.Helper()
	for i := 0; i < 50; i++ {
		live := make([]corev1.Pod, 0)
		for _, p := range listHuskPods(t, c, poolName) {
			if p.DeletionTimestamp == nil {
				live = append(live, p)
			}
		}
		if len(live) == 1 && live[0].Annotations[key] == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("did not converge to a single husk pod with %s=%q", key, want)
}

// First build and steady state leave the generation at 0; a content-change
// rebuild bumps it; a forced rebuild (the mitos-rebuild-template lever) bumps
// it again.
func TestTemplateBuildGenerationBumpsOnInPlaceRebuildsOnly(t *testing.T) {
	c := k8sClient
	const node = "gen-node-679"

	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "gen-679-pool", Namespace: "default"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{
				Image:    "python:3.12-slim",
				Workload: &v1.WorkloadSpec{Command: []string{"flask", "run"}},
			},
			Warm: &v1.PoolWarm{Min: 1},
		},
	}

	reg := controller.NewNodeRegistry()
	stop, rec, err := controller.StartFakeForkdNodeEncRecording(reg, node)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)

	r := &controller.SandboxPoolReconciler{Client: c, NodeRegistry: reg}

	// First build: not an in-place rebuild, generation stays 0.
	if err := r.EnsureTemplateBuiltForTest(ctx, pool, pool.Spec.Template); err != nil {
		t.Fatalf("ensureTemplateBuilt (first build): %v", err)
	}
	if got := pool.Status.TemplateBuildGeneration; got != 0 {
		t.Fatalf("generation after first build = %d, want 0", got)
	}

	// Steady state: no rebuild, no bump.
	if err := r.EnsureTemplateBuiltForTest(ctx, pool, pool.Spec.Template); err != nil {
		t.Fatalf("ensureTemplateBuilt (steady state): %v", err)
	}
	if got := pool.Status.TemplateBuildGeneration; got != 0 {
		t.Fatalf("generation after steady-state reconcile = %d, want 0", got)
	}

	// Content edit: in-place rebuild, bump to 1.
	pool.Spec.Template.Workload.Command = []string{"uvicorn", "app:app"}
	if err := r.EnsureTemplateBuiltForTest(ctx, pool, pool.Spec.Template); err != nil {
		t.Fatalf("ensureTemplateBuilt (content edit): %v", err)
	}
	if got := pool.Status.TemplateBuildGeneration; got != 1 {
		t.Fatalf("generation after content-change rebuild = %d, want 1", got)
	}
	if got := rec.CreateTemplateCount(); got < 2 {
		t.Fatalf("sanity: content edit did not rebuild (CreateTemplate count %d)", got)
	}

	// Forced rebuild via the annotation lever: bump to 2.
	pool.Annotations = map[string]string{"mitos.run/force-rebuild": "op-1"}
	if !r.DriveForceRebuildForTest(ctx, pool, pool.Spec.Template, nil, metav1.Now()) {
		t.Fatal("driveForceRebuild did not trigger on a fresh annotation value")
	}
	if got := pool.Status.TemplateBuildGeneration; got != 2 {
		t.Fatalf("generation after forced rebuild = %d, want 2", got)
	}
}

// A dormant husk pod created under an older generation is reaped on the next
// reconcile even when NO digest was ever known (the #679 prod fleet), and the
// refilled pod carries the current generation. A pod with the annotation
// stripped entirely (a legacy pre-#679 pod) is reaped the same way.
func TestHuskPodWithStaleGenerationIsReapedAndRefilled(t *testing.T) {
	c := k8sClient
	const poolName = "gen-reap-pool"

	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: poolName, Namespace: "default"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}
	if err := c.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, p := range listHuskPods(t, c, poolName) {
			_ = c.Delete(ctx, &p)
		}
		_ = c.Delete(ctx, pool)
	})

	// Registry with a node but NO digest for the template: the created pods get
	// no digest annotation, exactly the prod fallback fleet.
	reg := controller.NewNodeRegistry()
	reg.Register(&controller.NodeInfo{Name: "node-a", TemplateIDs: []string{poolName}})
	r := &controller.SandboxPoolReconciler{
		Client:          c,
		NodeRegistry:    reg,
		EnableHuskPods:  true,
		HuskStubImage:   "mitos-husk-stub:test",
		KVMResourceName: "mitos.run/kvm",
	}

	if _, err := r.ReconcileHuskPodsForTest(ctx, pool, pool.Spec.Template); err != nil {
		t.Fatalf("reconcileHuskPods (initial): %v", err)
	}
	pods := listHuskPods(t, c, poolName)
	if len(pods) != 1 {
		t.Fatalf("setup: want 1 husk pod, got %d", len(pods))
	}
	if got := pods[0].Annotations["mitos.run/template-digest"]; got != "" {
		t.Fatalf("setup: expected a digest-less pod, got digest %q", got)
	}
	if got := pods[0].Annotations["mitos.run/template-build-generation"]; got != "0" {
		t.Fatalf("setup: generation annotation = %q, want 0", got)
	}

	// An in-place rebuild happened: the pool's generation is now 1. The dormant
	// generation-0 pod references the OLD artifacts and must be reaped; the
	// refill carries generation 1.
	pool.Status.TemplateBuildGeneration = 1
	if _, err := r.ReconcileHuskPodsForTest(ctx, pool, pool.Spec.Template); err != nil {
		t.Fatalf("reconcileHuskPods (after generation bump): %v", err)
	}
	waitForSingleHuskAnnotation(t, c, poolName, "mitos.run/template-build-generation", "1")

	// Legacy fleet: strip the generation annotation from the surviving pod (a
	// pod created before #679 shipped), bump the generation again, and the
	// unstamped pod must be reaped too.
	pods = listHuskPods(t, c, poolName)
	if len(pods) != 1 {
		t.Fatalf("want 1 pod after refill, got %d", len(pods))
	}
	legacy := pods[0].DeepCopy()
	delete(legacy.Annotations, "mitos.run/template-build-generation")
	if err := c.Update(ctx, legacy); err != nil {
		t.Fatalf("strip generation annotation: %v", err)
	}
	pool.Status.TemplateBuildGeneration = 2
	if _, err := r.ReconcileHuskPodsForTest(ctx, pool, pool.Spec.Template); err != nil {
		t.Fatalf("reconcileHuskPods (legacy reap): %v", err)
	}
	waitForSingleHuskAnnotation(t, c, poolName, "mitos.run/template-build-generation", "2")
}
