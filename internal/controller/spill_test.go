package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// TestForkSpillsToNewPod pins the predicate that decides whether the one-time fork
// snapshot must include a `mem` file.
//
// A vmstate-only fork snapshot is restorable ONLY by a child co-located in the source
// pod (it boots its guest RAM from that pod's shared memfd). Any child that spills
// into its own pod can restore only from disk, so the snapshot must carry `mem` or
// that child never activates and the fork hangs forever.
func TestForkSpillsToNewPod(t *testing.T) {
	cases := []struct {
		name             string
		spawnInSourcePod bool
		budgetRemaining  int
		replicas         int
		want             bool
	}{
		{"multi-vm off: every child gets its own pod", false, 4, 1, true},
		{"multi-vm off, many children", false, 4, 6, true},
		{"all children fit the budget", true, 4, 4, false},
		{"fewer children than budget", true, 4, 2, false},
		{"one child over the budget spills", true, 4, 5, true},
		{"replicas 6 over budget 4 (the prod hang)", true, 4, 6, true},
		{"budget exhausted by other forks", true, 0, 1, true},
		{"single child, budget 1", true, 1, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := forkSpillsToNewPod(tc.spawnInSourcePod, tc.budgetRemaining, tc.replicas); got != tc.want {
				t.Errorf("forkSpillsToNewPod(%v, %d, %d) = %v, want %v",
					tc.spawnInSourcePod, tc.budgetRemaining, tc.replicas, got, tc.want)
			}
		})
	}
}

// TestForkReadyMessageNamesTheBlockedChild pins that a fork which cannot make progress
// says WHY, rather than repeating a bare "4/6 husk forks ready" forever.
//
// The fan-out loop skips a child whose pod is not Running+Ready. Before this, that
// skip was silent: a fork stuck because a spilled child could never activate showed a
// count and nothing else, with no reason, no event, and no failing condition. A caller
// had no way to tell "still coming up" from "will never come up" (mitos-run/mitos#872).
func TestForkReadyMessageNamesTheBlockedChild(t *testing.T) {
	cases := []struct {
		name                        string
		ready, replicas             int
		blockedChild, blockedReason string
		want                        string
	}{
		{"complete", 6, 6, "", "", "6/6 husk forks ready"},
		{"in progress, nothing blocked yet", 2, 6, "", "", "2/6 husk forks ready"},
		{
			"blocked child is named with its cause",
			4, 6, "fork-4", "pod not ready: ContainersNotReady",
			"4/6 husk forks ready; child fork-4 blocked: pod not ready: ContainersNotReady",
		},
		{"a complete fork never reports a blocked child", 6, 6, "fork-4", "stale", "6/6 husk forks ready"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := forkReadyMessage(tc.ready, tc.replicas, tc.blockedChild, tc.blockedReason)
			if got != tc.want {
				t.Errorf("forkReadyMessage(%d, %d, %q, %q)\n got: %q\nwant: %q",
					tc.ready, tc.replicas, tc.blockedChild, tc.blockedReason, got, tc.want)
			}
		})
	}
}

// TestHuskPodNotReadyReason pins that the reason a child pod is not usable is legible.
func TestHuskPodNotReadyReason(t *testing.T) {
	pending := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}}
	if got := huskPodNotReadyReason(pending); got != "pod phase Pending" {
		t.Errorf("pending pod reason = %q", got)
	}
	noIP := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	if got := huskPodNotReadyReason(noIP); got != "pod has no IP yet" {
		t.Errorf("no-IP pod reason = %q", got)
	}
	notReady := &corev1.Pod{Status: corev1.PodStatus{
		Phase:      corev1.PodRunning,
		PodIP:      "10.0.0.1",
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: "ContainersNotReady"}},
	}}
	if got := huskPodNotReadyReason(notReady); got != "pod not ready: ContainersNotReady" {
		t.Errorf("not-ready pod reason = %q", got)
	}
}
