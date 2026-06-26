// Package clusterforktree is the real console.ForkTreeSource: it builds an org's
// live fork tree from the controller's v1 Sandbox records scoped to one org.
// Under hard isolation each org's sandboxes live in its own namespace
// (tenant.NamespaceForOrg), so org scoping is the namespace boundary plus the
// org label as defense in depth: an org only ever sees its own namespace's
// sandboxes, exactly like the cluster sandbox control (internal/saas/console/
// clustersandbox) and the cross-tenant boundary in internal/saas/controlplane.
//
// The edges are the fork lineage: a Sandbox created from another sandbox
// (spec.source.fromSandbox) carries that source as its parent. A Sandbox with no
// fromSandbox source is a root. The CoW byte split (private-dirty vs shared) is
// per-node metering that is NOT carried on the Sandbox CR; it is left zero here
// and is the documented metering follow-up (the same #33 metering the usage
// collector scrapes), so this source never fabricates a CoW number it has not
// measured.
package clusterforktree

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/saas/console"
	"mitos.run/mitos/internal/tenant"
)

// Source implements console.ForkTreeSource against the Kubernetes API.
type Source struct {
	c client.Client
}

// New builds the cluster-backed fork-tree source.
func New(c client.Client) *Source {
	return &Source{c: c}
}

// Tree returns the org's fork tree from its namespace, filtered by the org label.
// It only ever returns the named org's nodes: the List is scoped to the org's
// namespace AND matches the org label, so a sandbox in another org's namespace is
// never observed.
func (s *Source) Tree(ctx context.Context, orgID string) (console.ForkTree, error) {
	var list v1.SandboxList
	if err := s.c.List(ctx, &list,
		client.InNamespace(tenant.NamespaceForOrg(orgID)),
		client.MatchingLabels(tenant.OrgLabels(orgID)),
	); err != nil {
		return console.ForkTree{}, fmt.Errorf("list sandboxes: %w", err)
	}
	nodes := make([]console.ForkNode, 0, len(list.Items))
	for i := range list.Items {
		nodes = append(nodes, nodeOf(&list.Items[i]))
	}
	return console.ForkTree{OrgID: orgID, Nodes: nodes}, nil
}

// nodeOf maps a v1.Sandbox to a fork-tree node. The parent is the live sandbox
// this one was forked from (spec.source.fromSandbox); a sandbox started from a
// pool snapshot or a workspace revision is a root (empty parent). The CoW byte
// split is not on the CR and is left zero (the metering follow-up).
func nodeOf(sb *v1.Sandbox) console.ForkNode {
	parent := ""
	if sb.Spec.Source.FromSandbox != nil {
		parent = sb.Spec.Source.FromSandbox.Name
	}
	return console.ForkNode{
		ID:        sb.Name,
		ParentID:  parent,
		Phase:     string(sb.Status.Phase),
		CreatedAt: sb.CreationTimestamp.Time,
	}
}
