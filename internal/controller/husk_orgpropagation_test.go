package controller

import (
	"testing"

	"mitos.run/mitos/internal/tenant"
)

// TestPropagateOrgToHuskPod is the claim-time org attribution unit for the
// pre-claimed checkout: the gateway stamps the org on a formerly buffered
// sandbox's CR labels, and the reconcile triggered by that patch must
// propagate the org to the backing husk pod, whose TRUSTED label is what the
// usage scraper bills. It must be idempotent and a no-op for classic claims
// whose pod was labeled at claim time.
func TestPropagateOrgToHuskPod(t *testing.T) {
	// A checked-out claim: CR has the org, pod does not -> propagate.
	c := rdyClaim()
	c.Labels = tenant.OrgLabels("acme")
	p := rdyPod(true)
	if !propagateOrgToHuskPod(c, p) {
		t.Fatal("org-labeled claim with an org-less pod must propagate")
	}
	if p.Labels[tenant.OrgLabelKey] != "acme" {
		t.Fatalf("pod org label = %q, want acme", p.Labels[tenant.OrgLabelKey])
	}

	// Idempotent: a second pass changes nothing.
	if propagateOrgToHuskPod(c, p) {
		t.Error("already-propagated pod must not re-patch")
	}

	// Classic claim: pod already labeled at claim time -> no-op.
	c2 := rdyClaim()
	c2.Labels = tenant.OrgLabels("acme")
	p2 := rdyPod(true)
	p2.Labels = map[string]string{tenant.OrgLabelKey: "acme"}
	if propagateOrgToHuskPod(c2, p2) {
		t.Error("matching labels must not report a change")
	}

	// An org-less claim (a still-buffered sandbox) must never stamp anything.
	c3 := rdyClaim()
	p3 := rdyPod(true)
	if propagateOrgToHuskPod(c3, p3) {
		t.Error("org-less claim must not propagate")
	}
	if _, has := p3.Labels[tenant.OrgLabelKey]; has {
		t.Error("org-less claim stamped a pod label")
	}

	// A nil pod (lost, being repended) is a no-op, never a panic.
	if propagateOrgToHuskPod(c, nil) {
		t.Error("nil pod must be a no-op")
	}

	// The threat-model invariant: a claim label can FILL attribution where
	// none derives, but can never OVERRIDE an existing pod org label (which
	// is control-plane identity: namespace-derived or stamped at claim time).
	// A tenant-set CR label must not rebill a pod to another org.
	c4 := rdyClaim()
	c4.Labels = tenant.OrgLabels("acme")
	p4 := rdyPod(true)
	p4.Labels = map[string]string{tenant.OrgLabelKey: "other"}
	if propagateOrgToHuskPod(c4, p4) {
		t.Fatal("an existing pod org label must never be rewritten")
	}
	if p4.Labels[tenant.OrgLabelKey] != "other" {
		t.Fatalf("pod org label = %q, want the original untouched", p4.Labels[tenant.OrgLabelKey])
	}
}
