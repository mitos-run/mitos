package console

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"mitos.run/mitos/internal/usage"
)

// TestUsageAPIMountedOrgScoped asserts the mounted #211 usage API at
// /console/usage/api serves the caller org's records in the canonical usage-API
// shape, derived from the gateway-verified org context.
func TestUsageAPIMountedOrgScoped(t *testing.T) {
	f := newFixture(t)
	w := f.req(t, "GET", "/console/usage/api", "", f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp usage.UsageResponse
	decode(t, w, &resp)
	if resp.OrgID != f.aliceOrg {
		t.Fatalf("org_id = %q, want alice", resp.OrgID)
	}
	if len(resp.Records) != 1 || resp.Records[0].SandboxID != "a-sb" {
		t.Fatalf("records = %+v, want exactly alice's a-sb", resp.Records)
	}
	for _, rec := range resp.Records {
		if rec.OrgID != f.aliceOrg {
			t.Fatalf("cross-org record in alice usage api: %+v", rec)
		}
	}
}

// TestUsageAPIMountedCannotReadOtherOrg asserts a request authenticated as bob
// reads ONLY bob's usage through the mounted API: org A can never read org B.
func TestUsageAPIMountedCannotReadOtherOrg(t *testing.T) {
	f := newFixture(t)
	w := f.req(t, "GET", "/console/usage/api", "", f.bobAcct, f.bobOrg)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp usage.UsageResponse
	decode(t, w, &resp)
	if resp.OrgID != f.bobOrg {
		t.Fatalf("org_id = %q, want bob", resp.OrgID)
	}
	for _, rec := range resp.Records {
		if rec.OrgID == f.aliceOrg || rec.SandboxID == "a-sb" {
			t.Fatalf("bob read alice's usage through the mounted api: %+v", rec)
		}
	}
}

// TestUsageAPIMountedIgnoresClientOrg asserts the mounted API never trusts an org
// the client tries to inject via a query parameter: the org comes only from the
// verified context.
func TestUsageAPIMountedIgnoresClientOrg(t *testing.T) {
	f := newFixture(t)
	// bob tries to read alice's org by naming it in the query; it must be ignored.
	w := f.req(t, "GET", "/console/usage/api?org="+f.aliceOrg, "", f.bobAcct, f.bobOrg)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp usage.UsageResponse
	decode(t, w, &resp)
	if resp.OrgID != f.bobOrg {
		t.Fatalf("org_id = %q, want bob (client-named org must be ignored)", resp.OrgID)
	}
}

// TestUsageAPIMountedRequiresAuth asserts the mounted API is refused without a
// caller/org context.
func TestUsageAPIMountedRequiresAuth(t *testing.T) {
	c := New(Deps{})
	r := httptest.NewRequest("GET", "/console/usage/api", nil)
	w := httptest.NewRecorder()
	c.ServeHTTP(w, r)
	if w.Code == http.StatusOK {
		t.Fatalf("unauthenticated usage api returned 200; must be refused")
	}
}
