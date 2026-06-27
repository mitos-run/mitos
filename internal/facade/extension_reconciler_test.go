package facade_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	agentsv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"

	runv1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/facade"
)

// newExtTemplate builds a minimal valid upstream extension SandboxTemplate: the
// podTemplate is required, so it carries a single container with the mapped
// fields (image, command, env).
func newExtTemplate(name string) *extv1beta1.SandboxTemplate {
	return &extv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: extv1beta1.SandboxTemplateSpec{
			PodTemplate: agentsv1beta1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:    "agent",
							Image:   "python:3.11-slim",
							Command: []string{"/bin/sh", "-c", "sleep 1"},
							Env:     []corev1.EnvVar{{Name: "K", Value: "v"}},
						},
					},
				},
			},
		},
	}
}

// getOurTemplate fetches the SandboxPool the template reconciler bridges from an
// upstream SandboxTemplate (ADR 0007: the template inlines into a pool's
// spec.template under the same name).
func getOurTemplate(t *testing.T, name string) (*runv1.SandboxPool, bool) {
	t.Helper()
	var tmpl runv1.SandboxPool
	err := k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: "default"}, &tmpl)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("get our template pool %s: %v", name, err)
	}
	return &tmpl, true
}

func getOurPool(t *testing.T, name string) (*runv1.SandboxPool, bool) {
	t.Helper()
	var pool runv1.SandboxPool
	err := k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: "default"}, &pool)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("get our pool %s: %v", name, err)
	}
	return &pool, true
}

