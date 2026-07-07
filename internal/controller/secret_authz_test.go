package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/tenant"
)

func secretAuthzScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func labeledSecret(name, ns, org string) *corev1.Secret {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data:       map[string][]byte{"token": []byte("super-secret")},
	}
	if org != "" {
		s.Labels = tenant.OrgLabels(org)
	}
	return s
}

// TestResolveSecretsRefusesCrossOrgLabeledSecret proves the controller-side
// defense in depth: a claim owned by org A cannot resolve a Secret carrying a
// different org's label, even in a shared namespace (GHSA-pgv2-9w24-j7wh).
func TestResolveSecretsRefusesCrossOrgLabeledSecret(t *testing.T) {
	c := fakeclient.NewClientBuilder().WithScheme(secretAuthzScheme(t)).
		WithObjects(labeledSecret("victim", "mitos", "org-b")).Build()
	r := &SandboxReconciler{Client: c}
	mounts := []v1.SecretMount{{Name: "x", SecretRef: corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "victim"}, Key: "token"}, EnvVar: "STOLEN"}}

	_, vals, err := r.resolveSecrets(context.Background(), "mitos", "org-a", nil, mounts)
	if err == nil {
		t.Fatalf("resolveSecrets allowed a cross-org secret; vals=%v", vals)
	}
	if _, ok := vals["STOLEN"]; ok {
		t.Fatalf("cross-org secret value was returned")
	}
}

// TestResolveSecretsAllowsSameOrgLabeledSecret proves the check does not break
// the legitimate same-org path.
func TestResolveSecretsAllowsSameOrgLabeledSecret(t *testing.T) {
	c := fakeclient.NewClientBuilder().WithScheme(secretAuthzScheme(t)).
		WithObjects(labeledSecret("mine", "mitos", "org-a")).Build()
	r := &SandboxReconciler{Client: c}
	mounts := []v1.SecretMount{{Name: "x", SecretRef: corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "mine"}, Key: "token"}, EnvVar: "OK"}}

	_, vals, err := r.resolveSecrets(context.Background(), "mitos", "org-a", nil, mounts)
	if err != nil {
		t.Fatalf("resolveSecrets refused a same-org secret: %v", err)
	}
	if vals["OK"] != "super-secret" {
		t.Fatalf("same-org secret not resolved: %v", vals)
	}
}

// TestResolveWorkspaceHeadRefusesCrossOrg proves a claim owned by org A cannot
// hydrate a Workspace carrying a different org's label (GHSA-pgv2-9w24-j7wh).
func TestResolveWorkspaceHeadRefusesCrossOrg(t *testing.T) {
	ws := &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "victim-ws", Namespace: "mitos", Labels: tenant.OrgLabels("org-b")},
		Status:     v1.WorkspaceStatus{Head: "rev-1"},
	}
	// Seed the head revision so that WITHOUT the org check resolveWorkspaceHead
	// returns cleanly (nil error); this keeps the RED pinned on the org check,
	// not on a missing revision.
	rev := &v1.WorkspaceRevision{ObjectMeta: metav1.ObjectMeta{Name: "rev-1", Namespace: "mitos"}}
	c := fakeclient.NewClientBuilder().WithScheme(secretAuthzScheme(t)).WithObjects(ws, rev).Build()
	r := &SandboxReconciler{Client: c}
	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "mitos", Labels: tenant.OrgLabels("org-a")},
		Spec:       v1.SandboxSpec{WorkspaceRef: &v1.LocalObjectReference{Name: "victim-ws"}},
	}
	if _, _, _, err := r.resolveWorkspaceHead(context.Background(), claim); err == nil {
		t.Fatalf("resolveWorkspaceHead allowed a cross-org workspace")
	}
}

// TestResolveSecretsAllowsWhenClaimHasNoOrg proves self-hosted / non-SaaS use
// (claims carry no org label) is unaffected: an empty wantOrg skips the check.
func TestResolveSecretsAllowsWhenClaimHasNoOrg(t *testing.T) {
	c := fakeclient.NewClientBuilder().WithScheme(secretAuthzScheme(t)).
		WithObjects(labeledSecret("any", "default", "")).Build()
	r := &SandboxReconciler{Client: c}
	mounts := []v1.SecretMount{{Name: "x", SecretRef: corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "any"}, Key: "token"}, EnvVar: "OK"}}

	_, vals, err := r.resolveSecrets(context.Background(), "default", "", nil, mounts)
	if err != nil {
		t.Fatalf("resolveSecrets refused with no org context: %v", err)
	}
	if vals["OK"] != "super-secret" {
		t.Fatalf("unlabeled secret not resolved in self-hosted mode: %v", vals)
	}
}
