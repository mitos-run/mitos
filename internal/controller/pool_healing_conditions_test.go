package controller_test

// Coverage for Task 4 of the self-healing template rebuild plan (#584,
// closes #578): the TemplateBuilt condition, which surfaces ensureTemplateBuilt's
// result on the pool object, and the mitos.run/force-rebuild annotation, the
// operator recovery lever that bypasses rebuildBackoff on demand.

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/controller"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// A template build failure (here: an Encrypted template with no KMS
// configured, the same fail-closed error templateEncKey already returns) must
// surface as TemplateBuilt=False/BuildFailed with the error text, and a
// successful build as TemplateBuilt=True/Built. This is the condition side of
// issue #578: before this, a build error was only logged, never visible on
// the pool object.
func TestTemplateBuiltConditionSurfacesBuildError(t *testing.T) {
	c := k8sClient

	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "template-built-pool", Namespace: "default"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim", Encrypted: true},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}
	if err := c.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Delete(ctx, pool) })

	reg := controller.NewNodeRegistry()
	stop, err := controller.StartFakeForkdNode(reg, "template-built-node")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)

	// No KMS configured: an Encrypted template cannot resolve a wrapped DEK,
	// so ensureTemplateBuilt fails closed (enc_key_secret.go's "no KMS
	// configured to wrap the DEK" error).
	r := &controller.SandboxPoolReconciler{
		Client:       c,
		NodeRegistry: reg,
	}

	var live v1.SandboxPool
	if err := c.Get(ctx, client.ObjectKeyFromObject(pool), &live); err != nil {
		t.Fatal(err)
	}

	buildErr := r.EnsureTemplateBuiltForTest(ctx, &live, live.Spec.Template)
	if buildErr == nil {
		t.Fatal("want a build error for an Encrypted template with no KMS configured")
	}

	now := metav1.NewTime(time.Now())
	cond := controller.TemplateBuiltConditionForTest(buildErr, now)
	if cond.Type != "TemplateBuilt" {
		t.Fatalf("condition type = %s, want TemplateBuilt", cond.Type)
	}
	if cond.Status != metav1.ConditionFalse || cond.Reason != "BuildFailed" {
		t.Fatalf("TemplateBuilt = %s/%s, want False/BuildFailed", cond.Status, cond.Reason)
	}
	if cond.Message != buildErr.Error() {
		t.Fatalf("TemplateBuilt message = %q, want the build error text %q", cond.Message, buildErr.Error())
	}

	// A successful build (a plain, non-encrypted template against the same
	// fake forkd node) must report True/Built.
	pool2 := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "template-built-pool-ok", Namespace: "default"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}
	if err := c.Create(ctx, pool2); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Delete(ctx, pool2) })

	var live2 v1.SandboxPool
	if err := c.Get(ctx, client.ObjectKeyFromObject(pool2), &live2); err != nil {
		t.Fatal(err)
	}
	if err := r.EnsureTemplateBuiltForTest(ctx, &live2, live2.Spec.Template); err != nil {
		t.Fatalf("ensureTemplateBuilt (plaintext, healthy node): %v", err)
	}
	okCond := controller.TemplateBuiltConditionForTest(nil, now)
	if okCond.Status != metav1.ConditionTrue || okCond.Reason != "Built" {
		t.Fatalf("TemplateBuilt = %s/%s, want True/Built", okCond.Status, okCond.Reason)
	}
}

