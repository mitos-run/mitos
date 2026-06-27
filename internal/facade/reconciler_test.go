package facade_test

import (
	"testing"

	agentsv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	runv1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/facade"
)

// newSandbox builds a minimal valid upstream Sandbox: podTemplate is required,
// so it carries a single container. The optional annotations let a test set the
// mitos.run/pool bridge annotation. operatingMode defaults to Running (empty).
func newSandbox(name string, annotations map[string]string, mode agentsv1beta1.SandboxOperatingMode) *agentsv1beta1.Sandbox {
	return &agentsv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "default",
			Annotations: annotations,
		},
		Spec: agentsv1beta1.SandboxSpec{
			OperatingMode: mode,
			PodTemplate: agentsv1beta1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "agent",
							Image: "busybox:latest",
							Env:   []corev1.EnvVar{{Name: "FOO", Value: "bar"}},
						},
					},
				},
			},
		},
	}
}

func getClaim(t *testing.T, name string) (*runv1.Sandbox, bool) {
	t.Helper()
	var claim runv1.Sandbox
	err := k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: "default"}, &claim)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("get claim %s: %v", name, err)
	}
	return &claim, true
}

func getSandbox(t *testing.T, name string) *agentsv1beta1.Sandbox {
	t.Helper()
	var sb agentsv1beta1.Sandbox
	if err := k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: "default"}, &sb); err != nil {
		t.Fatalf("get sandbox %s: %v", name, err)
	}
	return &sb
}

// TestFacadeCreatesClaimWithBridgeAnnotation: a Sandbox with the
// mitos.run/pool annotation drives the facade to create our SandboxClaim,
// bound to the annotated pool, owner-referenced to the Sandbox.
func TestFacadeCreatesClaimWithBridgeAnnotation(t *testing.T) {
	sb := newSandbox("facade-annotated", map[string]string{facade.PoolAnnotation: "my-pool"}, "")
	if err := k8sClient.Create(testCtx, sb); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, sb) })

	var claim *runv1.Sandbox
	eventually(t, "facade creates the SandboxClaim", func() bool {
		c, ok := getClaim(t, "facade-annotated")
		claim = c
		return ok
	})

	if claim.Spec.Source.PoolRef.Name != "my-pool" {
		t.Fatalf("claim poolRef = %q, want my-pool", claim.Spec.Source.PoolRef.Name)
	}
	if claim.Annotations[facade.PoolAnnotation] != "my-pool" {
		t.Fatalf("claim bridge annotation = %q, want my-pool", claim.Annotations[facade.PoolAnnotation])
	}
	// Owner reference back to the Sandbox for GC + the watch back-link.
	if len(claim.OwnerReferences) != 1 || claim.OwnerReferences[0].Kind != "Sandbox" || claim.OwnerReferences[0].Name != "facade-annotated" {
		t.Fatalf("claim owner references = %+v, want a single Sandbox owner", claim.OwnerReferences)
	}
	// podTemplate env mirrored onto the claim.
	if len(claim.Spec.Env) != 1 || claim.Spec.Env[0].Name != "FOO" || claim.Spec.Env[0].Value != "bar" {
		t.Fatalf("claim env = %+v, want FOO=bar from podTemplate", claim.Spec.Env)
	}
}

// TestFacadeUsesDefaultPoolWhenUnannotated: a Sandbox with no bridge annotation
// binds to the facade's configured --default-pool.
func TestFacadeUsesDefaultPoolWhenUnannotated(t *testing.T) {
	sb := newSandbox("facade-default", nil, "")
	if err := k8sClient.Create(testCtx, sb); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, sb) })

	var claim *runv1.Sandbox
	eventually(t, "facade creates the SandboxClaim with the default pool", func() bool {
		c, ok := getClaim(t, "facade-default")
		claim = c
		return ok && c.Spec.Source.PoolRef.Name == "default-pool"
	})
	if claim.Spec.Source.PoolRef.Name != "default-pool" {
		t.Fatalf("claim poolRef = %q, want default-pool", claim.Spec.Source.PoolRef.Name)
	}
}

