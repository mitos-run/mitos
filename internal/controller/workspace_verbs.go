package controller

import (
	"context"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/cas"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// RejectionError is the LLM-legible error the workspace verbs return when they
// refuse an operation (issue #28). Code is a stable machine identifier; Cause is
// the underlying detail; Remediation is a short actionable hint. No secret value
// ever appears in any field (workspaces carry content only; secrets are excluded
// at dehydrate, see the secret-exclude paths on the binding path).
type RejectionError struct {
	Code        string
	Message     string
	Cause       string
	Remediation string
}

func (e *RejectionError) Error() string {
	return fmt.Sprintf("[%s] %s: %s (remediation: %s)", e.Code, e.Message, e.Cause, e.Remediation)
}

// asRejection reports whether err is (or wraps) a *RejectionError, binding it to
// target. It is errors.As specialized to the workspace rejection type so the SDK
// and CLI backends and tests can branch on a structured rejection.
func asRejection(err error, target **RejectionError) bool { return errors.As(err, target) }

// WorkspaceVerbs materializes the git-shaped workspace operations the SDK and CLI
// expose: fork (branch a committed revision into a new workspace), revert /
// checkout (advance a workspace head to a past revision by creating a new
// revision that shares its content). It only ever CREATES revision objects with
// the right lineage; the WorkspaceReconciler commits them and advances the head.
type WorkspaceVerbs struct {
	client.Client
}

// Fork creates a new committed-content revision in dstWorkspace whose lineage
// edge points at srcRevision in srcWorkspace, sharing the parent content
// manifest (a content-addressed branch: zero new bytes). The destination
// workspace must already exist. A non-committed parent is rejected with an
// LLM-legible error.
func (v *WorkspaceVerbs) Fork(ctx context.Context, namespace, srcWorkspace, srcRevision, dstWorkspace string) (*v1.WorkspaceRevision, error) {
	var parent v1.WorkspaceRevision
	if err := v.Get(ctx, types.NamespacedName{Namespace: namespace, Name: srcRevision}, &parent); err != nil {
		return nil, &RejectionError{
			Code: "revision_not_found", Message: "fork source revision not found",
			Cause:       fmt.Sprintf("revision %q in namespace %q does not exist", srcRevision, namespace),
			Remediation: "List revisions with `mitos ws log <workspace>` and fork an existing one.",
		}
	}
	if parent.Spec.WorkspaceRef.Name != srcWorkspace {
		return nil, &RejectionError{
			Code: "revision_workspace_mismatch", Message: "fork source revision belongs to a different workspace",
			Cause:       fmt.Sprintf("revision %q belongs to workspace %q, not %q", srcRevision, parent.Spec.WorkspaceRef.Name, srcWorkspace),
			Remediation: "Pass the workspace the revision actually belongs to.",
		}
	}
	if parent.Status.Phase != v1.WorkspaceRevisionCommitted || cas.Digest(parent.Spec.ContentManifest).Validate() != nil {
		return nil, &RejectionError{
			Code: "revision_not_committed", Message: "cannot fork an uncommitted revision",
			Cause:       fmt.Sprintf("revision %q is in phase %q with no valid content manifest", srcRevision, parent.Status.Phase),
			Remediation: "Wait for the revision to commit (its dehydrate to finish) before forking it.",
		}
	}
	var dst v1.Workspace
	if err := v.Get(ctx, types.NamespacedName{Namespace: namespace, Name: dstWorkspace}, &dst); err != nil {
		return nil, &RejectionError{
			Code: "workspace_not_found", Message: "fork destination workspace not found",
			Cause:       fmt.Sprintf("workspace %q does not exist in namespace %q", dstWorkspace, namespace),
			Remediation: "Create the destination workspace first with `mitos ws create <name>`.",
		}
	}
	rev := &v1.WorkspaceRevision{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: dstWorkspace + "-",
			Namespace:    namespace,
			Labels:       map[string]string{WorkspaceLabel: dstWorkspace},
		},
		Spec: v1.WorkspaceRevisionSpec{
			WorkspaceRef: v1.LocalObjectReference{Name: dstWorkspace},
			Source: v1.RevisionSource{FromWorkspaceRevision: &v1.WorkspaceRevisionRef{
				Workspace: srcWorkspace, Revision: srcRevision,
			}},
			ContentManifest: parent.Spec.ContentManifest,
		},
		Status: v1.WorkspaceRevisionStatus{Phase: v1.WorkspaceRevisionPending},
	}
	if err := v.Create(ctx, rev); err != nil {
		return nil, fmt.Errorf("create fork revision in workspace %s: %w", dstWorkspace, err)
	}
	return rev, nil
}

// Revert advances a workspace's head to the content of a past revision in the
// SAME workspace by creating a new revision that shares that content and records
// a fromWorkspaceRevision edge to it. It never rewrites history (revisions are
// immutable, single-writer-per-revision): a revert is a new tip. Checkout is an
// alias callers use when the intent is "make this old state the new head".
func (v *WorkspaceVerbs) Revert(ctx context.Context, namespace, workspace, targetRevision string) (*v1.WorkspaceRevision, error) {
	return v.Fork(ctx, namespace, workspace, targetRevision, workspace)
}
