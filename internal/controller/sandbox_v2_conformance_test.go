package controller_test

// Conformance + guard envtests for the v1 consolidated Sandbox kind (issue #23,
// ADR 0007): the three-noun API expresses everything the four-noun API did, and
// the reconciler OWNS the engine directly (no intermediate SandboxClaim or
// SandboxFork object).
//
//   - A Sandbox{source.poolRef} reaches Ready, driving the fork-from-pool path
//     the old SandboxClaim drove (the CLAIM equivalent), with NO child object.
//   - A Sandbox{source.fromSandbox, replicas:N} produces N children, driving the
//     live-fork path the old SandboxFork drove (the FORK equivalent).
//   - A pool with an inline spec.template builds with NO SandboxTemplate object.
//   - A fork of a secret-holding source is denied by default (reissue), with the
//     SecretInheritanceDenied condition.
//   - A Sandbox{source.fromRevision} reports Ready=False / RevisionResumeNotImplemented.
//
// All run against the MOCK engine (the fake forkd node).

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/controller"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// waitSandboxReady polls until the named v1 Sandbox reaches Ready and returns it.
func waitSandboxReady(t *testing.T, name string) *v1.Sandbox {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last v1.Sandbox
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, &last); err == nil {
			if last.Status.Phase == v1.SandboxReady {
				return &last
			}
			if last.Status.Phase == v1.SandboxFailed {
				t.Fatalf("sandbox %s failed: %+v", name, last.Status)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("sandbox %s did not become Ready within 20s; last status: %+v", name, last.Status)
	return nil
}

// inlinePool builds a v1 SandboxPool with an inline template (no SandboxTemplate
// object). The pool name doubles as the template/snapshot id (poolTemplateID).
func inlinePool(name string) *v1.SandboxPool {
	return &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}
}

// TestSandboxV2PoolRefReachesReady is the CLAIM-equivalent conformance: a Sandbox
// with source.poolRef and the default replicas 1 reaches Ready, with an endpoint
// and sandboxID set by the engine, and NO child SandboxClaim object (the kind no
// longer exists).
func TestSandboxV2PoolRefReachesReady(t *testing.T) {
	stop, err := controller.StartFakeForkdNode(testRegistry, "v2pool-node-1", "v2pool-pool")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	pool := inlinePool("v2pool-pool")
	sb := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "v2pool-sandbox", Namespace: "default"},
		Spec: v1.SandboxSpec{
			Source:   v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: "v2pool-pool"}},
			Replicas: 1,
		},
	}
	for _, obj := range []client.Object{pool, sb} {
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, sb)
		_ = k8sClient.Delete(ctx, pool)
	})

	ready := waitSandboxReady(t, "v2pool-sandbox")
	if ready.Status.Endpoint == "" {
		t.Fatal("Ready sandbox has no endpoint set by the engine")
	}
	if ready.Status.SandboxID == "" {
		t.Fatal("Ready sandbox has no sandboxID set by the engine")
	}
}

// TestSandboxPoolRefCreatesNoChildClaim is the guard: a Sandbox{source.poolRef}
// drives the engine DIRECTLY and populates status.SandboxID + Endpoint, proving
// the engine path runs with no intermediate child object.
func TestSandboxPoolRefCreatesNoChildClaim(t *testing.T) {
	stop, err := controller.StartFakeForkdNode(testRegistry, "v2nochild-node-1", "v2nochild-pool")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	pool := inlinePool("v2nochild-pool")
	sb := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "v2nochild-sandbox", Namespace: "default"},
		Spec: v1.SandboxSpec{
			Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: "v2nochild-pool"}},
		},
	}
	for _, obj := range []client.Object{pool, sb} {
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, sb)
		_ = k8sClient.Delete(ctx, pool)
	})

	ready := waitSandboxReady(t, "v2nochild-sandbox")
	if ready.Status.SandboxID == "" || ready.Status.Endpoint == "" {
		t.Fatalf("engine did not populate status directly: %+v", ready.Status)
	}
}

// TestPoolInlineTemplateBuildsWithoutTemplateObject is the guard for the pool
// inline template: a pool with spec.template (and no SandboxTemplate object)
// drives a poolRef Sandbox to Ready, proving the engine reads the inline template.
func TestPoolInlineTemplateBuildsWithoutTemplateObject(t *testing.T) {
	stop, err := controller.StartFakeForkdNode(testRegistry, "v2inline-node-1", "v2inline-pool")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	pool := inlinePool("v2inline-pool")
	sb := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "v2inline-sandbox", Namespace: "default"},
		Spec: v1.SandboxSpec{
			Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: "v2inline-pool"}},
		},
	}
	for _, obj := range []client.Object{pool, sb} {
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, sb)
		_ = k8sClient.Delete(ctx, pool)
	})

	waitSandboxReady(t, "v2inline-sandbox")
}

