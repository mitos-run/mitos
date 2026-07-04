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
// Terminated and Failed do not. Terminating counting as live is deliberate (a
// tearing-down VM still holds capacity), with the honest consequence that a
// WEDGED teardown consumes a concurrency slot until it resolves.
//
// Replica semantics: a Sandbox with Spec.Replicas = N counts as N (the fork
// fan-out is N running VMs; the create path accepts a client-supplied
// replicas value, so counting objects would let a capped org fan out past its
// tier).
//
// Admission semantics (honest limits): the enforcer's check-then-create has NO
// reservation step, so parallel in-flight creates can each read the same
// pre-create count and overshoot the cap; the overshoot is bounded by the
// tier's creation-rate bucket burst (free tier: 5/min). And a create request
// asking for replicas = N is admitted as +1 today, not +N: the create body is
// not parsed at enforcement time (the same gap as the unwired SizeOf seam,
// same deferred follow-up), so the fan-out lands in the NEXT create's count
// rather than gating its own. Each create costs one uncached org-scoped LIST
// against the apiserver, which is fine at current fleet sizes; a cached or
// paginated counter is part of the same follow-up.
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
		// Replicas is the fork fan-out: ONE Sandbox object with replicas = N is N
		// running VMs, and the create path accepts a client-supplied replicas
		// value, so counting objects instead of replicas would let a capped org
		// fan out far past its tier (2 objects x 100 replicas on a cap of 2).
		// Unset/zero/one all count as one.
		r := int(sbs.Items[i].Spec.Replicas)
		if r < 1 {
			r = 1
		}
		n += r
	}
	return quota.LiveUsage{ConcurrentSandboxes: n}, nil
}
