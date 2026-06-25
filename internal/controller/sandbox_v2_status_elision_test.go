package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestConditionEqualDetectsObservedGenerationChange proves the status-write
// elision does not hide a stale observedGeneration. docs/conditions.md requires
// the Ready condition's observedGeneration to match the object's generation; if
// conditionEqual ignores it, a spec change (generation bump) whose phase and
// message are unchanged elides the write and leaves observedGeneration pointing
// at the previous generation forever, so tooling that checks
// observedGeneration == metadata.generation reads the status as never-current.
func TestConditionEqualDetectsObservedGenerationChange(t *testing.T) {
	mk := func(gen int64) []metav1.Condition {
		return []metav1.Condition{{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "Ready",
			Message:            "ok",
			ObservedGeneration: gen,
		}}
	}
	if conditionEqual(mk(1), mk(2), "Ready") {
		t.Fatal("a different Ready condition observedGeneration must compare unequal so the status write is not elided")
	}
	if !conditionEqual(mk(3), mk(3), "Ready") {
		t.Fatal("identical conditions must compare equal (no spurious writes)")
	}
}
