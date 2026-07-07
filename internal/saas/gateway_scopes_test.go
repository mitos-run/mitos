package saas

import (
	"context"
	"net/http"
	"testing"
)

// scopedGateway builds a gateway with one org and a key carrying exactly the
// given scopes, and returns the raw key.
func scopedGateway(t *testing.T, scopes []string) (*Gateway, *fakeControlPlane, string) {
	t.Helper()
	store := NewMemStore()
	newTestOrg(t, store, "org-a")
	keys := NewKeyService(store)
	created, err := keys.CreateKey(context.Background(), CreateKeyRequest{OrgID: "org-a", Scopes: scopes})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	cp := &fakeControlPlane{respBody: []byte(`{"ok":true}`)}
	return NewGateway(keys, nil, cp, nil), cp, created.RawKey
}

// TestGatewayRequiredScopeMapping pins the op -> required scope table that the
// gateway authz path enforces. Read ops need read, runtime (exec/files/run_code)
// needs execute, and every mutating lifecycle verb needs lifecycle. An unmapped
// op fails closed to lifecycle (the most privileged resource scope).
func TestGatewayRequiredScopeMapping(t *testing.T) {
	cases := map[string]string{
		"sandbox.list":      ScopeReadOnly,
		"sandbox.status":    ScopeReadOnly,
		"template.list":     ScopeReadOnly,
		opRuntime:           ScopeExecute,
		"sandbox.create":    ScopeLifecycle,
		"sandbox.fork":      ScopeLifecycle,
		"sandbox.terminate": ScopeLifecycle,
		"template.ensure":   ScopeLifecycle,
		"sandbox.pause":     ScopeLifecycle,
		"sandbox.resume":    ScopeLifecycle,
		"sandbox.unknown":   ScopeLifecycle, // fail closed
	}
	for op, want := range cases {
		if got := requiredScopeFor(op); got != want {
			t.Errorf("requiredScopeFor(%q) = %q, want %q", op, got, want)
		}
	}
}

// TestGatewayExecuteScopeCanExecNotCreate: the execute-scoped key (the CI-safe or
// browser-safe key) proxies a runtime exec call but is refused a create with a
// forbidden envelope, and the control plane is never reached on the refusal.
func TestGatewayExecuteScopeCanExecNotCreate(t *testing.T) {
	gw, cp, raw := scopedGateway(t, []string{ScopeExecute})

	// Runtime exec is allowed (execute scope).
	rec := doRequest(gw, http.MethodPost, "/v1/sandboxes/sb-1/exec", raw, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("exec status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(cp.got) != 1 || cp.got[0].Op != opRuntime {
		t.Fatalf("exec forwarded = %+v, want opRuntime", cp.got)
	}

	// Create is refused: it needs lifecycle.
	rec = doRequest(gw, http.MethodPost, "/v1/sandboxes", raw, `{"pool":"default"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("create status = %d, want 403", rec.Code)
	}
	if code := decodeErr(t, rec); code != "forbidden" {
		t.Errorf("create error code = %q, want forbidden", code)
	}
	if len(cp.got) != 1 {
		t.Error("control plane reached despite the scope refusal")
	}
}

// TestGatewayLifecycleScopeCanCreateNotExec: the lifecycle-scoped key creates,
// forks, and terminates sandboxes and may list them (lifecycle implies read),
// but is refused a runtime exec (which needs execute). This is the create-only
// CI key with no in-sandbox code-execution reach.
func TestGatewayLifecycleScopeCanCreateNotExec(t *testing.T) {
	gw, _, raw := scopedGateway(t, []string{ScopeLifecycle})

	rec := doRequest(gw, http.MethodPost, "/v1/sandboxes", raw, `{"pool":"default"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// Listing is allowed (lifecycle implies read, no dead end).
	rec = doRequest(gw, http.MethodGet, "/v1/sandboxes", raw, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// Runtime exec is refused: it needs execute.
	rec = doRequest(gw, http.MethodPost, "/v1/sandboxes/sb-1/exec", raw, `{}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("exec status = %d, want 403", rec.Code)
	}
	if code := decodeErr(t, rec); code != "forbidden" {
		t.Errorf("exec error code = %q, want forbidden", code)
	}
}

// TestGatewayReadScopeIsDeniedEveryMutation: a read-only key (the classic
// browser-safe key) may list and status but is refused create, fork, terminate,
// and exec, each with a forbidden envelope. This is the smallest blast radius.
func TestGatewayReadScopeIsDeniedEveryMutation(t *testing.T) {
	gw, cp, raw := scopedGateway(t, []string{ScopeReadOnly})
	muts := []struct {
		method, path, body string
	}{
		{http.MethodPost, "/v1/sandboxes", `{"pool":"default"}`},
		{http.MethodPost, "/v1/sandboxes/sb-1/fork", `{}`},
		{http.MethodDelete, "/v1/sandboxes/sb-1", ""},
		{http.MethodPost, "/v1/sandboxes/sb-1/exec", `{}`},
	}
	for _, m := range muts {
		rec := doRequest(gw, m.method, m.path, raw, m.body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: status = %d, want 403", m.method, m.path, rec.Code)
		}
	}
	if len(cp.got) != 0 {
		t.Errorf("control plane reached %d times despite read-only key on mutations", len(cp.got))
	}
	// The read op it does carry still works.
	if rec := doRequest(gw, http.MethodGet, "/v1/sandboxes", raw, ""); rec.Code != http.StatusOK {
		t.Errorf("read-only key list status = %d, want 200", rec.Code)
	}
}

// TestGatewayLegacyScopelessKeyRetainsFullAccess is the gateway-level
// backward-compatibility proof: a key persisted with NO scopes (a pre-scopes
// record) still reaches every op through the gateway exactly as before.
func TestGatewayLegacyScopelessKeyRetainsFullAccess(t *testing.T) {
	store := NewMemStore()
	newTestOrg(t, store, "org-a")
	keys := NewKeyService(store)
	// Mint a normal key, then rewrite the stored record to carry NO scopes,
	// simulating a legacy key persisted before the scope model existed.
	created, err := keys.CreateKey(context.Background(), CreateKeyRequest{OrgID: "org-a", Scopes: []string{ScopeReadOnly}})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	legacy := created.Record
	legacy.Scopes = nil
	store.keys[legacy.ID] = legacy // in-package test can reach the map directly.

	cp := &fakeControlPlane{respBody: []byte(`{"ok":true}`)}
	gw := NewGateway(keys, nil, cp, nil)
	// Every op class must succeed for the scopeless legacy key.
	ops := []struct{ method, path, body string }{
		{http.MethodGet, "/v1/sandboxes", ""},
		{http.MethodPost, "/v1/sandboxes", `{"pool":"default"}`},
		{http.MethodPost, "/v1/sandboxes/sb-1/exec", `{}`},
		{http.MethodPost, "/v1/sandboxes/sb-1/fork", `{}`},
		{http.MethodDelete, "/v1/sandboxes/sb-1", ""},
	}
	for _, o := range ops {
		rec := doRequest(gw, o.method, o.path, created.RawKey, o.body)
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Errorf("legacy scopeless key %s %s: status = %d, body = %s", o.method, o.path, rec.Code, rec.Body.String())
		}
	}
}
