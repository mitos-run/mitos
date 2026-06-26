package agentcli

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "mitos.run/mitos/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestClusterCreateAndLogWorkspace(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(Scheme()).Build()
	b := NewClusterBackend(c, "ns", nil)
	ws := b.Workspace()
	if err := ws.CreateWorkspace(context.Background(), "proj-x"); err != nil {
		t.Fatalf("create: %v", err)
	}
	var got v1.Workspace
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "proj-x"}, &got); err != nil {
		t.Fatalf("workspace not created: %v", err)
	}

	rev := &v1.WorkspaceRevision{
		ObjectMeta: metav1.ObjectMeta{Name: "proj-x-1", Namespace: "ns"},
		Spec: v1.WorkspaceRevisionSpec{
			WorkspaceRef: v1.LocalObjectReference{Name: "proj-x"},
			Source:       v1.RevisionSource{FromClaim: "c1"},
		},
		Status: v1.WorkspaceRevisionStatus{Phase: v1.WorkspaceRevisionCommitted},
	}
	if err := c.Create(context.Background(), rev); err != nil {
		t.Fatalf("seed revision: %v", err)
	}
	revs, err := ws.Log(context.Background(), "proj-x")
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	if len(revs) != 1 || revs[0].Name != "proj-x-1" || revs[0].Lineage != "fromClaim:c1" {
		t.Fatalf("unexpected log: %+v", revs)
	}
}

func TestServeCreatesExposedSandbox(t *testing.T) {
	scheme := Scheme()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1.Sandbox{}).
		Build()

	wb := &ClusterWorkspaceBackend{
		client:       c,
		namespace:    "ns",
		now:          time.Now,
		pollInterval: time.Millisecond,
		pollTimeout:  2 * time.Second,
	}

	// readyHook simulates the controller reconciling the sandbox to Ready.
	wb.readyHook = func(ctx context.Context, name string) {
		var sandbox v1.Sandbox
		if err := c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: name}, &sandbox); err != nil {
			return
		}
		sandbox.Status.Phase = v1.SandboxReady
		sandbox.Status.Endpoint = "10.0.0.5:9091"
		_ = c.Status().Update(ctx, &sandbox)
	}

	res, err := wb.Serve(context.Background(), "myws", "mitos.app", ServeOptions{
		Pool: "p",
		Port: 8080,
	})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}

	// Verify the created Sandbox has the expected fields.
	var sbxList v1.SandboxList
	if err := c.List(context.Background(), &sbxList, client.InNamespace("ns")); err != nil {
		t.Fatalf("list sandboxes: %v", err)
	}
	if len(sbxList.Items) != 1 {
		t.Fatalf("want 1 sandbox, got %d", len(sbxList.Items))
	}
	sbx := &sbxList.Items[0]

	if sbx.Spec.WorkspaceRef == nil || sbx.Spec.WorkspaceRef.Name != "myws" {
		t.Fatalf("workspaceRef = %v, want myws", sbx.Spec.WorkspaceRef)
	}
	if sbx.Spec.Expose == nil {
		t.Fatalf("expose is nil, want non-nil")
	}
	if sbx.Spec.Expose.Port != 8080 {
		t.Fatalf("expose.Port = %d, want 8080", sbx.Spec.Expose.Port)
	}
	if sbx.Spec.Expose.Sharing != "private" {
		t.Fatalf("expose.Sharing = %q, want private", sbx.Spec.Expose.Sharing)
	}

	if !strings.HasPrefix(res.URL, "https://") {
		t.Fatalf("URL = %q, want https:// prefix", res.URL)
	}
	if !strings.HasSuffix(res.URL, ".mitos.app/") {
		t.Fatalf("URL = %q, want .mitos.app/ suffix", res.URL)
	}
	if res.SandboxName == "" {
		t.Fatalf("SandboxName is empty")
	}
}

func TestServeRequiresPoolAndDomain(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(Scheme()).Build()
	wb := &ClusterWorkspaceBackend{
		client:    c,
		namespace: "ns",
		now:       time.Now,
	}

	t.Run("empty pool", func(t *testing.T) {
		_, err := wb.Serve(context.Background(), "myws", "mitos.app", ServeOptions{Pool: ""})
		if err == nil {
			t.Fatal("want error for empty pool, got nil")
		}
		if !strings.Contains(err.Error(), "pool") {
			t.Fatalf("error = %q, want containing 'pool'", err.Error())
		}
	})

	t.Run("empty domain", func(t *testing.T) {
		_, err := wb.Serve(context.Background(), "myws", "", ServeOptions{Pool: "p"})
		if err == nil {
			t.Fatal("want error for empty domain, got nil")
		}
		if !strings.Contains(err.Error(), "domain") {
			t.Fatalf("error = %q, want containing 'domain'", err.Error())
		}
	})
}

func TestClusterForkRejectsUncommittedWithRejectionError(t *testing.T) {
	parent := &v1.WorkspaceRevision{
		ObjectMeta: metav1.ObjectMeta{Name: "proj-x-1", Namespace: "ns"},
		Spec: v1.WorkspaceRevisionSpec{
			WorkspaceRef: v1.LocalObjectReference{Name: "proj-x"},
		},
		Status: v1.WorkspaceRevisionStatus{Phase: v1.WorkspaceRevisionPending},
	}
	dst := &v1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "branch", Namespace: "ns"}}
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(parent, dst).Build()
	b := NewClusterBackend(c, "ns", nil)

	_, err := b.Workspace().Fork(context.Background(), "proj-x", "proj-x-1", "branch")
	if err == nil {
		t.Fatalf("want rejection, got nil")
	}
}
