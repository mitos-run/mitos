package controller_test

// Envtest coverage for issue #688: a husk-backed claim that hits its lifetime
// bound must actually STOP the in-pod VM. terminateOnNode is a no-op for husk
// pods (forkd never tracks them), so terminateLifetime must delete the claimed
// husk pod, exactly as reconcileDelete does on object deletion. Without that
// the pod keeps Running with its claim+org labels, the usage scrape lister
// (labels plus Running, never claim phase) keeps returning it, and the sandbox
// keeps being billed as live after the platform said Terminated.

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/controller"
	"mitos.run/mitos/internal/husk"
	"mitos.run/mitos/internal/tenant"
)

// scrapeListerHasPod reports whether the usage HuskPodScrapeLister currently
// returns a husk pod with the given vm-id (pod name): the exact billable-set
// membership the collector uses each cycle.
func scrapeListerHasPod(t *testing.T, name string) bool {
	t.Helper()
	lister := &controller.HuskPodScrapeLister{Client: k8sClient}
	pods, err := lister.ListHuskPods(ctx)
	if err != nil {
		t.Fatalf("ListHuskPods: %v", err)
	}
	for _, p := range pods {
		if p.VMID == name {
			return true
		}
	}
	return false
}

func TestHuskClaimLifetimeTerminateDeletesPod(t *testing.T) {
	pod := makeDormantHuskPod(t, "husk-life-pool", "10.1.2.77")

	act := &fakeActivator{result: husk.ActivateResult{OK: true, VsockPath: "/run/husk/vm/vsock", LatencyMs: 1.0}}
	setHuskTestActivator(act.activate)
	t.Cleanup(func() { setHuskTestActivator(nil) })

	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "husk-life-pool", Namespace: "default"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}
	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, pool) })

	// The claim carries the org label the hosted gateway stamps (copied onto the
	// pod at claim time), so the pod is in the scrape lister's billable set, and
	// a short maxLifetime so the lifetime reaper fires.
	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "husk-life-claim",
			Namespace: "default",
			Labels: map[string]string{
				controller.HuskTestClaimLabel: "true",
				tenant.OrgLabelKey:            "org-life",
			},
		},
		Spec: v1.SandboxSpec{
			Source:   v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: "husk-life-pool"}},
			Lifetime: &v1.SandboxLifetime{TTL: &metav1.Duration{Duration: 2 * time.Second}},
		},
	}
	if err := k8sClient.Create(ctx, claim); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, claim) })

	waitClaimPhase(t, claim.Name, func(c *v1.Sandbox) bool {
		return c.Status.Phase == v1.SandboxReady
	})

	// Before the lifetime bound, the claimed org-labeled Running pod is in the
	// billable set.
	if !scrapeListerHasPod(t, pod.Name) {
		t.Fatalf("claimed husk pod %s not in the scrape lister's billable set while Ready", pod.Name)
	}

	got := waitClaimTerminated(t, claim.Name)
	if r := terminatedReason(got); r != "MaxLifetimeExceeded" {
		t.Fatalf("terminated reason = %q, want MaxLifetimeExceeded", r)
	}

	// The fix: the claimed husk pod must be DELETED at the lifetime terminate,
	// not just the claim stamped Terminated. Poll for it to vanish.
	gone := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var p corev1.Pod
		err := k8sClient.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: "default"}, &p)
		if apierrors.IsNotFound(err) {
			gone = true
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if !gone {
		t.Fatal("husk pod still exists after lifetime terminate; the in-pod VM keeps running, holding a warm slot, and BILLING (issue #688)")
	}

	// The scrape lister must no longer return the pod: billing stops at the
	// terminate instant (the tail is carried by the single termination event).
	if scrapeListerHasPod(t, pod.Name) {
		t.Fatal("scrape lister still returns the terminated sandbox's pod")
	}
}