// TestSandboxV2FromSandboxProducesNChildren is the FORK-equivalent conformance: a
// Sandbox with source.fromSandbox and replicas N produces N ready children, each
// mirrored into status.children, and reports readyReplicas == N.
func TestSandboxV2FromSandboxProducesNChildren(t *testing.T) {
	stop, err := controller.StartFakeForkdNode(testRegistry, "v2fork-node-1", "v2fork-pool")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	pool := inlinePool("v2fork-pool")
	source := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "v2fork-source", Namespace: "default"},
		Spec: v1.SandboxSpec{
			Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: "v2fork-pool"}},
		},
	}
	for _, obj := range []client.Object{pool, source} {
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, source)
		_ = k8sClient.Delete(ctx, pool)
	})

	waitSandboxReady(t, "v2fork-source")

	const replicas = 3
	fanout := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "v2fork-fanout", Namespace: "default"},
		Spec: v1.SandboxSpec{
			Source:   v1.SandboxSource{FromSandbox: &v1.FromSandboxSource{Name: "v2fork-source"}},
			Replicas: replicas,
		},
	}
	if err := k8sClient.Create(ctx, fanout); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, fanout) })

	ready := waitSandboxReady(t, "v2fork-fanout")
	if ready.Status.ReadyReplicas != replicas {
		t.Fatalf("readyReplicas = %d, want %d", ready.Status.ReadyReplicas, replicas)
	}
	if len(ready.Status.Children) != replicas {
		t.Fatalf("children = %d, want %d: %+v", len(ready.Status.Children), replicas, ready.Status.Children)
	}
	for i, c := range ready.Status.Children {
		if c.Name == "" || c.Endpoint == "" || c.SandboxID == "" {
			t.Fatalf("child %d incomplete: %+v", i, c)
		}
		if c.Phase != v1.SandboxReady {
			t.Fatalf("child %d phase = %q, want Ready", i, c.Phase)
		}
	}
}

// TestForkDeniesSecretInheritanceByDefault is the security guard: a fork
// (source.fromSandbox) of a source that holds secrets is DENIED by default
// (secretInheritance defaults to reissue), reported with the SecretInheritanceDenied
// condition. This preserves docs/fork-correctness.md section 3. The fresh per-fork
// bearer-token reissue is asserted directly in fork_secrets_test.go and
// token_secret_envtest_test.go.
func TestForkDeniesSecretInheritanceByDefault(t *testing.T) {
	stop, err := controller.StartFakeForkdNode(testRegistry, "v2deny-node-1", "v2deny-pool")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "v2deny-secret", Namespace: "default"},
		Data:       map[string][]byte{"k": []byte("v")},
	}
	pool := inlinePool("v2deny-pool")
	source := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "v2deny-source", Namespace: "default"},
		Spec: v1.SandboxSpec{
			Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: "v2deny-pool"}},
			Secrets: []v1.SecretMount{{
				Name:      "k",
				SecretRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "v2deny-secret"}, Key: "k"},
			}},
		},
	}
	for _, obj := range []client.Object{secret, pool, source} {
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, source)
		_ = k8sClient.Delete(ctx, pool)
		_ = k8sClient.Delete(ctx, secret)
	})

	waitSandboxReady(t, "v2deny-source")

	// A fork with NO secretInheritance set (defaults to reissue) must be denied.
	fork := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "v2deny-fork", Namespace: "default"},
		Spec: v1.SandboxSpec{
			Source:   v1.SandboxSource{FromSandbox: &v1.FromSandboxSource{Name: "v2deny-source"}},
			Replicas: 1,
		},
	}
	if err := k8sClient.Create(ctx, fork); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, fork) })

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var got v1.Sandbox
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "v2deny-fork", Namespace: "default"}, &got); err == nil {
			if c := apimeta.FindStatusCondition(got.Status.Conditions, "Rejected"); c != nil &&
				c.Status == metav1.ConditionTrue && c.Reason == "SecretInheritanceDenied" {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("fork was not denied with SecretInheritanceDenied by default (reissue)")
}

// TestFromRevisionReportsNotServedCondition is the guard for the not-served
// source: a Sandbox{source.fromRevision} reports phase Pending and a Ready=False
// condition with reason RevisionResumeNotImplemented, never silently dropped.
func TestFromRevisionReportsNotServedCondition(t *testing.T) {
	sb := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "v2rev-sandbox", Namespace: "default"},
		Spec: v1.SandboxSpec{
			Source: v1.SandboxSource{FromRevision: &v1.FromRevisionSource{Workspace: "w", Revision: "rev-1"}},
		},
	}
	if err := k8sClient.Create(ctx, sb); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, sb) })

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var got v1.Sandbox
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "v2rev-sandbox", Namespace: "default"}, &got); err == nil {
			c := apimeta.FindStatusCondition(got.Status.Conditions, "Ready")
			if c != nil && c.Status == metav1.ConditionFalse && c.Reason == "RevisionResumeNotImplemented" {
				if got.Status.Phase != v1.SandboxPending {
					t.Fatalf("phase = %q, want Pending", got.Status.Phase)
				}
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("fromRevision sandbox did not report Ready=False reason RevisionResumeNotImplemented")
}
