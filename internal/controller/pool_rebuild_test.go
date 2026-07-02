package controller

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// rebuildBackoff must grow 1m << (attempts-1), capped at 30m, with attempts 0
// and 1 both waiting the same 1m base interval before a second attempt is
// allowed (attempts is pre-increment: 0 before the first rebuild, 1 right
// after it).
func TestRebuildBackoff(t *testing.T) {
	tests := []struct {
		attempts int32
		want     time.Duration
	}{
		{0, time.Minute},
		{1, time.Minute},
		{2, 2 * time.Minute},
		{3, 4 * time.Minute},
		{4, 8 * time.Minute},
		{5, 16 * time.Minute},
		{6, 30 * time.Minute},
		{7, 30 * time.Minute},
		{100, 30 * time.Minute},
	}
	for _, tc := range tests {
		if got := rebuildBackoff(tc.attempts); got != tc.want {
			t.Errorf("rebuildBackoff(%d) = %s, want %s", tc.attempts, got, tc.want)
		}
	}
}

// crashloopPod builds a fixture dormant husk pod carrying the given
// template-digest annotation and container status.
func crashloopPod(name, digest string, restarts int32, reason string, ready bool) corev1.Pod {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: map[string]string{huskTemplateDigestAnnotation: digest},
		},
	}
	cs := corev1.ContainerStatus{
		Name:         huskContainerName,
		RestartCount: restarts,
		Ready:        ready,
	}
	if reason != "" {
		cs.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}
	} else if ready {
		cs.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{cs}
	return pod
}

// templateRestoreFailing keys on the CURRENT digest and a minFailingHusks (2)
// threshold: a single flaky crashlooper never trips it, a wrong-digest or
// healthy pod never counts toward it, and two independent crashloopers on the
// current digest do trip it.
func TestTemplateRestoreFailing(t *testing.T) {
	const digest = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	const otherDigest = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

	t.Run("two crashloopers on the current digest trip it", func(t *testing.T) {
		pods := []corev1.Pod{
			crashloopPod("husk-1", digest, 3, "CrashLoopBackOff", false),
			crashloopPod("husk-2", digest, 5, "CrashLoopBackOff", false),
		}
		if !templateRestoreFailing(pods, digest) {
			t.Fatal("want templateRestoreFailing true for two crashloopers on the current digest")
		}
	})

	t.Run("one crashlooper is below minFailingHusks", func(t *testing.T) {
		pods := []corev1.Pod{
			crashloopPod("husk-1", digest, 3, "CrashLoopBackOff", false),
		}
		if templateRestoreFailing(pods, digest) {
			t.Fatal("want templateRestoreFailing false for a single crashlooper (below minFailingHusks)")
		}
	})

	t.Run("wrong digest does not count", func(t *testing.T) {
		pods := []corev1.Pod{
			crashloopPod("husk-1", digest, 3, "CrashLoopBackOff", false),
			crashloopPod("husk-2", otherDigest, 5, "CrashLoopBackOff", false),
		}
		if templateRestoreFailing(pods, digest) {
			t.Fatal("want templateRestoreFailing false when only one pod matches the current digest")
		}
	})

	t.Run("ready pods on the current digest do not count", func(t *testing.T) {
		pods := []corev1.Pod{
			crashloopPod("husk-1", digest, 3, "CrashLoopBackOff", false),
			crashloopPod("husk-2", digest, 0, "", true),
			crashloopPod("husk-3", digest, 0, "", true),
		}
		if templateRestoreFailing(pods, digest) {
			t.Fatal("want templateRestoreFailing false when only one pod is actually crashlooping")
		}
	})

	t.Run("restartCount below crashloopThreshold does not count", func(t *testing.T) {
		pods := []corev1.Pod{
			crashloopPod("husk-1", digest, 2, "CrashLoopBackOff", false),
			crashloopPod("husk-2", digest, 2, "CrashLoopBackOff", false),
		}
		if templateRestoreFailing(pods, digest) {
			t.Fatal("want templateRestoreFailing false when restartCount is below crashloopThreshold")
		}
	})

	t.Run("empty digest never fails", func(t *testing.T) {
		pods := []corev1.Pod{
			crashloopPod("husk-1", "", 3, "CrashLoopBackOff", false),
			crashloopPod("husk-2", "", 5, "CrashLoopBackOff", false),
		}
		if templateRestoreFailing(pods, "") {
			t.Fatal("want templateRestoreFailing false for an empty (not-yet-built) digest")
		}
	})
}