// The mitos.run/force-rebuild annotation (#584, #578) is the operator
// recovery lever: setting it to a new value forces an immediate rebuild that
// bypasses rebuildBackoff, and stamps Status.ForceRebuildHandled so exactly
// one rebuild happens per distinct annotation value. It must NOT touch
// RebuildAttempts or LastRebuildTime (those belong to the automatic path).
func TestForceRebuildAnnotationTriggersExactlyOneRebuildPerValue(t *testing.T) {
	c := k8sClient
	const (
		poolName = "force-rebuild-pool"
		node     = "force-rebuild-node"
	)

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
	t.Cleanup(func() { _ = c.Delete(ctx, pool) })

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

	var live v1.SandboxPool
	if err := c.Get(ctx, client.ObjectKeyFromObject(pool), &live); err != nil {
		t.Fatal(err)
	}

	// Build the initial snapshot so a holder (and a digest) exists to rebuild.
	if err := r.EnsureTemplateBuiltForTest(ctx, &live, live.Spec.Template); err != nil {
		t.Fatalf("ensureTemplateBuilt: %v", err)
	}
	if got := rec.CreateTemplateCount(); got != 1 {
		t.Fatalf("CreateTemplate count after the first build = %d, want 1", got)
	}
	digest, ok := reg.TemplateDigest(poolName)
	if !ok || digest == "" {
		t.Fatal("no digest recorded after the first build")
	}
	live.Status.TemplateDigest = digest

	now := metav1.NewTime(time.Now())

	// No annotation yet: driveForceRebuild must be a no-op.
	if r.DriveForceRebuildForTest(ctx, &live, live.Spec.Template, nil, now) {
		t.Fatal("driveForceRebuild triggered with no annotation set")
	}
	if got := rec.CreateTemplateCount(); got != 1 {
		t.Fatalf("CreateTemplate count with no annotation set = %d, want 1 (unchanged)", got)
	}

	// Set the annotation: exactly one rebuild.
	if live.Annotations == nil {
		live.Annotations = map[string]string{}
	}
	live.Annotations[controller.ForceRebuildAnnotationForTest] = "1000"
	if !r.DriveForceRebuildForTest(ctx, &live, live.Spec.Template, nil, now) {
		t.Fatal("driveForceRebuild did not trigger on a new annotation value")
	}
	if got := rec.CreateTemplateCount(); got != 2 {
		t.Fatalf("CreateTemplate count after the first force-rebuild = %d, want 2", got)
	}
	if live.Status.ForceRebuildHandled != "1000" {
		t.Fatalf("ForceRebuildHandled = %q, want %q", live.Status.ForceRebuildHandled, "1000")
	}
	if live.Status.RebuildAttempts != 0 {
		t.Fatalf("RebuildAttempts = %d, want 0 (a forced rebuild must not count toward the automatic backoff)", live.Status.RebuildAttempts)
	}
	if live.Status.LastRebuildTime != nil {
		t.Fatalf("LastRebuildTime = %v, want nil (a forced rebuild must not touch the automatic backoff clock)", live.Status.LastRebuildTime)
	}
	cond := templateHealthyCondition(&live)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "ForceRebuilding" {
		t.Fatalf("TemplateHealthy after force-rebuild = %+v, want False/ForceRebuilding", cond)
	}

	// A second evaluation with the SAME annotation value must NOT re-trigger.
	if r.DriveForceRebuildForTest(ctx, &live, live.Spec.Template, nil, now) {
		t.Fatal("driveForceRebuild re-triggered for an already-handled annotation value")
	}
	if got := rec.CreateTemplateCount(); got != 2 {
		t.Fatalf("CreateTemplate count after a repeat evaluation of the same value = %d, want 2 (unchanged)", got)
	}

	// Changing the annotation value triggers exactly one more rebuild.
	live.Annotations[controller.ForceRebuildAnnotationForTest] = "2000"
	if !r.DriveForceRebuildForTest(ctx, &live, live.Spec.Template, nil, now) {
		t.Fatal("driveForceRebuild did not trigger on a changed annotation value")
	}
	if got := rec.CreateTemplateCount(); got != 3 {
		t.Fatalf("CreateTemplate count after the second force-rebuild = %d, want 3", got)
	}
	if live.Status.ForceRebuildHandled != "2000" {
		t.Fatalf("ForceRebuildHandled = %q, want %q", live.Status.ForceRebuildHandled, "2000")
	}
}
