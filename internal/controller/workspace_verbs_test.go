package controller

import (
	"context"
	v1 "mitos.run/mitos/api/v1"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// validDigest is a valid lowercase-hex sha256 content-addressed manifest digest
// for use as a committed revision's ContentManifest in unit tests.
const validDigest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// testScheme builds a runtime scheme with the mitos v1alpha1 types registered,
// for the in-package (controller) fork/revert verb unit tests. The envtest
// suite uses its own scheme in the external controller_test package.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return s
}

func TestForkRevisionCreatesLineageEdge(t *testing.T) {
	parent := &v1.WorkspaceRevision{
		ObjectMeta: metav1.ObjectMeta{Name: "src-1", Namespace: "ns"},
		Spec: v1.WorkspaceRevisionSpec{
			WorkspaceRef:    v1.LocalObjectReference{Name: "src"},
			ContentManifest: validDigest,
		},
		Status: v1.WorkspaceRevisionStatus{Phase: v1.WorkspaceRevisionCommitted},
	}
	dst := &v1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "branch", Namespace: "ns"}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(parent, dst).Build()

	v := &WorkspaceVerbs{Client: c}
	rev, err := v.Fork(context.Background(), "ns", "src", "src-1", "branch")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if rev.Spec.Source.FromWorkspaceRevision == nil ||
		rev.Spec.Source.FromWorkspaceRevision.Revision != "src-1" {
		t.Fatalf("fork revision missing fromWorkspaceRevision edge: %+v", rev.Spec.Source)
	}
	if rev.Spec.WorkspaceRef.Name != "branch" {
		t.Fatalf("fork revision belongs to %q, want branch", rev.Spec.WorkspaceRef.Name)
	}
	if rev.Spec.ContentManifest != validDigest {
		t.Fatalf("fork must share the parent content manifest (content-addressed branch), got %q", rev.Spec.ContentManifest)
	}
}

func TestForkRejectsUncommittedParentWithLLMError(t *testing.T) {
	parent := &v1.WorkspaceRevision{
		ObjectMeta: metav1.ObjectMeta{Name: "src-1", Namespace: "ns"},
		Spec:       v1.WorkspaceRevisionSpec{WorkspaceRef: v1.LocalObjectReference{Name: "src"}},
		Status:     v1.WorkspaceRevisionStatus{Phase: v1.WorkspaceRevisionPending},
	}
	dst := &v1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "branch", Namespace: "ns"}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(parent, dst).Build()

	v := &WorkspaceVerbs{Client: c}
	_, err := v.Fork(context.Background(), "ns", "src", "src-1", "branch")
	var rej *RejectionError
	if !asRejection(err, &rej) {
		t.Fatalf("want RejectionError, got %v", err)
	}
	if rej.Code != "revision_not_committed" || rej.Remediation == "" {
		t.Fatalf("rejection must be LLM-legible {code,remediation}: %+v", rej)
	}
}

func TestRevertCreatesNewTipInSameWorkspace(t *testing.T) {
	parent := &v1.WorkspaceRevision{
		ObjectMeta: metav1.ObjectMeta{Name: "proj-1", Namespace: "ns"},
		Spec: v1.WorkspaceRevisionSpec{
			WorkspaceRef:    v1.LocalObjectReference{Name: "proj"},
			ContentManifest: validDigest,
		},
		Status: v1.WorkspaceRevisionStatus{Phase: v1.WorkspaceRevisionCommitted},
	}
	ws := &v1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "proj", Namespace: "ns"}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(parent, ws).Build()

	v := &WorkspaceVerbs{Client: c}
	rev, err := v.Revert(context.Background(), "ns", "proj", "proj-1")
	if err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if rev.Spec.WorkspaceRef.Name != "proj" {
		t.Fatalf("revert tip belongs to %q, want proj", rev.Spec.WorkspaceRef.Name)
	}
	if rev.Spec.Source.FromWorkspaceRevision == nil ||
		rev.Spec.Source.FromWorkspaceRevision.Revision != "proj-1" {
		t.Fatalf("revert tip missing lineage edge to proj-1: %+v", rev.Spec.Source)
	}
}
