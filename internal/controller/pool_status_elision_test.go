package controller

import (
	"context"
	v1 "mitos.run/mitos/api/v1"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestPoolStatusUnchanged proves the change-detection that gates a status write
// (issue #163): a status differing only in the LastSnapshotTime heartbeat is
// "unchanged", while a change to any field an operator or the autoscaler reads is
// detected.
func TestPoolStatusUnchanged(t *testing.T) {
	t1 := metav1.Now()
	base := v1.SandboxPoolStatus{
		ReadySnapshots:   2,
		TotalSnapshots:   2,
		DesiredWarm:      2,
		TemplateDigest:   "sha256:abc",
		NodeDistribution: map[string]int32{"n1": 1, "n2": 1},
		LastSnapshotTime: &t1,
		Conditions: []metav1.Condition{{
			Type: "Ready", Status: metav1.ConditionTrue, Reason: "HuskPodsReady",
			Message: "2/2 warm husk pods", LastTransitionTime: t1,
		}},
	}

	// Only the heartbeat differs: unchanged.
	t2 := metav1.NewTime(t1.Add(time.Minute))
	bumped := base.DeepCopy()
	bumped.LastSnapshotTime = &t2
	if !poolStatusUnchanged(&base, bumped) {
		t.Fatal("statuses differing only in LastSnapshotTime must be reported unchanged")
	}

	// Each meaningful field change must be detected.
	mutations := map[string]func(*v1.SandboxPoolStatus){
		"ReadySnapshots":   func(s *v1.SandboxPoolStatus) { s.ReadySnapshots = 1 },
		"TemplateDigest":   func(s *v1.SandboxPoolStatus) { s.TemplateDigest = "sha256:def" },
		"NodeDistribution": func(s *v1.SandboxPoolStatus) { s.NodeDistribution = map[string]int32{"n1": 2} },
		"ConditionMessage": func(s *v1.SandboxPoolStatus) { s.Conditions[0].Message = "1/2 warm husk pods" },
		"ConditionStatus":  func(s *v1.SandboxPoolStatus) { s.Conditions[0].Status = metav1.ConditionFalse },
		"DesiredWarm":      func(s *v1.SandboxPoolStatus) { s.DesiredWarm = 3 },
		"ScaleDownTime":    func(s *v1.SandboxPoolStatus) { s.LastScaleDownTime = &t2 },
	}
	for name, mut := range mutations {
		changed := base.DeepCopy()
		mut(changed)
		if poolStatusUnchanged(&base, changed) {
			t.Fatalf("%s change must be detected as a status difference", name)
		}
	}
}

// TestWritePoolStatusIfChangedElidesNoOp proves the wiring (issue #163): a no-op
// reconcile does NOT write status (the object's resourceVersion is stable and the
// LastSnapshotTime heartbeat is not stamped), while a real change writes and
// stamps the heartbeat.
func TestWritePoolStatusIfChangedElidesNoOp(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Status:     v1.SandboxPoolStatus{ReadySnapshots: 1, TotalSnapshots: 1},
	}
	c := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1.SandboxPool{}).
		WithObjects(pool).
		Build()
	r := &SandboxPoolReconciler{Client: c}
	ctx := context.Background()

	var live v1.SandboxPool
	if err := c.Get(ctx, client.ObjectKeyFromObject(pool), &live); err != nil {
		t.Fatal(err)
	}
	rv0 := live.ResourceVersion

	// No-op: status identical to the stored object, so no write.
	before := live.Status.DeepCopy()
	if err := r.writePoolStatusIfChanged(ctx, &live, before, metav1.Now()); err != nil {
		t.Fatalf("writePoolStatusIfChanged (no-op): %v", err)
	}
	var afterNoop v1.SandboxPool
	if err := c.Get(ctx, client.ObjectKeyFromObject(pool), &afterNoop); err != nil {
		t.Fatal(err)
	}
	if afterNoop.ResourceVersion != rv0 {
		t.Fatalf("no-op reconcile wrote status: resourceVersion %s -> %s", rv0, afterNoop.ResourceVersion)
	}
	if afterNoop.Status.LastSnapshotTime != nil {
		t.Fatal("no-op reconcile must not stamp LastSnapshotTime")
	}

	// Real change: ReadySnapshots moves, so the status must be written and the
	// heartbeat stamped.
	before2 := live.Status.DeepCopy()
	live.Status.ReadySnapshots = 5
	if err := r.writePoolStatusIfChanged(ctx, &live, before2, metav1.Now()); err != nil {
		t.Fatalf("writePoolStatusIfChanged (change): %v", err)
	}
	var afterChange v1.SandboxPool
	if err := c.Get(ctx, client.ObjectKeyFromObject(pool), &afterChange); err != nil {
		t.Fatal(err)
	}
	if afterChange.ResourceVersion == rv0 {
		t.Fatal("a real status change did not write (resourceVersion unchanged)")
	}
	if afterChange.Status.ReadySnapshots != 5 {
		t.Fatalf("ReadySnapshots = %d, want 5", afterChange.Status.ReadySnapshots)
	}
	if afterChange.Status.LastSnapshotTime == nil {
		t.Fatal("a real change must stamp LastSnapshotTime")
	}
}
