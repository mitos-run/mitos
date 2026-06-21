package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"mitos.run/mitos/api/v1alpha2"
)

// TestEqualSandboxStatusDetectsObservedGenerationChange proves the status-write
// elision does not hide a stale observedGeneration. docs/conditions.md requires
// the Ready condition's observedGeneration to match the object's generation; if
// equalSandboxStatus ignores it, a spec change (generation bump) whose phase and
// message are unchanged elides the write and leaves observedGeneration pointing
// at the previous generation forever, so tooling that checks
// observedGeneration == metadata.generation reads the status as never-current.
func TestEqualSandboxStatusDetectsObservedGenerationChange(t *testing.T) {
	mk := func(gen int64) *v1alpha2.SandboxStatus {
		return &v1alpha2.SandboxStatus{
			Phase: "Ready",
			Conditions: []metav1.Condition{{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				Reason:             "Ready",
				Message:            "ok",
				ObservedGeneration: gen,
			}},
		}
	}
	if equalSandboxStatus(mk(1), mk(2)) {
		t.Fatal("a different Ready condition observedGeneration must compare unequal so the status write is not elided")
	}
	if !equalSandboxStatus(mk(3), mk(3)) {
		t.Fatal("identical statuses must compare equal (no spurious writes)")
	}
}
