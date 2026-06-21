package controller_test

// Envtest coverage for maxLifetime and idleTimeout reaping. A Ready claim with
// a Timeout (maxLifetime) reaches the terminal Terminated phase once its
// lifetime is exceeded; a claim with an IdleTimeout and no activity is reaped;
// a claim kept active is not reaped within the window. Reaping stamps
// FinishedAt and a Terminated condition, and forkd records the Terminate.

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	v1alpha1 "mitos.run/mitos/api/v1alpha1"
	"mitos.run/mitos/internal/controller"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// waitClaimTerminated polls until the named claim reaches the Terminated phase
// and returns it, failing the test if it does not within the window.
func waitClaimTerminated(t *testing.T, name string) *v1alpha1.SandboxClaim {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var got v1alpha1.SandboxClaim
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, &got); err == nil {
			if got.Status.Phase == v1alpha1.SandboxTerminated {
				return &got
			}
			if got.Status.Phase == v1alpha1.SandboxFailed {
				t.Fatalf("claim failed: %+v", got.Status)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("claim %s did not become Terminated within 20s", name)
	return nil
}

// terminatedCondition returns the Terminated condition's reason, or "".
func terminatedReason(claim *v1alpha1.SandboxClaim) string {
	for _, c := range claim.Status.Conditions {
		if c.Type == "Terminated" {
			return c.Reason
		}
	}
	return ""
}

func makeLifecycleClaim(t *testing.T, prefix string, spec v1alpha1.SandboxClaimSpec) *v1alpha1.SandboxClaim {
	t.Helper()
	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: prefix + "-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: prefix + "-pool", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: prefix + "-tmpl"},
			Replicas:    1,
		},
	}
	spec.PoolRef = v1alpha1.LocalObjectReference{Name: prefix + "-pool"}
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: prefix + "-claim", Namespace: "default"},
		Spec:       spec,
	}
	for _, obj := range []client.Object{template, pool, claim} {
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, claim)
		_ = k8sClient.Delete(ctx, pool)
		_ = k8sClient.Delete(ctx, template)
	})
	return claim
}

func TestClaimMaxLifetimeReaped(t *testing.T) {
	stop, engine, _, err := controller.StartFakeForkdNodeRecording(testRegistry, "life-node-1", "life1-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	makeLifecycleClaim(t, "life1", v1alpha1.SandboxClaimSpec{
		Timeout: &metav1.Duration{Duration: 2 * time.Second},
	})

	ready := waitClaimReady(t, "life1-claim")
	sandboxID := ready.Status.SandboxID

	got := waitClaimTerminated(t, "life1-claim")
	if r := terminatedReason(got); r != "MaxLifetimeExceeded" {
		t.Fatalf("terminated reason = %q, want MaxLifetimeExceeded", r)
	}
	if got.Status.FinishedAt == nil {
		t.Fatal("FinishedAt not stamped on terminated claim")
	}
	waitTerminated(t, engine, sandboxID)
}

func TestClaimIdleTimeoutReaped(t *testing.T) {
	stop, engine, _, err := controller.StartFakeForkdNodeRecording(testRegistry, "life-node-2", "life2-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	makeLifecycleClaim(t, "life2", v1alpha1.SandboxClaimSpec{
		IdleTimeout: &metav1.Duration{Duration: 2 * time.Second},
	})

	ready := waitClaimReady(t, "life2-claim")
	sandboxID := ready.Status.SandboxID

	got := waitClaimTerminated(t, "life2-claim")
	if r := terminatedReason(got); r != "IdleTimeout" {
		t.Fatalf("terminated reason = %q, want IdleTimeout", r)
	}
	if got.Status.FinishedAt == nil {
		t.Fatal("FinishedAt not stamped on terminated claim")
	}
	waitTerminated(t, engine, sandboxID)
}