// TestFacadeMirrorsReadyIntoSandboxStatus: when our SandboxClaim reaches phase
// Ready, the facade mirrors a Ready=True condition + serviceFQDN
// into the upstream Sandbox status.
func TestFacadeMirrorsReadyIntoSandboxStatus(t *testing.T) {
	sb := newSandbox("facade-ready", map[string]string{facade.PoolAnnotation: "p"}, "")
	if err := k8sClient.Create(testCtx, sb); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, sb) })

	var claim *runv1.Sandbox
	eventually(t, "facade creates the SandboxClaim", func() bool {
		c, ok := getClaim(t, "facade-ready")
		claim = c
		return ok
	})

	// Drive our claim Ready via the status subresource (the test seam: the real
	// husk activation path sets this phase).
	statusUpdateWithRetry(t, types.NamespacedName{Name: "facade-ready", Namespace: "default"}, claim, func() {
		claim.Status.Phase = runv1.SandboxReady
		claim.Status.Endpoint = "10.0.0.5:9091"
	})

	eventually(t, "sandbox status mirrors Ready=True", func() bool {
		got := getSandbox(t, "facade-ready")
		cond := apimeta.FindStatusCondition(got.Status.Conditions, string(agentsv1beta1.SandboxConditionReady))
		return cond != nil && cond.Status == metav1.ConditionTrue
	})

	got := getSandbox(t, "facade-ready")
	if got.Status.ServiceFQDN != "facade-ready.default.svc.cluster.local" {
		t.Fatalf("serviceFQDN = %q, want facade-ready.default.svc.cluster.local", got.Status.ServiceFQDN)
	}
	// A Running, Ready sandbox must NOT report Suspended=True.
	if susp := apimeta.FindStatusCondition(got.Status.Conditions, string(agentsv1beta1.SandboxConditionSuspended)); susp != nil && susp.Status == metav1.ConditionTrue {
		t.Fatalf("Suspended condition = True on a Running/Ready sandbox, want False/absent")
	}
}

// TestFacadeSuspendedTerminatesClaim: setting a Sandbox to operatingMode
// Suspended terminates our run-path object (deletes the SandboxClaim) and
// sets the Suspended condition.
func TestFacadeSuspendedTerminatesClaim(t *testing.T) {
	sb := newSandbox("facade-scale", map[string]string{facade.PoolAnnotation: "p"}, "")
	if err := k8sClient.Create(testCtx, sb); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, sb) })

	eventually(t, "facade creates the SandboxClaim", func() bool {
		_, ok := getClaim(t, "facade-scale")
		return ok
	})

	// Set operatingMode to Suspended.
	var cur agentsv1beta1.Sandbox
	updateWithRetry(t, types.NamespacedName{Name: "facade-scale", Namespace: "default"}, &cur, func() {
		cur.Spec.OperatingMode = agentsv1beta1.SandboxOperatingModeSuspended
	})

	eventually(t, "claim terminated on Suspended", func() bool {
		_, ok := getClaim(t, "facade-scale")
		return !ok
	})

	eventually(t, "sandbox status reports Suspended and Ready=False", func() bool {
		got := getSandbox(t, "facade-scale")
		ready := apimeta.FindStatusCondition(got.Status.Conditions, string(agentsv1beta1.SandboxConditionReady))
		susp := apimeta.FindStatusCondition(got.Status.Conditions, string(agentsv1beta1.SandboxConditionSuspended))
		return ready != nil && ready.Status == metav1.ConditionFalse &&
			susp != nil && susp.Status == metav1.ConditionTrue
	})
}

// driveClaimReady drives our claim to phase Ready with an endpoint via the
// status subresource (the test seam the real husk activation path sets), then
// waits for the facade to mirror Ready=True + the endpoint observables onto the
// Sandbox status.
func driveClaimReady(t *testing.T, name string) {
	t.Helper()
	var claim runv1.Sandbox
	statusUpdateWithRetry(t, types.NamespacedName{Name: name, Namespace: "default"}, &claim, func() {
		claim.Status.Phase = runv1.SandboxReady
		claim.Status.Endpoint = "10.0.0.5:9091"
	})
	eventually(t, "facade mirrors Ready + endpoint for "+name, func() bool {
		got := getSandbox(t, name)
		cond := apimeta.FindStatusCondition(got.Status.Conditions, string(agentsv1beta1.SandboxConditionReady))
		susp := apimeta.FindStatusCondition(got.Status.Conditions, string(agentsv1beta1.SandboxConditionSuspended))
		return cond != nil && cond.Status == metav1.ConditionTrue &&
			(susp == nil || susp.Status == metav1.ConditionFalse) &&
			got.Status.ServiceFQDN != "" && len(got.Status.PodIPs) == 1
	})
}

