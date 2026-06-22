package clustersandbox

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "mitos.run/mitos/api/v1alpha1"
	sandboxv2 "mitos.run/mitos/api/v1alpha2"
	"mitos.run/mitos/internal/saas/console"
	"mitos.run/mitos/internal/tenant"
)

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := sandboxv2.AddToScheme(s); err != nil {
		t.Fatalf("add v2 scheme: %v", err)
	}
	return s
}

// sb builds a v1alpha2.Sandbox owned by org, in that org's hard-isolation
// namespace and carrying the org label.
func sb(org, name, phase string) *sandboxv2.Sandbox {
	return &sandboxv2.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: tenant.NamespaceForOrg(org),
			Labels:    tenant.OrgLabels(org),
		},
		Spec:   sandboxv2.SandboxSpec{Source: sandboxv2.SandboxSource{PoolRef: &v1alpha1.LocalObjectReference{Name: "python"}}},
		Status: sandboxv2.SandboxStatus{Phase: v1alpha1.SandboxPhase(phase), SandboxID: "engine-" + name},
	}
}

func newControl(t *testing.T, objs ...client.Object) *Control {
	t.Helper()
	c := fakeclient.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objs...).Build()
	return New(c)
}

// TestListScopedToOrgNamespace asserts List returns only the caller org's
// sandboxes — bob's, in bob's namespace, are never seen by alice.
func TestListScopedToOrgNamespace(t *testing.T) {
	c := newControl(t, sb("alice", "sb-a1", "Ready"), sb("alice", "sb-a2", "Pending"), sb("bob", "sb-b1", "Ready"))
	got, err := c.List(context.Background(), "alice")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("alice saw %d sandboxes, want 2", len(got))
	}
	for _, v := range got {
		if v.OrgID != "alice" {
			t.Fatalf("cross-org sandbox in alice list: %+v", v)
		}
	}
}

// TestGetMapsViewFields asserts Get returns the mapped view for an owned sandbox.
func TestGetMapsViewFields(t *testing.T) {
	c := newControl(t, sb("alice", "sb-a1", "Ready"))
	v, err := c.Get(context.Background(), "alice", "sb-a1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v.ID != "sb-a1" || v.OrgID != "alice" || v.Template != "python" || string(v.Phase) != "Ready" {
		t.Fatalf("view = %+v, want id/org/template/phase mapped", v)
	}
}

// TestGetCrossOrgIsNotFound asserts a sandbox owned by another org is reported
// as not-found (the namespace boundary plus the label check), indistinguishable
// from a missing one.
func TestGetCrossOrgIsNotFound(t *testing.T) {
	c := newControl(t, sb("bob", "sb-b1", "Ready"))
	if _, err := c.Get(context.Background(), "alice", "sb-b1"); err != console.ErrNotFound {
		t.Fatalf("cross-org Get err = %v, want console.ErrNotFound", err)
	}
}

// TestTerminateCrossOrgIsNotFoundAndSurvives asserts alice cannot terminate
// bob's sandbox, and it survives.
func TestTerminateCrossOrgIsNotFoundAndSurvives(t *testing.T) {
	c := newControl(t, sb("bob", "sb-b1", "Ready"))
	if err := c.Terminate(context.Background(), "alice", "sb-b1"); err != console.ErrNotFound {
		t.Fatalf("cross-org Terminate err = %v, want console.ErrNotFound", err)
	}
	if _, err := c.Get(context.Background(), "bob", "sb-b1"); err != nil {
		t.Fatalf("bob's sandbox was terminated cross-org: %v", err)
	}
}

// TestTerminateOwnedDeletes asserts terminating an owned sandbox removes it.
func TestTerminateOwnedDeletes(t *testing.T) {
	c := newControl(t, sb("alice", "sb-a1", "Ready"))
	if err := c.Terminate(context.Background(), "alice", "sb-a1"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if _, err := c.Get(context.Background(), "alice", "sb-a1"); err != console.ErrNotFound {
		t.Fatalf("sandbox not deleted: %v", err)
	}
}

// TestImplementsSandboxControl is a compile-time seam assertion.
func TestImplementsSandboxControl(t *testing.T) {
	var _ console.SandboxControl = (*Control)(nil)
}
