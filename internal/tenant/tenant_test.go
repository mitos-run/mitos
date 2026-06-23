package tenant

import "testing"

// TestNamespaceForOrg asserts the canonical hard-isolation namespace mapping:
// each org's workloads (sandboxes + secrets) live in mitos-org-<id>.
func TestNamespaceForOrg(t *testing.T) {
	if got := NamespaceForOrg("alice"); got != "mitos-org-alice" {
		t.Fatalf("NamespaceForOrg(alice) = %q, want mitos-org-alice", got)
	}
}

// TestOrgLabelStable pins the org label key — the contract the gateway stamps
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