// TestFacadePauseReleasesEndpointObservables: setting a Sandbox to
// operatingMode Suspended (pause) RELEASES the run path to the warm pool
// (deletes the bridged claim so the husk pod returns dormant) and clears the
// conformant serving observables: Ready False, serviceFQDN cleared, podIPs
// cleared. Suspended=True is set on the status.
func TestFacadePauseReleasesEndpointObservables(t *testing.T) {
	sb := newSandbox("facade-pause", map[string]string{facade.PoolAnnotation: "p"}, "")
	if err := k8sClient.Create(testCtx, sb); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, sb) })

	eventually(t, "facade creates the SandboxClaim", func() bool {
		_, ok := getClaim(t, "facade-pause")
		return ok
	})
	// Activate: the run path is Ready with a serving endpoint (serviceFQDN +
	// podIPs populated).
	driveClaimReady(t, "facade-pause")

	// Pause: operatingMode Suspended.
	var cur agentsv1beta1.Sandbox
	updateWithRetry(t, types.NamespacedName{Name: "facade-pause", Namespace: "default"}, &cur, func() {
		cur.Spec.OperatingMode = agentsv1beta1.SandboxOperatingModeSuspended
	})

	eventually(t, "claim released to the warm pool on pause", func() bool {
		_, ok := getClaim(t, "facade-pause")
		return !ok
	})
	eventually(t, "paused sandbox clears the serving observables", func() bool {
		got := getSandbox(t, "facade-pause")
		ready := apimeta.FindStatusCondition(got.Status.Conditions, string(agentsv1beta1.SandboxConditionReady))
		susp := apimeta.FindStatusCondition(got.Status.Conditions, string(agentsv1beta1.SandboxConditionSuspended))
		return ready != nil && ready.Status == metav1.ConditionFalse &&
			susp != nil && susp.Status == metav1.ConditionTrue &&
			got.Status.ServiceFQDN == "" && len(got.Status.PodIPs) == 0
	})
}

// TestFacadeResumeReactivates: resuming a paused Sandbox (operatingMode Running
// after Suspended) RE-ACTIVATES via the warm-pool fast path: the facade
// re-creates the bridged husk-backed claim (the same activation as create), and
// once the run path is Ready it re-populates the serving observables.
func TestFacadeResumeReactivates(t *testing.T) {
	sb := newSandbox("facade-resume", map[string]string{facade.PoolAnnotation: "p"}, "")
	if err := k8sClient.Create(testCtx, sb); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, sb) })

	eventually(t, "facade creates the SandboxClaim", func() bool {
		_, ok := getClaim(t, "facade-resume")
		return ok
	})
	driveClaimReady(t, "facade-resume")

	// Pause.
	var cur agentsv1beta1.Sandbox
	updateWithRetry(t, types.NamespacedName{Name: "facade-resume", Namespace: "default"}, &cur, func() {
		cur.Spec.OperatingMode = agentsv1beta1.SandboxOperatingModeSuspended
	})
	eventually(t, "claim released on pause", func() bool {
		_, ok := getClaim(t, "facade-resume")
		return !ok
	})

	// Resume: operatingMode Running.
	updateWithRetry(t, types.NamespacedName{Name: "facade-resume", Namespace: "default"}, &cur, func() {
		cur.Spec.OperatingMode = agentsv1beta1.SandboxOperatingModeRunning
	})
	eventually(t, "claim re-activated on resume", func() bool {
		_, ok := getClaim(t, "facade-resume")
		return ok
	})
	// The re-activated claim reaches Ready and re-populates the observables.
	driveClaimReady(t, "facade-resume")
}

