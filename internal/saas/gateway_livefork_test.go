package saas

import (
	"net/http"
	"testing"
)

// TestOpFromPathPerSandboxForkMapsToSandboxFork proves the hosted gateway routes
// the flat SDK's per-sandbox fork (POST /v1/sandboxes/{id}/fork,
// DirectSandbox._fork_one, issues #596 and #709) to the dedicated sandbox.fork
// op, which the control plane serves as a TRUE live fork through the
// Source.FromSandbox controller path (#611). It must NOT be misread as a runtime
// proxy call, a status GET, or the old sandbox.create template claim.
func TestOpFromPathPerSandboxForkMapsToSandboxFork(t *testing.T) {
	if got := opFromPath(http.MethodPost, "/v1/sandboxes/sb-123/fork"); got != "sandbox.fork" {
		t.Fatalf("POST /v1/sandboxes/{id}/fork op = %q, want sandbox.fork", got)
	}
	// A GET on the same id is still a status read, not a fork: the /fork suffix
	// plus POST is what selects the fork op.
	if got := opFromPath(http.MethodGet, "/v1/sandboxes/sb-123"); got != "sandbox.status" {
		t.Fatalf("GET /v1/sandboxes/{id} op = %q, want sandbox.status", got)
	}
	// The runtime aliases are unaffected: exec/files/run_code still proxy.
	if got := opFromPath(http.MethodPost, "/v1/sandboxes/sb-123/exec"); got != opRuntime {
		t.Fatalf("POST /v1/sandboxes/{id}/exec op = %q, want opRuntime", got)
	}
	// The flat POST /v1/fork has NO source sandbox in its path and MUST stay a
	// template claim (sandbox.create): only the per-sandbox route live-forks.
	if got := opFromPath(http.MethodPost, "/v1/fork"); got != "sandbox.create" {
		t.Fatalf("POST /v1/fork op = %q, want sandbox.create (template claim, unchanged)", got)
	}
	// A fork is a mutating lifecycle verb: it must require the sandboxes scope,
	// never be satisfied by a read-only key.
	if got := requiredScopeFor("sandbox.fork"); got != ScopeSandboxes {
		t.Fatalf("requiredScopeFor(sandbox.fork) = %q, want %q", got, ScopeSandboxes)
	}
}

// TestGatewayPerSandboxForkForwardsAsSandboxFork asserts the gateway forwards
// POST /v1/sandboxes/{id}/fork as the sandbox.fork op with the path preserved
// (the control plane resolves the org-owned source from it) and the OrgID taken
// SOLELY from the verified key, never from the body or path.
func TestGatewayPerSandboxForkForwardsAsSandboxFork(t *testing.T) {
	f := newGatewayFixture(t, nil)
	f.cp.respBody = []byte(`{"id":"sb-child","endpoint":"10.0.0.9:9091","token":"tok","phase":"Ready","template_id":"python","fork_time_ms":12.0}`)
	f.cp.respCode = http.StatusCreated
	rec := doRequest(f.gw, http.MethodPost, "/v1/sandboxes/sb-src/fork", f.rawA, `{"id":"child","template":"python","pause_source":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(f.cp.got) != 1 {
		t.Fatalf("control plane saw %d requests, want 1", len(f.cp.got))
	}
	if f.cp.got[0].Op != "sandbox.fork" {
		t.Errorf("op = %q, want sandbox.fork (per-sandbox fork must route to the live fork op)", f.cp.got[0].Op)
	}
	if f.cp.got[0].Path != "/v1/sandboxes/sb-src/fork" {
		t.Errorf("path = %q, want /v1/sandboxes/sb-src/fork (the control plane resolves the source from it)", f.cp.got[0].Path)
	}
	if f.cp.got[0].OrgID != "org-a" {
		t.Errorf("OrgID = %q, want org-a (must come from the verified key, not the request)", f.cp.got[0].OrgID)
	}
}
