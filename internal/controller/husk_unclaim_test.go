package controller_test

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/controller"
	"mitos.run/mitos/internal/husk"
)

// TestHuskActivationFailureReleasesClaimLabel is the regression for the orphaned
// husk pod half of issue #177. The claim path stamps the mitos.run/claim label
// BEFORE activating (the mutual-exclusion commit). If activation then fails
// (a transient transport error, or a per-node-digest mismatch when failing over
// to another node), the label must be RELEASED so the pod returns to the dormant
// pool. Without that, the stamped-but-never-activated pod is excluded from
// selectDormantHuskPod forever, leaking warm capacity and blocking failover (the
// claim could find "no dormant pod" while a healthy pod sits orphaned).
func TestHuskActivationFailureReleasesClaimLabel(t *testing.T) {
	testRegistry.Register(&controller.NodeInfo{
		Name:            "kvm-node-1",
		TemplateIDs:     []string{"huskfail-tmpl"},
		TemplateDigests: map[string]string{"huskfail-tmpl": "4444444444444444444444444444444444444444444444444444444444444444"},
	})
	t.Cleanup(func() { testRegistry.Unregister("kvm-node-1") })

	pod := makeDormantHuskPod(t, "huskfail-pool", "10.1.2.12")

	// Activator that always FAILS (fail-closed, !OK).
	act := &fakeActivator{result: husk.ActivateResult{OK: false, Error: "simulated activation failure"}}
	setHuskTestActivator(act.activate)
	t.Cleanup(func() { setHuskTestActivator(nil) })

	_ = makeHuskClaim(t, "huskfail", v1.SandboxSpec{})

	// The claim cannot go Ready. The pod must be released back to dormant: poll
	// for the claim label to be cleared (the fix releases it after each failed
	// activation). Without the fix the label stays set forever -> this times out.
	released := false
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		var p corev1.Pod
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &p); err == nil {
			if p.Labels["mitos.run/claim"] == "" {
				released = true
				break
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	if !released {
		t.Error("husk pod claim label was never released after a failed activation; the pod is orphaned (#177)")
	}
}