// TestFacadePauseResumeToggleStable: a Running->Suspended->Running->Suspended
// toggle is stable and idempotent: each Suspended releases the claim + clears
// the observables, each Running re-activates the claim, and a re-applied
// identical state is a no-op.
func TestFacadePauseResumeToggleStable(t *testing.T) {
	sb := newSandbox("facade-toggle", map[string]string{facade.PoolAnnotation: "p"}, "")
	if err := k8sClient.Create(testCtx, sb); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, sb) })

	setMode := func(mode agentsv1beta1.SandboxOperatingMode) {
		var cur agentsv1beta1.Sandbox
		updateWithRetry(t, types.NamespacedName{Name: "facade-toggle", Namespace: "default"}, &cur, func() {
			cur.Spec.OperatingMode = mode
		})
	}
	claimPresent := func() bool {
		_, ok := getClaim(t, "facade-toggle")
		return ok
	}
	claimAbsent := func() bool {
		_, ok := getClaim(t, "facade-toggle")
		return !ok
	}
	isSuspended := func() bool {
		got := getSandbox(t, "facade-toggle")
		susp := apimeta.FindStatusCondition(got.Status.Conditions, string(agentsv1beta1.SandboxConditionSuspended))
		return susp != nil && susp.Status == metav1.ConditionTrue &&
			got.Status.ServiceFQDN == "" && len(got.Status.PodIPs) == 0
	}

	// Running (create) -> claim present + Ready.
	eventually(t, "initial activation creates the claim", claimPresent)
	driveClaimReady(t, "facade-toggle")

	// -> Suspended (pause): released + observables cleared.
	setMode(agentsv1beta1.SandboxOperatingModeSuspended)
	eventually(t, "first pause releases the claim", claimAbsent)
	eventually(t, "first pause clears the observables", isSuspended)

	// -> Running (resume): re-activated.
	setMode(agentsv1beta1.SandboxOperatingModeRunning)
	eventually(t, "resume re-activates the claim", claimPresent)
	driveClaimReady(t, "facade-toggle")

	// -> Suspended (pause again): released again, stable.
	setMode(agentsv1beta1.SandboxOperatingModeSuspended)
	eventually(t, "second pause releases the claim", claimAbsent)
	eventually(t, "second pause clears the observables", isSuspended)

	// Idempotent: re-applying Suspended (no spec change but a forced reconcile
	// via an annotation bump) keeps the released state; the claim stays absent.
	var nudge agentsv1beta1.Sandbox
	updateWithRetry(t, types.NamespacedName{Name: "facade-toggle", Namespace: "default"}, &nudge, func() {
		if nudge.Annotations == nil {
			nudge.Annotations = map[string]string{}
		}
		nudge.Annotations["test.mitos.run/nudge"] = "1"
	})
	// Give the reconcile a moment, then assert the claim is still absent and
	// the Suspended condition is True.
	eventually(t, "idempotent re-reconcile keeps the claim released", claimAbsent)
	got := getSandbox(t, "facade-toggle")
	susp := apimeta.FindStatusCondition(got.Status.Conditions, string(agentsv1beta1.SandboxConditionSuspended))
	if susp == nil || susp.Status != metav1.ConditionTrue {
		t.Fatalf("idempotent pause: Suspended condition absent or not True, want True")
	}
}

// TestFacadeDeleteTerminatesClaim: deleting a Sandbox garbage-collects our
// SandboxClaim via the owner reference.
func TestFacadeDeleteTerminatesClaim(t *testing.T) {
	sb := newSandbox("facade-delete", map[string]string{facade.PoolAnnotation: "p"}, "")
	if err := k8sClient.Create(testCtx, sb); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	eventually(t, "facade creates the SandboxClaim", func() bool {
		_, ok := getClaim(t, "facade-delete")
		return ok
	})

	if err := k8sClient.Delete(testCtx, sb); err != nil {
		t.Fatalf("delete sandbox: %v", err)
	}

	// envtest has no real garbage collector controller, so the owner-reference
	// cascade is not exercised by the apiserver. Assert the linkage instead: the
	// claim carries a controller owner reference to the deleted Sandbox, which is
	// what a live apiserver GC acts on. Also delete it explicitly to clean up.
	claim, ok := getClaim(t, "facade-delete")
	if !ok {
		return
	}
	if !hasControllerOwner(claim, "facade-delete") {
		t.Fatalf("claim missing controller owner reference to the Sandbox: %+v", claim.OwnerReferences)
	}
	_ = k8sClient.Delete(testCtx, claim, &client.DeleteOptions{})
}

func hasControllerOwner(claim *runv1.Sandbox, sandboxName string) bool {
	for _, o := range claim.OwnerReferences {
		if o.Kind == "Sandbox" && o.Name == sandboxName && o.Controller != nil && *o.Controller {
			return true
		}
	}
	return false
}
