package usage

import "mitos.run/mitos/internal/tenant"

// LabelLookup returns the labels the controller stamped on a sandbox's backing
// husk pod, keyed by the sandbox id (the forkd-side sandbox/VM id, which the
// controller sets as the husk pod's --vm-id). It is the injectable seam over the
// controller pod cache / k8s lister, so the live resolver is unit-testable
// against a fake map without a real client. ok is false when the cache has not
// observed a pod for the sandbox (a just-created or already-gone sandbox).
//
// The returned labels are the controller's OWN stamped label set (see
// tenant.OrgLabelKey and the husk pod builder): the org label there was derived
// from the trusted per-org namespace, never from client input. The resolver
// trusts ONLY this label.
type LabelLookup interface {
	LabelsForSandbox(sandboxID string) (labels map[string]string, ok bool)
}

// LabelOrgResolver is the live OrgResolver (issue #164): it attributes a sandbox
// to its org by reading the TRUSTED tenant.OrgLabelKey (mitos.run/org) label the
// controller stamped on the sandbox's husk pod from the per-org namespace. It is
// the production replacement for StaticOrgs.
//
// It never consults a client-provided value: the only input is the controller's
// own label, so a tenant cannot bill another org. A sandbox whose pod carries no
// (or an empty) org label is UNATTRIBUTED (ok=false), which keeps self-host
// single-tenant working (the record stays in the physical-footprint totals but is
// dropped from billable samples) rather than being forced into a default org.
type LabelOrgResolver struct {
	lookup LabelLookup
}

// NewLabelOrgResolver builds the live resolver over a LabelLookup (the controller
// pod cache / k8s lister seam).
func NewLabelOrgResolver(lookup LabelLookup) *LabelOrgResolver {
	return &LabelOrgResolver{lookup: lookup}
}

// OrgFor implements OrgResolver. It returns (orgID, true) only when the sandbox's
// husk pod carries a non-empty trusted mitos.run/org label; otherwise ("", false)
// for the unattributed (self-host) path.
func (r *LabelOrgResolver) OrgFor(sandboxID string) (string, bool) {
	labels, ok := r.lookup.LabelsForSandbox(sandboxID)
	if !ok {
		return "", false
	}
	org := labels[tenant.OrgLabelKey]
	if org == "" {
		return "", false
	}
	return org, true
}
