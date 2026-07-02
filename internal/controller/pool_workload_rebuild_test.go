package controller_test

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/controller"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

// A husk pod's dormant VMM restores the pool's template snapshot at start. A
// snapshot that "built" successfully but fails to RESTORE crashloops every
// dormant husk pod bound to it forever; no other reconcile path rebuilds it
// (issue #584). driveTemplateHealth must detect >= 2 crashlooping dormant husk
// pods on the pool's CURRENT digest, report TemplateHealthy=False/RestoreFailing
// then False/Rebuilding, force a real forkd CreateTemplate (via the fake forkd's
// CreateTemplateCount), reap the crashloopers, bump RebuildAttempts, and then
// refuse a second rebuild for a reconcile that lands inside the backoff window.
func TestPoolRebuildsRestoreFailingTemplate(t *testing.T) {
	c := k8sClient
	const (
		poolName = "restore-failing-pool"
		tmpl     = poolName
		node     = "restore-failing-node"
	)

	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: poolName, Namespace: "default"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 2},
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

	reg := controller.NewNodeRegistry()
	stop, rec, err := controller.StartFakeForkdNodeEncRecording(reg, node)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)

	r := &controller.SandboxPoolReconciler{
		Client:          c,
		NodeRegistry:    reg,
		EnableHuskPods:  true,
		HuskStubImage:   "mitos-husk-stub:test",
		KVMResourceName: "mitos.run/kvm",
	}

	var live v1.SandboxPool
	if err := c.Get(ctx, client.ObjectKeyFromObject(pool), &live); err != nil {
		t.Fatal(err)
	}

	// Build the snapshot once, then place 2 warm husk pods pinned to the holder.
	if err := r.EnsureTemplateBuiltForTest(ctx, &live, live.Spec.Template); err != nil {
		t.Fatalf("ensureTemplateBuilt: %v", err)
	}
	if got := rec.CreateTemplateCount(); got != 1 {
		t.Fatalf("CreateTemplate count after the first build = %d, want 1", got)
	}
	digest, ok := reg.TemplateDigest(tmpl)
	if !ok || digest == "" {
		t.Fatal("no digest recorded after the first build")
	}
	// The production Reconcile husk path stamps this from the registry before
	// evaluating template health; mirror that here since this test drives
	// driveTemplateHealth directly rather than the full Reconcile.
	live.Status.TemplateDigest = digest

	if _, err := r.ReconcileHuskPodsForTest(ctx, &live, live.Spec.Template); err != nil {
		t.Fatalf("reconcileHuskPods: %v", err)
	}
	pods := listHuskPods(t, c, poolName)
	if len(pods) != 2 {
		t.Fatalf("want 2 warm husk pods, got %d", len(pods))
	}

	// Simulate every dormant husk crashlooping on the CURRENT digest (a
	// snapshot that built but does not restore).
	for i := range pods {
		p := &pods[i]
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:         "husk-stub",
			RestartCount: 5,
			State:        corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		}}
		if err := c.Status().Update(ctx, p); err != nil {
			t.Fatalf("patch husk pod %s into CrashLoopBackOff: %v", p.Name, err)
		}
	}

	now := metav1.NewTime(time.Now())
	r.DriveTemplateHealthForTest(ctx, &live, live.Spec.Template, pods, true, now)

	cond := templateHealthyCondition(&live)
	if cond == nil {
		t.Fatal("TemplateHealthy condition was not set")
	}
	if cond.Status != metav1.ConditionFalse || cond.Reason != "Rebuilding" {
		t.Fatalf("TemplateHealthy = %s/%s, want False/Rebuilding", cond.Status, cond.Reason)
	}
	if got := rec.CreateTemplateCount(); got != 2 {
		t.Fatalf("CreateTemplate count after the forced rebuild = %d, want 2 (1 initial build + 1 forced rebuild)", got)
	}
	if live.Status.RebuildAttempts != 1 {
		t.Fatalf("RebuildAttempts = %d, want 1", live.Status.RebuildAttempts)
	}
	if live.Status.LastRebuildTime == nil || !live.Status.LastRebuildTime.Time.Equal(now.Time) {
		t.Fatalf("LastRebuildTime = %v, want %v", live.Status.LastRebuildTime, now)
	}

	// The crashlooping pods on the bad digest were deleted so they recreate
	// against the fresh snapshot; envtest has no kubelet, so each is either
	// already gone or carries a DeletionTimestamp.
	for i := range pods {
		var got corev1.Pod
		err := c.Get(ctx, client.ObjectKeyFromObject(&pods[i]), &got)
		if err == nil && got.DeletionTimestamp == nil {
			t.Errorf("crashlooping husk pod %s was not deleted after the forced rebuild", pods[i].Name)
		} else if err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("get husk pod %s: %v", pods[i].Name, err)
		}
	}

	// A second evaluation with the SAME "now" (a hot reconcile loop landing
	// inside the backoff window) must NOT trigger another rebuild.
	r.DriveTemplateHealthForTest(ctx, &live, live.Spec.Template, pods, true, now)
	if got := rec.CreateTemplateCount(); got != 2 {
		t.Fatalf("CreateTemplate count after a second evaluation inside the backoff window = %d, want 2 (no double rebuild)", got)
	}
	if live.Status.RebuildAttempts != 1 {
		t.Fatalf("RebuildAttempts after the backoff-gated evaluation = %d, want 1 (unchanged)", live.Status.RebuildAttempts)
	}
}

func templateHealthyCondition(pool *v1.SandboxPool) *metav1.Condition {
	for i := range pool.Status.Conditions {
		if pool.Status.Conditions[i].Type == "TemplateHealthy" {
			return &pool.Status.Conditions[i]
		}
	}
	return nil
}
