package tenant

import "testing"

// TestNamespaceForOrg asserts the canonical hard-isolation namespace mapping:
// each org's workloads (sandboxes + secrets) live in mitos-org-<id>.
func TestNamespaceForOrg(t *testing.T) {
	if got := NamespaceForOrg("alice"); got != "mitos-org-alice" {
		t.Fatalf("NamespaceForOrg(alice) = %q, want mitos-org-alice", got)
	}
}

// TestOrgLabelStable pins the org label key: the contract the gateway stamps
// and the console / usage resolver read. Changing it is a breaking change.
func TestOrgLabelStable(t *testing.T) {
	if OrgLabelKey != "mitos.run/org" {
		t.Fatalf("OrgLabelKey = %q, want mitos.run/org", OrgLabelKey)
	}
}

// TestOrgLabels builds the standard label set carrying the org, for stamping on
// claims/sandboxes.
func TestOrgLabels(t *testing.T) {
	ls := OrgLabels("alice")
	if ls[OrgLabelKey] != "alice" {
		t.Fatalf("OrgLabels(alice)[%s] = %q, want alice", OrgLabelKey, ls[OrgLabelKey])
	}
}

// TestOrgFromNamespace pins the inverse of NamespaceForOrg: an org namespace
// maps back to its org id, and a non-org namespace is not attributed. This is
// the TRUSTED source of the billing org: the controller derives the org from the
// namespace the control plane placed the workload in, never from client input.
func TestOrgFromNamespace(t *testing.T) {
	cases := []struct {
		ns      string
		wantOrg string
		wantOK  bool
	}{
		{"mitos-org-acme", "acme", true},
		{"mitos-org-alice", "alice", true},
		// Round-trips through NamespaceForOrg for an org id with hyphens.
		{NamespaceForOrg("team-7"), "team-7", true},
		// Non-org namespaces (self-host single-tenant) are not attributed.
		{"default", "", false},
		{"mitos", "", false},
		{"kube-system", "", false},
		{"", "", false},
		// The bare prefix with no org id is not a valid org namespace.
		{"mitos-org-", "", false},
	}
	for _, c := range cases {
		gotOrg, gotOK := OrgFromNamespace(c.ns)
		if gotOrg != c.wantOrg || gotOK != c.wantOK {
			t.Errorf("OrgFromNamespace(%q) = (%q, %t), want (%q, %t)",
				c.ns, gotOrg, gotOK, c.wantOrg, c.wantOK)
		}
	}
}
