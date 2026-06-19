package controller

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/paperclipinc/mitos/api/v1alpha1"
)

func rdyClaim() *v1alpha1.SandboxClaim {
	return &v1alpha1.SandboxClaim{Status: v1alpha1.SandboxClaimStatus{
		Phase: v1alpha1.SandboxReady, Node: "n1", Endpoint: "10.0.0.1:9091", SandboxID: "pod-1",
		Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "HuskActivated", LastTransitionTime: metav1.Now()}},
	}}
}
func rdyPod(ready bool) *corev1.Pod {
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	return &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.1", Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: st}}}}
}
func rdyCond(c *v1alpha1.SandboxClaim) (metav1.ConditionStatus, string) {
	for _, x := range c.Status.Conditions {
		if x.Type == "Ready" {
			return x.Status, x.Reason
		}
	}
	return "", ""
}

// TestReflectHuskBackingReadiness is the #177 readiness-accuracy unit: a Ready
// claim must reflect its backing pod going NotReady (node outage) in its Ready
// condition, and recover when the pod is Ready again, without churning otherwise.
func TestReflectHuskBackingReadiness(t *testing.T) {
	now := time.Now()

	// Ready pod: no change to a Ready=True condition.
	c := rdyClaim()
	if reflectHuskBackingReadiness(c, rdyPod(true), now) {
		t.Error("ready backing pod should not change the Ready condition")
	}
	if s, _ := rdyCond(c); s != metav1.ConditionTrue {
		t.Errorf("Ready=%s, want True", s)
	}

	// NotReady pod: flip Ready -> False with BackingPodNotReady.
	c = rdyClaim()
	if !reflectHuskBackingReadiness(c, rdyPod(false), now) {
		t.Fatal("NotReady backing pod should flip Ready to False")
	}
	if s, r := rdyCond(c); s != metav1.ConditionFalse || r != "BackingPodNotReady" {
		t.Errorf("got Ready=%s reason=%s, want False/BackingPodNotReady", s, r)
	}
	// Stable: a second NotReady pass does not churn.
	if reflectHuskBackingReadiness(c, rdyPod(false), now) {
		t.Error("already-reflected NotReady should not change again")
	}
	// Recovery: pod Ready again flips back to True.
	if !reflectHuskBackingReadiness(c, rdyPod(true), now) {
		t.Fatal("recovered backing pod should flip Ready back to True")
	}
	if s, _ := rdyCond(c); s != metav1.ConditionTrue {
		t.Errorf("Ready=%s after recovery, want True", s)
	}

	// nil pod (lost-but-not-yet-repended edge): treated as NotReady.
	c = rdyClaim()
	if !reflectHuskBackingReadiness(c, nil, now) {
		t.Error("nil backing pod should flip Ready to False")
	}
}
