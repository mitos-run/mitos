package kubesecrets

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"mitos.run/mitos/internal/saas/console"
)

func newProvider(t *testing.T, objs ...client.Object) (*Provider, client.Client) {
	t.Helper()
	c := fakeclient.NewClientBuilder().WithObjects(objs...).Build()
	// namespace = "org-<id>" so cross-org isolation is by namespace.
	return New(c, func(orgID string) string { return "org-" + orgID }), c
}

// TestPutCreatesNamespacedSecretWithValue asserts Put writes a k8s Secret in the
// org's namespace that holds the value (so the controller can resolve it for
// injection) and returns a SecretView that does NOT carry the value.
func TestPutCreatesNamespacedSecretWithValue(t *testing.T) {
	p, c := newProvider(t)
	view, err := p.Put(context.Background(), "alice", "OPENAI_API_KEY", "sk-secret")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if view.Provider != "kube" || view.Mode != "copy_in" || view.Version != 1 {
		t.Errorf("view = %+v, want kube/copy_in/v1", view)
	}
	if view.Fingerprint == "" {
		t.Error("view.Fingerprint empty")
	}

	// The underlying Secret holds the value in the org's namespace.
	var list corev1.SecretList
	if err := c.List(context.Background(), &list, client.InNamespace("org-alice")); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("got %d secrets, want 1", len(list.Items))
	}
	if got := string(list.Items[0].Data["value"]); got != "sk-secret" {
		t.Errorf("stored value = %q, want sk-secret", got)
	}
	if list.Items[0].Labels["mitos.run/org"] != "alice" {
		t.Errorf("missing org label: %v", list.Items[0].Labels)
	}
}

// TestPutRotateBumpsVersion asserts a second Put of the same name updates the
// secret in place and bumps the version.
func TestPutRotateBumpsVersion(t *testing.T) {
	p, _ := newProvider(t)
	if _, err := p.Put(context.Background(), "alice", "TOKEN", "v1"); err != nil {
		t.Fatalf("put1: %v", err)
	}
	v2, err := p.Put(context.Background(), "alice", "TOKEN", "v2")
	if err != nil {
		t.Fatalf("put2: %v", err)
	}
	if v2.Version != 2 {
		t.Errorf("version = %d, want 2", v2.Version)
	}
	list, err := p.List(context.Background(), "alice")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("rotate created a duplicate: %d secrets", len(list))
	}
}

// TestListScopedToOrgNamespace asserts List returns only the org's secrets;
// another org's namespace is never read.
func TestListScopedToOrgNamespace(t *testing.T) {
	p, _ := newProvider(t)
	_, _ = p.Put(context.Background(), "alice", "A", "x")
	_, _ = p.Put(context.Background(), "bob", "B", "y")

	alice, err := p.List(context.Background(), "alice")
	if err != nil {
		t.Fatalf("list alice: %v", err)
	}
	if len(alice) != 1 || alice[0].Name != "A" {
		t.Fatalf("alice list = %+v, want exactly A", alice)
	}
	bob, err := p.List(context.Background(), "bob")
	if err != nil {
		t.Fatalf("list bob: %v", err)
	}
	if len(bob) != 1 || bob[0].Name != "B" {
		t.Fatalf("bob list = %+v, want exactly B", bob)
	}
}

// TestDeleteRemovesSecretAndMissingIsNotFound asserts delete removes the secret
// and a missing name maps to console.ErrNotFound.
func TestDeleteRemovesSecretAndMissingIsNotFound(t *testing.T) {
	p, _ := newProvider(t)
	_, _ = p.Put(context.Background(), "alice", "K", "x")
	if err := p.Delete(context.Background(), "alice", "K"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	list, _ := p.List(context.Background(), "alice")
	if len(list) != 0 {
		t.Fatalf("secret survived delete: %+v", list)
	}
	if err := p.Delete(context.Background(), "alice", "missing"); err != console.ErrNotFound {
		t.Fatalf("delete missing err = %v, want console.ErrNotFound", err)
	}
}

// TestImplementsSecretStore is a compile-time assertion that the provider
// satisfies the console.SecretStore seam.
func TestImplementsSecretStore(t *testing.T) {
	var _ console.SecretStore = (*Provider)(nil)
	_ = metav1.ObjectMeta{}
}
