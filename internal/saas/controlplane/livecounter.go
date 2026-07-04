package controlplane

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/saas/quota"
	"mitos.run/mitos/internal/tenant"
)

// LiveCounter implements quota.LiveCounter over live Kubernetes state: it
// lists the org's Sandbox objects (the same namespace + org-label scoping the
// control plane's list path uses) and reports every sandbox in a NON-terminal
// phase as live. This is the authoritative concurrency input the gateway's
// quota enforcer wants (issue #615 seam 2): what the org is running right now,
// not a lagging usage-window tally.
//
// Phase semantics: Pending, Restoring, Ready, Terminating, and the empty
// just-created phase all count (each holds or is about to hold capacity);
// Terminated and Failed do not.
//
// Aggregate footprint (VCPUs, MemBytes, StorageBytes) is left at ZERO for now:
// a Sandbox carries no per-create resource fields, so honest aggregates
// require resolving each sandbox's pool profile. That pool-resolved aggregate
// count, together with wiring the gateway adapter's SizeOf seam, is an
// explicitly deferred follow-up of issue #615; until then only the concurrency
// cap is enforced from this counter and the aggregate caps stay inert.
//
// Failure posture: a List error is RETURNED, never swallowed. The quota
// enforcer maps a live-usage error to a deny, which is correct on the
// anti-abuse path: a transient apiserver blip denying a create beats reading
// an unreachable cluster as "zero live sandboxes".
type LiveCounter struct {
	c client.Client
	// singleTenantNamespace mirrors K8sControlPlane's single-tenant mode: when
	// non-empty every org's sandboxes live in this fixed namespace (org labels
	// stay authoritative); when empty the per-org mitos-org-<id> namespace is
	// used.
	singleTenantNamespace string
}

// NewLiveCounter builds a LiveCounter over a controller-runtime client whose
// scheme has mitos.run/v1 registered. singleTenantNamespace must match the
// control plane's WithSingleTenantNamespace setting (empty for per-org
// namespacing) so the counter looks where the control plane creates.
func NewLiveCounter(c client.Client, singleTenantNamespace string) *LiveCounter {
	return &LiveCounter{c: c, singleTenantNamespace: singleTenantNamespace}
}

// compile-time assertion that LiveCounter satisfies the quota seam.
var _ quota.LiveCounter = (*LiveCounter)(nil)

// Count returns the org's live footprint now. See the type comment for the
// phase semantics and the zero-aggregate caveat.
func (l *LiveCounter) Count(ctx context.Context, orgID string) (quota.LiveUsage, error) {
	ns := l.singleTenantNamespace
	if ns == "" {
		ns = tenant.NamespaceForOrg(orgID)
	}
	var sbs v1.SandboxList
	err := l.c.List(ctx, &sbs,
		client.InNamespace(ns),
		client.MatchingLabels(tenant.OrgLabels(orgID)),
	)
	if err != nil {
		// Fail closed: the enforcer denies on a live-usage error. The error text
		// carries the org id and namespace (non-secret identifiers) only.
		return quota.LiveUsage{}, fmt.Errorf("list live sandboxes for org %s in %s: %w", orgID, ns, err)
	}
	n := 0
	for i := range sbs.Items {
		// Defense in depth: the namespace and the label selector already bound
		// the list to this org; re-checking the label keeps the invariant local
		// (the same posture as the control plane's list path).
		if sbs.Items[i].Labels[tenant.OrgLabelKey] != orgID {
			continue
		}
		switch sbs.Items[i].Status.Phase {
		case v1.SandboxTerminated, v1.SandboxFailed:
			continue
		}
		n++
	}
	return quota.LiveUsage{ConcurrentSandboxes: n}, nil
}
