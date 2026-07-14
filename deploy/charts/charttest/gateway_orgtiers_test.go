package charttest

import (
	"strings"
	"testing"
)

// Operator tier grants (mitos-run/mitos #881): the gateway grew the repeatable
// --org-tier flag, but the chart never rendered it, so neither the hosted
// operator nor a self-hoster could grant an org a tier above the fail-closed
// free default through values. These tests pin the wiring: off by default,
// one flag per granted org when set, and the schema rejecting unknown tiers.

// TestGatewayOrgTiersOffByDefault: a default render carries no tier grants;
// every org stays on the fail-closed free default.
func TestGatewayOrgTiersOffByDefault(t *testing.T) {
	if strings.Contains(render(t), "--org-tier") {
		t.Fatal("--org-tier rendered by default; tier grants must be operator opt-in")
	}
}

// TestGatewayOrgTiersRenderPerOrg: each granted org renders its own
// --org-tier=<org>=<tier> flag on the gateway container.
func TestGatewayOrgTiersRenderPerOrg(t *testing.T) {
	out := render(t,
		"gateway.orgTiers.org-bench=pro",
		"gateway.orgTiers.org-partner=starter",
	)
	for _, needle := range []string{
		"- --org-tier=org-bench=pro",
		"- --org-tier=org-partner=starter",
	} {
		if !strings.Contains(out, needle) {
			t.Fatalf("rendered manifests missing %q", needle)
		}
	}
}

// TestGatewayOrgTiersSchemaRejectsUnknownTier: a typo'd tier fails the render
// instead of deploying a gateway that exits on flag parsing.
func TestGatewayOrgTiersSchemaRejectsUnknownTier(t *testing.T) {
	renderExpectSchemaError(t, "gateway.orgTiers.org-bench=platinum")
}