// TestFacadeMapsExtSandboxTemplate: an upstream extension SandboxTemplate
// reconciles to our consolidated mitos.run/v1 SandboxPool with an inline
// spec.template (ADR 0007), mapping the first container's image/command/env,
// stamping the bridge annotation, and owner-referenced for GC.
func TestFacadeMapsExtSandboxTemplate(t *testing.T) {
	src := newExtTemplate("ext-template")
	if err := k8sClient.Create(testCtx, src); err != nil {
		t.Fatalf("create ext template: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, src) })

	var tmpl *runv1.SandboxPool
	eventually(t, "facade maps the ext SandboxTemplate to our pool inline template", func() bool {
		tt, ok := getOurTemplate(t, "ext-template")
		tmpl = tt
		return ok && tt.Spec.Template != nil
	})

	if tmpl.Spec.Template == nil {
		t.Fatalf("our pool has no inline template, want one bridged from the upstream SandboxTemplate")
	}
	if tmpl.Spec.Template.Image != "python:3.11-slim" {
		t.Fatalf("our template image = %q, want python:3.11-slim", tmpl.Spec.Template.Image)
	}
	if len(tmpl.Spec.Template.Command) != 3 || tmpl.Spec.Template.Command[0] != "/bin/sh" {
		t.Fatalf("our template command = %+v, want the upstream container command", tmpl.Spec.Template.Command)
	}
	if len(tmpl.Spec.Template.Env) != 1 || tmpl.Spec.Template.Env[0].Name != "K" || tmpl.Spec.Template.Env[0].Value != "v" {
		t.Fatalf("our template env = %+v, want K=v", tmpl.Spec.Template.Env)
	}
	if tmpl.Annotations[facade.TemplateAnnotation] != "ext-template" {
		t.Fatalf("our template bridge annotation = %q, want ext-template", tmpl.Annotations[facade.TemplateAnnotation])
	}
	if len(tmpl.OwnerReferences) != 1 || tmpl.OwnerReferences[0].Kind != "SandboxTemplate" || tmpl.OwnerReferences[0].Name != "ext-template" {
		t.Fatalf("our template owner refs = %+v, want a single SandboxTemplate owner", tmpl.OwnerReferences)
	}
}

// newExtWarmPool builds an upstream extension SandboxWarmPool referencing a
// template by name at the requested replicas.
func newExtWarmPool(name, templateName string, replicas int32) *extv1beta1.SandboxWarmPool {
	return &extv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: extv1beta1.SandboxWarmPoolSpec{
			Replicas:    replicas,
			TemplateRef: extv1beta1.SandboxTemplateRef{Name: templateName},
		},
	}
}

// TestFacadeMapsExtSandboxWarmPool: an upstream extension SandboxWarmPool
// reconciles to our mitos.run SandboxPool at the requested replicas, pointing
// at our template, owner-referenced and bridge-annotated.
func TestFacadeMapsExtSandboxWarmPool(t *testing.T) {
	src := newExtWarmPool("ext-warmpool", "ext-wp-template", 3)
	if err := k8sClient.Create(testCtx, src); err != nil {
		t.Fatalf("create ext warm pool: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, src) })

	var pool *runv1.SandboxPool
	eventually(t, "facade maps the ext SandboxWarmPool to our pool", func() bool {
		p, ok := getOurPool(t, "ext-warmpool")
		pool = p
		return ok
	})

	// The warm-slot count re-homes onto spec.warm.min (ADR 0007 folded the
	// v1beta1 spec.replicas onto warm.min).
	if pool.Spec.Warm == nil || pool.Spec.Warm.Min != 3 {
		t.Fatalf("our pool warm.min = %+v, want 3", pool.Spec.Warm)
	}
	if pool.Spec.TemplateRef == nil || pool.Spec.TemplateRef.Name != "ext-wp-template" {
		t.Fatalf("our pool templateRef = %+v, want ext-wp-template", pool.Spec.TemplateRef)
	}
	if pool.Annotations[facade.WarmPoolAnnotation] != "ext-warmpool" {
		t.Fatalf("our pool warmpool bridge annotation = %q, want ext-warmpool", pool.Annotations[facade.WarmPoolAnnotation])
	}
	if len(pool.OwnerReferences) != 1 || pool.OwnerReferences[0].Kind != "SandboxWarmPool" || pool.OwnerReferences[0].Name != "ext-warmpool" {
		t.Fatalf("our pool owner refs = %+v, want a single SandboxWarmPool owner", pool.OwnerReferences)
	}

	// The mirrored scale-subresource selector must match the husk pods of the
	// mapped pool (mitos.run/pool=<pool>,mitos.run/husk=true, the exact
	// keys/values buildHuskPod stamps), so an HPA reading pod-resource metrics
	// finds the real husk pods. The pool and warm pool share the same name.
	wantSelector := "mitos.run/pool=ext-warmpool,mitos.run/husk=true"
	eventually(t, "the facade mirrors a husk-pod-matching status.selector", func() bool {
		var cur extv1beta1.SandboxWarmPool
		if err := k8sClient.Get(testCtx, types.NamespacedName{Name: "ext-warmpool", Namespace: "default"}, &cur); err != nil {
			return false
		}
		return cur.Status.Selector == wantSelector
	})
}

// TestFacadeWarmPoolReplicasFollowUpstream: changing the upstream warm pool's
// spec.replicas (as an HPA would) updates our pool's replicas; the facade
// re-reads the replica count every reconcile.
func TestFacadeWarmPoolReplicasFollowUpstream(t *testing.T) {
	src := newExtWarmPool("ext-warmpool-hpa", "ext-hpa-template", 1)
	if err := k8sClient.Create(testCtx, src); err != nil {
		t.Fatalf("create ext warm pool: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, src) })

	eventually(t, "our pool starts at 1 warm slot", func() bool {
		p, ok := getOurPool(t, "ext-warmpool-hpa")
		return ok && p.Spec.Warm != nil && p.Spec.Warm.Min == 1
	})

	// Simulate an HPA scaling their warm pool to 5.
	var cur extv1beta1.SandboxWarmPool
	updateWithRetry(t, types.NamespacedName{Name: "ext-warmpool-hpa", Namespace: "default"}, &cur, func() {
		cur.Spec.Replicas = 5
	})

	eventually(t, "our pool follows the upstream replica change to 5", func() bool {
		p, ok := getOurPool(t, "ext-warmpool-hpa")
		return ok && p.Spec.Warm != nil && p.Spec.Warm.Min == 5
	})
}

func getOurClaimT(t *testing.T, name string) (*runv1.Sandbox, bool) {
	t.Helper()
	return getClaim(t, name)
}

// newExtClaim builds an upstream extension SandboxClaim referencing a warm pool
// by name directly (v1beta1: no policy, direct WarmPoolRef).
func newExtClaim(name, poolName string) *extv1beta1.SandboxClaim {
	return &extv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: extv1beta1.SandboxClaimSpec{
			WarmPoolRef: extv1beta1.SandboxWarmPoolRef{Name: poolName},
		},
	}
}

// TestFacadeClaimWarmPoolRefBindsPool: an upstream SandboxClaim with
// spec.warmPoolRef.name binds our run-path Sandbox to that exact pool and
// stamps the mitos.run/pool bridge annotation.
func TestFacadeClaimWarmPoolRefBindsPool(t *testing.T) {
	src := newExtClaim("ext-claim-warmref", "claim-target-pool")
	if err := k8sClient.Create(testCtx, src); err != nil {
		t.Fatalf("create ext claim: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, src) })

	var claim *runv1.Sandbox
	eventually(t, "facade creates our run-path Sandbox bound to the warm pool ref", func() bool {
		c, ok := getOurClaimT(t, "ext-claim-warmref")
		claim = c
		return ok && c.Spec.Source.PoolRef != nil && c.Spec.Source.PoolRef.Name == "claim-target-pool"
	})
	if claim.Annotations[facade.PoolAnnotation] != "claim-target-pool" {
		t.Fatalf("claim mitos.run/pool annotation = %q, want claim-target-pool", claim.Annotations[facade.PoolAnnotation])
	}
	if !hasControllerOwner2(claim, "ext-claim-warmref") {
		t.Fatalf("claim missing controller owner reference to the upstream SandboxClaim: %+v", claim.OwnerReferences)
	}
}

// TestFacadeClaimEnvMapped: a SandboxClaim with spec.env maps the env vars onto
// our run-path Sandbox's spec.env (the husk run path applies them into the guest).
func TestFacadeClaimEnvMapped(t *testing.T) {
	src := &extv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "ext-claim-env", Namespace: "default"},
		Spec: extv1beta1.SandboxClaimSpec{
			WarmPoolRef: extv1beta1.SandboxWarmPoolRef{Name: "env-test-pool"},
			Env: []extv1beta1.EnvVar{
				{Name: "MYKEY", Value: "myvalue"},
				{Name: "OTHER", Value: "thing"},
			},
		},
	}
	if err := k8sClient.Create(testCtx, src); err != nil {
		t.Fatalf("create ext claim with env: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, src) })

	eventually(t, "facade maps claim env onto our run-path Sandbox", func() bool {
		c, ok := getOurClaimT(t, "ext-claim-env")
		if !ok {
			return false
		}
		return len(c.Spec.Env) == 2 &&
			c.Spec.Env[0].Name == "MYKEY" && c.Spec.Env[0].Value == "myvalue" &&
			c.Spec.Env[1].Name == "OTHER" && c.Spec.Env[1].Value == "thing"
	})
}

// TestFacadeClaimMirrorsStatusAndGCs: when our claim reaches Ready, the facade
// mirrors a Ready=True condition + the bound sandbox name into the upstream
// claim status; deleting the upstream claim leaves our claim owner-referenced
// for GC.
func TestFacadeClaimMirrorsStatusAndGCs(t *testing.T) {
	src := newExtClaim("ext-claim-status", "claim-status-pool")
	if err := k8sClient.Create(testCtx, src); err != nil {
		t.Fatalf("create ext claim: %v", err)
	}

	var claim *runv1.Sandbox
	eventually(t, "facade creates our claim", func() bool {
		c, ok := getOurClaimT(t, "ext-claim-status")
		claim = c
		return ok
	})

	// Drive our claim Ready (the test seam the real husk activation path sets).
	statusUpdateWithRetry(t, types.NamespacedName{Name: "ext-claim-status", Namespace: "default"}, claim, func() {
		claim.Status.Phase = runv1.SandboxReady
		claim.Status.Endpoint = "10.0.0.9:9091"
	})

	eventually(t, "upstream claim status mirrors Bound/Ready + sandbox name", func() bool {
		var got extv1beta1.SandboxClaim
		if err := k8sClient.Get(testCtx, types.NamespacedName{Name: "ext-claim-status", Namespace: "default"}, &got); err != nil {
			return false
		}
		cond := apimetaFind(got.Status.Conditions, "Ready")
		return cond != nil && cond.Status == metav1.ConditionTrue && got.Status.SandboxStatus.Name == "ext-claim-status"
	})

	// Deletion: our claim stays owner-referenced for GC (envtest has no GC, assert
	// the linkage like the core delete test does).
	if err := k8sClient.Delete(testCtx, src); err != nil {
		t.Fatalf("delete upstream claim: %v", err)
	}
	got, ok := getOurClaimT(t, "ext-claim-status")
	if ok && !hasControllerOwner2(got, "ext-claim-status") {
		t.Fatalf("our claim missing controller owner reference to the upstream claim: %+v", got.OwnerReferences)
	}
	if ok {
		_ = k8sClient.Delete(testCtx, got)
	}
}

// hasControllerOwner2 reports whether our claim carries a controller owner
// reference to the upstream SandboxClaim of the given name.
func hasControllerOwner2(claim *runv1.Sandbox, name string) bool {
	for _, o := range claim.OwnerReferences {
		if o.Kind == "SandboxClaim" && o.Name == name && o.Controller != nil && *o.Controller {
			return true
		}
	}
	return false
}

// apimetaFind finds a status condition by type.
func apimetaFind(conds []metav1.Condition, condType string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}
