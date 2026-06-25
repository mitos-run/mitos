package usage

import "testing"

// fakeLabelLookup is a fake LabelLookup: a fixed sandbox-id -> labels map. A
// missing sandbox returns (nil, false), modeling a sandbox whose backing pod the
// cache has not (yet) observed.
type fakeLabelLookup map[string]map[string]string

func (f fakeLabelLookup) LabelsForSandbox(sandboxID string) (map[string]string, bool) {
	ls, ok := f[sandboxID]
	return ls, ok
}

// TestLabelOrgResolver is the billing trust boundary at resolve time (issue
// #164): the org of a sandbox is read ONLY from the trusted mitos.run/org label
// the controller stamped on the sandbox's husk pod from the per-org namespace.
// An unlabeled sandbox (self-host single-tenant) is unattributed, NOT forced
// into a default org, so the unknown path stays auditable. No client-provided
// value is ever consulted.
func TestLabelOrgResolver(t *testing.T) {
	lookup := fakeLabelLookup{
		// Attributed: the controller stamped the trusted org label.
		"sb-acme": {"mitos.run/org": "acme", "mitos.run/husk": "true"},
		// Self-host: a husk pod with no org label (non-org namespace).
		"sb-selfhost": {"mitos.run/husk": "true"},
		// Defensive: an empty org-label value is treated as unattributed, not as
		// the org "".
		"sb-empty": {"mitos.run/org": ""},
	}
	r := NewLabelOrgResolver(lookup)

	if org, ok := r.OrgFor("sb-acme"); !ok || org != "acme" {
		t.Errorf("OrgFor(sb-acme) = (%q, %t), want (acme, true)", org, ok)
	}
	if org, ok := r.OrgFor("sb-selfhost"); ok || org != "" {
		t.Errorf("OrgFor(sb-selfhost) = (%q, %t), want (\"\", false)", org, ok)
	}
	if org, ok := r.OrgFor("sb-empty"); ok || org != "" {
		t.Errorf("OrgFor(sb-empty) = (%q, %t), want (\"\", false)", org, ok)
	}
	if org, ok := r.OrgFor("sb-unknown"); ok || org != "" {
		t.Errorf("OrgFor(sb-unknown) = (%q, %t), want (\"\", false)", org, ok)
	}
}

// TestLabelOrgResolverImplementsInterface pins that the live resolver satisfies
// the OrgResolver seam the collector consumes, so it can be swapped for the
// StaticOrgs test default without further wiring.
func TestLabelOrgResolverImplementsInterface(t *testing.T) {
	var _ OrgResolver = NewLabelOrgResolver(fakeLabelLookup{})
}