func TestClaimIdleTimeoutNotReapedWhenActive(t *testing.T) {
	stop, _, setActivity, err := controller.StartFakeForkdNodeRecording(testRegistry, "life-node-3", "life3-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	makeLifecycleClaim(t, "life3", v1alpha1.SandboxClaimSpec{
		IdleTimeout: &metav1.Duration{Duration: 2 * time.Second},
	})

	ready := waitClaimReady(t, "life3-claim")
	sandboxID := ready.Status.SandboxID

	// Keep the sandbox active across the idle window: stamp recent activity
	// repeatedly so the controller never sees it as idle.
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(300 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				setActivity(sandboxID, time.Now())
			}
		}
	}()

	// Within the idle window plus margin, the claim must stay Ready.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var got v1alpha1.SandboxClaim
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "life3-claim", Namespace: "default"}, &got); err == nil {
			if got.Status.Phase == v1alpha1.SandboxTerminated {
				t.Fatalf("active claim was reaped: reason %q", terminatedReason(&got))
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestClaimIdleTimeoutNotReapedWithBackgroundJob is the work-aware idle
// regression (issue #218): a sandbox with NO inbound interaction but a live
// background job (an open stream, surfaced as active_streams via ListSandboxes)
// must NOT be idle-reaped mid-run. This is the Daytona weakness we beat. The
// background work is injected by holding an open stream slot on the node's
// SandboxAPI; the controller sees a non-zero active-streams count and keeps the
// claim Ready across the idle window.
func TestClaimIdleTimeoutNotReapedWithBackgroundJob(t *testing.T) {
	stop, _, api, err := controller.StartFakeForkdNodeWithAPI(testRegistry, "life-node-4", "life4-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	makeLifecycleClaim(t, "life4", v1alpha1.SandboxClaimSpec{
		IdleTimeout: &metav1.Duration{Duration: 2 * time.Second},
	})

	ready := waitClaimReady(t, "life4-claim")
	sandboxID := ready.Status.SandboxID

	// Inject a live background job: hold an open stream slot for this sandbox so
	// ListSandboxes reports active_streams > 0. No inbound interaction is
	// recorded, so an interaction-only idle clock would reap it.
	release := api.HoldStreamForTest(sandboxID)
	defer release()

	// Across the idle window plus margin, the claim with a live background job
	// must stay Ready.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var got v1alpha1.SandboxClaim
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "life4-claim", Namespace: "default"}, &got); err == nil {
			if got.Status.Phase == v1alpha1.SandboxTerminated {
				t.Fatalf("sandbox with a live background job was idle-reaped: reason %q", terminatedReason(&got))
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestClaimSetTimeoutExtendsLiveTTL asserts a live set_timeout extends a running
// sandbox's TTL: with a short idle timeout but a far-future live deadline set
// via the SandboxAPI, the sandbox is NOT reaped within the idle window because
// the caller explicitly extended its TTL (issue #218, the #206 set_timeout seam).
func TestClaimSetTimeoutExtendsLiveTTL(t *testing.T) {
	stop, _, api, err := controller.StartFakeForkdNodeWithAPI(testRegistry, "life-node-5", "life5-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	makeLifecycleClaim(t, "life5", v1alpha1.SandboxClaimSpec{
		IdleTimeout: &metav1.Duration{Duration: 2 * time.Second},
	})

	ready := waitClaimReady(t, "life5-claim")
	sandboxID := ready.Status.SandboxID

	// Live set_timeout to a far-future deadline keeps the sandbox alive past the
	// idle window. Set it synchronously first so no early reconcile can race the
	// idle clock before the deadline is recorded.
	api.SetTimeout(sandboxID, time.Hour)
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(300 * time.Millisecond)
		defer ticker.Stop()
		api.SetTimeout(sandboxID, time.Hour)
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				api.SetTimeout(sandboxID, time.Hour)
			}
		}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var got v1alpha1.SandboxClaim
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "life5-claim", Namespace: "default"}, &got); err == nil {
			if got.Status.Phase == v1alpha1.SandboxTerminated {
				t.Fatalf("sandbox with an extended live TTL was reaped: reason %q", terminatedReason(&got))
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
}
