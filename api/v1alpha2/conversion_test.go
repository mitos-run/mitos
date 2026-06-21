package v1alpha2

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1alpha1 "mitos.run/mitos/api/v1alpha1"
)

// TestSandboxPoolRoundTripFromHub proves a v1alpha1 SandboxPool (the Hub)
// converts to v1alpha2 and back without losing any pool-level value. This is the
// load-bearing correctness for the migration table (docs/api/v2-migration.md):
// the warm/snapshots regrouping preserves every v1 value, only re-homing the
// field paths.
func TestSandboxPoolRoundTripFromHub(t *testing.T) {
	delay := &metav1.Duration{Duration: 0}
	hub := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef:            v1alpha1.LocalObjectReference{Name: "tmpl-a"},
			Replicas:               4,
			SnapshotAfter:          v1alpha1.SnapshotAfterReady,
			SnapshotDelay:          delay,
			ScaleDownAfterSnapshot: true,
			SnapshotStorage:        "/var/snap",
			DrainPolicy:            v1alpha1.DrainCheckpoint,
			Autoscale: &v1alpha1.PoolAutoscaleSpec{
				MinWarm:                  2,
				MaxWarm:                  50,
				TargetSpare:              3,
				ScaleDownCooldownSeconds: 120,
			},
			Placement: &v1alpha1.PoolPlacement{
				NodeSelector: map[string]string{"tenant": "a"},
			},
		},
		Status: v1alpha1.SandboxPoolStatus{ReadySnapshots: 4, TotalSnapshots: 4},
	}

	var v2 SandboxPool
	if err := v2.ConvertFrom(hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}

	// The template axis: a v1 templateRef becomes a v2 templateRef (a pure
	// conversion cannot inline the separate SandboxTemplate body).
	if v2.Spec.TemplateRef == nil || v2.Spec.TemplateRef.Name != "tmpl-a" {
		t.Fatalf("templateRef not carried: %+v", v2.Spec.TemplateRef)
	}
	if v2.Spec.Template != nil {
		t.Fatalf("inline template should be nil when v1 used templateRef")
	}
	// The warm axis: v1 autoscale maps onto warm.
	if v2.Spec.Warm == nil || v2.Spec.Warm.Min != 2 || v2.Spec.Warm.Max != 50 ||
		v2.Spec.Warm.TargetPending != 3 || v2.Spec.Warm.CooldownSeconds != 120 {
		t.Fatalf("warm not mapped from autoscale: %+v", v2.Spec.Warm)
	}
	// The snapshots axis.
	if v2.Spec.Snapshots == nil || v2.Spec.Snapshots.ReplicasPerNode != 4 ||
		v2.Spec.Snapshots.SnapshotAfter != v1alpha1.SnapshotAfterReady ||
		!v2.Spec.Snapshots.ScaleDownAfterSnapshot || v2.Spec.Snapshots.Storage != "/var/snap" {
		t.Fatalf("snapshots not mapped: %+v", v2.Spec.Snapshots)
	}
	if v2.Spec.DrainPolicy != v1alpha1.DrainCheckpoint {
		t.Fatalf("drainPolicy not carried: %q", v2.Spec.DrainPolicy)
	}
	if v2.Spec.Placement == nil || v2.Spec.Placement.NodeSelector["tenant"] != "a" {
		t.Fatalf("placement not carried: %+v", v2.Spec.Placement)
	}

	// Round-trip back to the hub: every value preserved.
	var back v1alpha1.SandboxPool
	if err := v2.ConvertTo(&back); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	if back.Spec.TemplateRef.Name != "tmpl-a" {
		t.Fatalf("templateRef lost on round-trip: %+v", back.Spec.TemplateRef)
	}
	if back.Spec.Replicas != 4 {
		t.Fatalf("replicas lost: %d", back.Spec.Replicas)
	}
	if back.Spec.SnapshotAfter != v1alpha1.SnapshotAfterReady || !back.Spec.ScaleDownAfterSnapshot ||
		back.Spec.SnapshotStorage != "/var/snap" {
		t.Fatalf("snapshot fields lost: %+v", back.Spec)
	}
	if back.Spec.DrainPolicy != v1alpha1.DrainCheckpoint {
		t.Fatalf("drainPolicy lost: %q", back.Spec.DrainPolicy)
	}
	if back.Spec.Autoscale == nil || back.Spec.Autoscale.MinWarm != 2 || back.Spec.Autoscale.MaxWarm != 50 ||
		back.Spec.Autoscale.TargetSpare != 3 || back.Spec.Autoscale.ScaleDownCooldownSeconds != 120 {
		t.Fatalf("autoscale lost on round-trip: %+v", back.Spec.Autoscale)
	}
	if back.Spec.Placement == nil || back.Spec.Placement.NodeSelector["tenant"] != "a" {
		t.Fatalf("placement lost on round-trip: %+v", back.Spec.Placement)
	}
	if back.ObjectMeta.Name != "p" || back.Status.ReadySnapshots != 4 {
		t.Fatalf("meta/status lost: %+v / %+v", back.ObjectMeta, back.Status)
	}
}

// TestSandboxPoolNoAutoscaleMapsReplicasToWarmMin proves a v1 pool with no
// autoscale block maps its fixed replicas onto warm.min (the documented
// back-compat mapping), and round-trips back to the same replicas with no
// autoscale fabricated.
func TestSandboxPoolNoAutoscaleMapsReplicasToWarmMin(t *testing.T) {
	hub := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "fixed"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "t"},
			Replicas:    3,
		},
	}
	var v2 SandboxPool
	if err := v2.ConvertFrom(hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	if v2.Spec.Warm == nil || v2.Spec.Warm.Min != 3 {
		t.Fatalf("fixed replicas should map to warm.min=3: %+v", v2.Spec.Warm)
	}
	var back v1alpha1.SandboxPool
	if err := v2.ConvertTo(&back); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	if back.Spec.Replicas != 3 {
		t.Fatalf("replicas lost: %d", back.Spec.Replicas)
	}
	if back.Spec.Autoscale != nil {
		t.Fatalf("no autoscale should be fabricated for a fixed pool: %+v", back.Spec.Autoscale)
	}
}

// TestSandboxPoolInlineTemplateConvertsToSyntheticRef proves a v2 pool authored
// with an inline template converts to a v1 pool with a templateRef whose name is
// derived from the pool (the documented default conversion: the inline template
// inlines into the single pool that referenced it; on the way down to the v1
// model it becomes a ref the storage migration would materialize). The inline
// body is preserved on the v2 object across a round-trip so no inline field is
// lost when the source is already v2.
func TestSandboxPoolInlineTemplateConvertsToSyntheticRef(t *testing.T) {
	v2 := &SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "inline-pool"},
		Spec: SandboxPoolSpec{
			Template: &PoolTemplateSpec{
				Image:     "ghcr.io/x/y:1",
				Init:      []string{"pip install numpy"},
				Encrypted: true,
			},
			Warm: &PoolWarm{Min: 1},
		},
	}
	var hub v1alpha1.SandboxPool
	if err := v2.ConvertTo(&hub); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	if hub.Spec.TemplateRef.Name == "" {
		t.Fatalf("inline template must yield a non-empty synthetic templateRef")
	}
	if hub.Spec.Replicas != 1 {
		t.Fatalf("warm.min should map to replicas: %d", hub.Spec.Replicas)
	}
}
