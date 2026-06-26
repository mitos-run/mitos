package clusterforktree

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/saas/console"
	"mitos.run/mitos/internal/tenant"
)

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1.AddToScheme(s); err != nil {
		t.Fatalf("add v1 scheme: %v", err)
	}
	return s
}

// root builds a v1.Sandbox started from a pool (a fork-tree root) owned by org.
func root(org, name, phase string) *v1.Sandbox {
	return &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: tenant.NamespaceForOrg(org),
			Labels:    tenant.OrgLabels(org),
		},
		Spec:   v1.SandboxSpec{Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: "python"}}},
		Status: v1.SandboxStatus{Phase: v1.SandboxPhase(phase)},
	}
}

// fork builds a v1.Sandbox forked from parent, owned by org.
func fork(org, name, parent, phase string) *v1.Sandbox {
	sb := root(org, name, phase)
	sb.Spec.Source = v1.SandboxSource{FromSandbox: &v1.FromSandboxSource{Name: parent}}
	return sb
}

func newSource(t *testing.T, objs ...client.Object) *Source {
	t.Helper()
	c := fakeclient.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objs...).Build()
	return New(c)
}

// TestTreeScopedToOrgNamespace asserts the fork tree returns ONLY the caller
// org's sandboxes: bob's, in bob's namespace, are never seen by alice.
func TestTreeScopedToOrgNamespace(t *testing.T) {
	c := newSource(t,
		root("alice", "sb-a1", "Ready"),
		fork("alice", "sb-a2", "sb-a1", "Ready"),
		root("bob", "sb-b1", "Ready"),
	)
	tree, err := c.Tree(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if tree.OrgID != "alice" {
		t.Fatalf("org = %q, want alice", tree.OrgID)
	}
	if len(tree.Nodes) != 2 {
		t.Fatalf("alice saw %d nodes, want 2: %+v", len(tree.Nodes), tree.Nodes)
	}
	for _, n := range tree.Nodes {
		if n.ID == "sb-b1" {
			t.Fatalf("bob node sb-b1 leaked into alice fork tree")
		}
	}
}

// TestTreeMapsForkLineage asserts a forked sandbox carries its source as parent
// and a pool-started sandbox is a root (empty parent).
func TestTreeMapsForkLineage(t *testing.T) {
	c := newSource(t,
		root("alice", "sb-a1", "Ready"),
		fork("alice", "sb-a2", "sb-a1", "Pending"),
	)
	tree, err := c.Tree(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	byID := map[string]console.ForkNode{}
	for _, n := range tree.Nodes {
		byID[n.ID] = n
	}
	if got := byID["sb-a1"].ParentID; got != "" {
		t.Fatalf("root parent = %q, want empty", got)
	}
	if got := byID["sb-a2"].ParentID; got != "sb-a1" {
		t.Fatalf("fork parent = %q, want sb-a1", got)
	}
	if got := byID["sb-a2"].Phase; got != "Pending" {
		t.Fatalf("fork phase = %q, want Pending", got)
	}
}

// TestTreeEmptyOrgIsEmpty asserts an org with no sandboxes gets an empty,
// org-scoped tree, never another org's nodes.
func TestTreeEmptyOrgIsEmpty(t *testing.T) {
	c := newSource(t, root("bob", "sb-b1", "Ready"))
	tree, err := c.Tree(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if len(tree.Nodes) != 0 {
		t.Fatalf("alice tree = %+v, want empty", tree.Nodes)
	}
}

// TestImplementsForkTreeSource is a compile-time seam assertion.
func TestImplementsForkTreeSource(t *testing.T) {
	var _ console.ForkTreeSource = (*Source)(nil)
}
