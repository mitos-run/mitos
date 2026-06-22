// Package clustersandbox is the real console.SandboxControl: it queries the
// controller's v1alpha2 Sandbox records scoped to one org, the cluster-backed
// implementation of the live-sandbox seam (issue #2). Under hard isolation each
// org's sandboxes live in its own namespace (tenant.NamespaceForOrg), so org
// scoping is the namespace boundary plus the org label as defense in depth — a
// cross-org id is reported as not-found, indistinguishable from a missing one.
package clustersandbox

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv2 "mitos.run/mitos/api/v1alpha2"
	"mitos.run/mitos/internal/saas/console"
	"mitos.run/mitos/internal/tenant"
)

// Control implements console.SandboxControl against the Kubernetes API.
type Control struct {
	c client.Client
}

// New builds the cluster-backed sandbox control.
func New(c client.Client) *Control {
	return &Control{c: c}
}

// List returns the org's sandboxes from its namespace, filtered by the org
// label.
func (s *Control) List(ctx context.Context, orgID string) ([]console.SandboxView, error) {
	var list sandboxv2.SandboxList
	if err := s.c.List(ctx, &list,
		client.InNamespace(tenant.NamespaceForOrg(orgID)),
		client.MatchingLabels(tenant.OrgLabels(orgID)),
	); err != nil {
		return nil, fmt.Errorf("list sandboxes: %w", err)
	}
	out := make([]console.SandboxView, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, viewOf(&list.Items[i], orgID))
	}
	return out, nil
}

// Get returns one of the org's sandboxes by name. A sandbox in another org's
// namespace (or missing, or not carrying the org label) is console.ErrNotFound.
func (s *Control) Get(ctx context.Context, orgID, sandboxID string) (console.SandboxView, error) {
	sb, err := s.get(ctx, orgID, sandboxID)
	if err != nil {
		return console.SandboxView{}, err
	}
	return viewOf(sb, orgID), nil
}

// Terminate deletes one of the org's sandboxes. A cross-org or missing id is
// console.ErrNotFound and nothing is deleted.
func (s *Control) Terminate(ctx context.Context, orgID, sandboxID string) error {
	sb, err := s.get(ctx, orgID, sandboxID)
	if err != nil {
		return err
	}
	if err := s.c.Delete(ctx, sb); err != nil {
		if apierrors.IsNotFound(err) {
			return console.ErrNotFound
		}
		return fmt.Errorf("delete sandbox: %w", err)
	}
	return nil
}

// get fetches the org's sandbox by name from its namespace and verifies the org
// label, collapsing missing / cross-org / mislabeled into ErrNotFound.
func (s *Control) get(ctx context.Context, orgID, sandboxID string) (*sandboxv2.Sandbox, error) {
	var sb sandboxv2.Sandbox
	key := client.ObjectKey{Namespace: tenant.NamespaceForOrg(orgID), Name: sandboxID}
	if err := s.c.Get(ctx, key, &sb); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, console.ErrNotFound
		}
		return nil, fmt.Errorf("get sandbox: %w", err)
	}
	if sb.Labels[tenant.OrgLabelKey] != orgID {
		return nil, console.ErrNotFound // present but not labelled for this org
	}
	return &sb, nil
}

// viewOf maps a v1alpha2.Sandbox to the console view. Template is the pool the
// sandbox started from; the engine id and node-bearing pod come from status.
func viewOf(sb *sandboxv2.Sandbox, orgID string) console.SandboxView {
	template := ""
	if sb.Spec.Source.PoolRef != nil {
		template = sb.Spec.Source.PoolRef.Name
	}
	return console.SandboxView{
		ID:        sb.Name,
		OrgID:     orgID,
		Template:  template,
		Node:      sb.Status.Pod,
		Phase:     string(sb.Status.Phase),
		CreatedAt: sb.CreationTimestamp.Time,
	}
}
