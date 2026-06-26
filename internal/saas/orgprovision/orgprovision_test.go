package orgprovision

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	v1 "mitos.run/mitos/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return s
}

// TestProvisionCreatesOrgCR asserts a fresh provision creates the cluster-scoped
// Org with name == org id and the given display name.
func TestProvisionCreatesOrgCR(t *testing.T) {
	c := fakeclient.NewClientBuilder().WithScheme(newScheme(t)).Build()
	p := New(c)

	if err := p.Provision(context.Background(), "org-abc", "Personal"); err != nil {
		t.Fatalf("provision: %v", err)
	}

	var got v1.Org
	if err := c.Get(context.Background(), client.ObjectKey{Name: "org-abc"}, &got); err != nil {
		t.Fatalf("org not created: %v", err)
	}
	if got.Name != "org-abc" {
		t.Fatalf("org name %q, want org-abc", got.Name)
	}
	if got.Spec.DisplayName != "Personal" {
		t.Fatalf("display name %q, want Personal", got.Spec.DisplayName)
	}
}

// TestProvisionIsIdempotentOnAlreadyExists asserts a second provision for the
// same org id (AlreadyExists) succeeds rather than erroring.
func TestProvisionIsIdempotentOnAlreadyExists(t *testing.T) {
	c := fakeclient.NewClientBuilder().WithScheme(newScheme(t)).Build()
	p := New(c)
	ctx := context.Background()

	if err := p.Provision(ctx, "org-dup", "Personal"); err != nil {
		t.Fatalf("first provision: %v", err)
	}
	if err := p.Provision(ctx, "org-dup", "Personal"); err != nil {
		t.Fatalf("second provision should be idempotent, got: %v", err)
	}

	var list v1.OrgList
	if err := c.List(ctx, &list); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("got %d orgs, want exactly 1 (idempotent)", len(list.Items))
	}
}

// TestProvisionReconcilesDisplayName asserts a provision with a changed display
// name updates the existing Org rather than failing.
func TestProvisionReconcilesDisplayName(t *testing.T) {
	c := fakeclient.NewClientBuilder().WithScheme(newScheme(t)).Build()
	p := New(c)
	ctx := context.Background()

	if err := p.Provision(ctx, "org-rename", "Old Name"); err != nil {
		t.Fatalf("first provision: %v", err)
	}
	if err := p.Provision(ctx, "org-rename", "New Name"); err != nil {
		t.Fatalf("rename provision: %v", err)
	}
	var got v1.Org
	if err := c.Get(ctx, client.ObjectKey{Name: "org-rename"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.DisplayName != "New Name" {
		t.Fatalf("display name %q, want New Name", got.Spec.DisplayName)
	}
}

func TestProvisionRejectsEmptyOrgID(t *testing.T) {
	c := fakeclient.NewClientBuilder().WithScheme(newScheme(t)).Build()
	p := New(c)
	if err := p.Provision(context.Background(), "", "x"); err == nil {
		t.Fatal("expected error for empty org id")
	}
}
