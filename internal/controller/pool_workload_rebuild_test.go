package controller_test

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/controller"
)

// Editing a pool template's workload.command must re-trigger the snapshot build
// (issue #475). Before the fix the snapshot was keyed by pool name alone, so the
// holder count stayed satisfied and the changed command was silently ignored. The
// build identity now folds in the workload, so a command edit rebuilds the
// snapshot in place; an unchanged template does not churn.
func TestPoolWorkloadEditTriggersRebuild(t *testing.T) {
	c := k8sClient
	const node = "rebuild-node-475"

	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "rebuild-475-pool", Namespace: "default"},
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

	r := &controller.SandboxPoolReconciler{
		Client:       c,
		NodeRegistry: reg,
	}

	// First build: no holder yet, so the snapshot is built once and the build
	// identity is recorded.
	if err := r.EnsureTemplateBuiltForTest(ctx, pool, pool.Spec.Template); err != nil {
		t.Fatalf("ensureTemplateBuilt (first build): %v", err)
	}
	if got := rec.CreateTemplateCount(); got != 1 {
		t.Fatalf("CreateTemplate count after first build = %d, want 1", got)
	}
	firstHash := pool.Status.TemplateBuildHash
	if firstHash == "" {
		t.Fatal("TemplateBuildHash was not recorded after the first build")
	}

	// A no-op reconcile (identical template, holder count satisfied) must NOT
	// rebuild: no snapshot churn.
	if err := r.EnsureTemplateBuiltForTest(ctx, pool, pool.Spec.Template); err != nil {
		t.Fatalf("ensureTemplateBuilt (steady state): %v", err)
	}
	if got := rec.CreateTemplateCount(); got != 1 {
		t.Fatalf("CreateTemplate count after a no-op reconcile = %d, want 1 (no churn)", got)
	}

	// Edit the workload command: the build identity changes, so the snapshot must
	// be rebuilt in place on the holder even though the holder count is satisfied.
	pool.Spec.Template.Workload.Command = []string{"gunicorn", "app:app"}
	if err := r.EnsureTemplateBuiltForTest(ctx, pool, pool.Spec.Template); err != nil {
		t.Fatalf("ensureTemplateBuilt (after workload edit): %v", err)
	}
	if got := rec.CreateTemplateCount(); got != 2 {
		t.Fatalf("CreateTemplate count after a workload.command edit = %d, want 2 (the edit must rebuild)", got)
	}
	if pool.Status.TemplateBuildHash == firstHash {
		t.Fatalf("TemplateBuildHash did not change after a workload.command edit (still %s)", firstHash)
	}
}
