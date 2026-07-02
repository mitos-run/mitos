package saas

import (
	"net/http"
	"testing"
)

// TestOpFromPathLiveForkMapsToCreate proves the gateway keeps the hosted flat
// SDK WORKING after the SDK repoints its fork call from POST /v1/fork to POST
// /v1/sandboxes/{id}/fork (issue #596). Without an explicit case a POST under the
// sandboxes/ prefix falls through to "sandbox.post", which the control plane
// rejects as an unknown operation, breaking hosted fork. Mapping the new path to
// sandbox.create preserves today's hosted behavior exactly: the control-plane
// create handler reads the body's template as the pool, so a hosted flat fork
// remains a template claim (a TRUE hosted live fork routed to the FromSandbox
// controller path is a documented follow-up; the cluster SDK already live-forks
// on hosted). It must NOT be misread as a runtime proxy call or a status GET.
func TestOpFromPathLiveForkMapsToCreate(t *testing.T) {
	if got := opFromPath(http.MethodPost, "/v1/sandboxes/sb-123/fork"); got != "sandbox.create" {
		t.Fatalf("POST /v1/sandboxes/{id}/fork op = %q, want sandbox.create", got)
	}
	// A GET on the same id is still a status read, not a create: the /fork suffix
	// plus POST is what selects create.
	if got := opFromPath(http.MethodGet, "/v1/sandboxes/sb-123"); got != "sandbox.status" {
		t.Fatalf("GET /v1/sandboxes/{id} op = %q, want sandbox.status", got)
	}
	// The runtime aliases are unaffected: exec/files/run_code still proxy.
	if got := opFromPath(http.MethodPost, "/v1/sandboxes/sb-123/exec"); got != opRuntime {
		t.Fatalf("POST /v1/sandboxes/{id}/exec op = %q, want opRuntime", got)
	}
	// The original cold fork route is unchanged.
	if got := opFromPath(http.MethodPost, "/v1/fork"); got != "sandbox.create" {
		t.Fatalf("POST /v1/fork op = %q, want sandbox.create", got)
	}
}
