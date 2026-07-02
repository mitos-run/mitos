package saas

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mitos.run/mitos/internal/apierr"
)

// fakeControlPlane records every ForwardRequest it receives so a test can assert
// exactly which org the gateway attached. It returns a canned response, which may
// carry a streamed body and custom headers (the runtime proxy case).
type fakeControlPlane struct {
	got        []ForwardRequest
	respBody   []byte
	respStream io.ReadCloser
	respHeader http.Header
	respCode   int
	err        error
}

func (f *fakeControlPlane) Forward(_ context.Context, req ForwardRequest) (ForwardResponse, error) {
	f.got = append(f.got, req)
	if f.err != nil {
		return ForwardResponse{}, f.err
	}
	code := f.respCode
	if code == 0 {
		code = http.StatusOK
	}
	return ForwardResponse{Status: code, Body: f.respBody, BodyStream: f.respStream, Header: f.respHeader}, nil
}

// denyQuota is a QuotaEnforcer that always denies, to exercise the quota seam.
type denyQuota struct{}

func (denyQuota) Check(_ context.Context, _, _ string) error { return errors.New("over quota") }

// gatewayFixture builds a gateway with one org and a live key for it.
type gatewayFixture struct {
	store *MemStore
	keys  *KeyService
	cp    *fakeControlPlane
	gw    *Gateway
	rawA  string
	orgA  string
}

func newGatewayFixture(t *testing.T, quota QuotaEnforcer) gatewayFixture {
	t.Helper()
	store := NewMemStore()
	newTestOrg(t, store, "org-a")
	keys := NewKeyService(store)
	created, err := keys.CreateKey(context.Background(), CreateKeyRequest{OrgID: "org-a", Scopes: []string{ScopeSandboxes, ScopeReadOnly}})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	cp := &fakeControlPlane{respBody: []byte(`{"ok":true}`)}
	gw := NewGateway(keys, quota, cp, nil)
	return gatewayFixture{store: store, keys: keys, cp: cp, gw: gw, rawA: created.RawKey, orgA: "org-a"}
}

func doRequest(gw *Gateway, method, path, bearer, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	return rec
}

func decodeErr(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Code        string `json:"code"`
			Remediation string `json:"remediation"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope from %q: %v", rec.Body.String(), err)
	}
	if env.Error.Remediation == "" {
		t.Errorf("error envelope missing remediation: %s", rec.Body.String())
	}
	return env.Error.Code
}

// TestGatewayAuthenticatesAndForwardsWithOrg is the happy path: a valid key
// authenticates, the gateway resolves the org, and forwards with the org
// attached.
func TestGatewayAuthenticatesAndForwardsWithOrg(t *testing.T) {
	f := newGatewayFixture(t, nil)
	rec := doRequest(f.gw, http.MethodPost, "/v1/sandboxes", f.rawA, `{"pool":"default"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(f.cp.got) != 1 {
		t.Fatalf("control plane saw %d requests, want 1", len(f.cp.got))
	}
	if f.cp.got[0].OrgID != "org-a" {
		t.Errorf("forwarded OrgID = %q, want org-a", f.cp.got[0].OrgID)
	}
	if f.cp.got[0].Op != "sandbox.create" {
		t.Errorf("forwarded Op = %q, want sandbox.create", f.cp.got[0].Op)
	}
}

// TestGatewayRejectsMissingKey asserts no Authorization header yields a public
// unauthorized envelope and never reaches the control plane.
func TestGatewayRejectsMissingKey(t *testing.T) {
	f := newGatewayFixture(t, nil)
	rec := doRequest(f.gw, http.MethodPost, "/v1/sandboxes", "", "{}")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if code := decodeErr(t, rec); code != "unauthorized" {
		t.Errorf("error code = %q, want unauthorized", code)
	}
	if len(f.cp.got) != 0 {
		t.Error("control plane was reached for an unauthenticated request")
	}
}

// TestGatewayRejectsForgedKey asserts a forged key yields unauthorized and does
// not reach the control plane.
func TestGatewayRejectsForgedKey(t *testing.T) {
	f := newGatewayFixture(t, nil)
	rec := doRequest(f.gw, http.MethodPost, "/v1/sandboxes", keyPrefix+"forged-aaaaaaaaaaaaaaaaaaaaaaaaaaaa", "{}")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(f.cp.got) != 0 {
		t.Error("control plane reached for a forged key")
	}
}

// TestGatewayRejectsWrongScopeAsForbidden asserts a key lacking the op's scope
// yields a forbidden envelope (valid credential, not allowed) and never
// forwards. A read-only key cannot create a sandbox.
func TestGatewayRejectsWrongScopeAsForbidden(t *testing.T) {
	store := NewMemStore()
	newTestOrg(t, store, "org-a")
	keys := NewKeyService(store)
	created, err := keys.CreateKey(context.Background(), CreateKeyRequest{OrgID: "org-a", Scopes: []string{ScopeReadOnly}})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	cp := &fakeControlPlane{}
	gw := NewGateway(keys, nil, cp, nil)
	rec := doRequest(gw, http.MethodPost, "/v1/sandboxes", created.RawKey, "{}")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if code := decodeErr(t, rec); code != "forbidden" {
		t.Errorf("error code = %q, want forbidden", code)
	}
	if len(cp.got) != 0 {
		t.Error("control plane reached despite scope refusal")
	}
}

// TestGatewayQuotaDeniedNeverForwards asserts a denied quota check yields a
// quota_exceeded envelope and never reaches the control plane.
func TestGatewayQuotaDeniedNeverForwards(t *testing.T) {
	f := newGatewayFixture(t, denyQuota{})
	rec := doRequest(f.gw, http.MethodPost, "/v1/sandboxes", f.rawA, "{}")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if code := decodeErr(t, rec); code != "quota_exceeded" {
		t.Errorf("error code = %q, want quota_exceeded", code)
	}
	if len(f.cp.got) != 0 {
		t.Error("control plane reached despite quota denial")
	}
}

// rateLimitedQuota denies with an enforcer error that carries a precise public
// envelope (rate_limited), exercising the gateway's APIError seam: when an
// enforcer supplies its own envelope, the gateway honors it instead of the
// generic quota_exceeded fallback.
type rateLimitedQuota struct{}

func (rateLimitedQuota) Check(_ context.Context, _, _ string) error {
	return preciseDenial{code: "rate_limited", status: http.StatusTooManyRequests}
}

// preciseDenial is a quota error that carries its own public apierr envelope via
// the APIError method the gateway probes for.
type preciseDenial struct {
	code   string
	status int
}

func (preciseDenial) Error() string { return "rate limited" }
func (d preciseDenial) APIError() apierr.Error {
	return apierr.Lookup(d.code)
}

// TestGatewayHonorsEnforcerEnvelope asserts the gateway emits the enforcer's own
// envelope (rate_limited here) when the enforcer supplies one, rather than
// collapsing every quota denial to quota_exceeded.
func TestGatewayHonorsEnforcerEnvelope(t *testing.T) {
	f := newGatewayFixture(t, rateLimitedQuota{})
	rec := doRequest(f.gw, http.MethodPost, "/v1/sandboxes", f.rawA, "{}")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if code := decodeErr(t, rec); code != "rate_limited" {
		t.Errorf("error code = %q, want rate_limited", code)
	}
	if len(f.cp.got) != 0 {
		t.Error("control plane reached despite rate-limit denial")
	}
}

// TestGatewayCrossOrgIsolation is the load-bearing isolation test: a key for org
// A, no matter what the request body or path claims, is ALWAYS forwarded with
// OrgID org-a. The forwarded org is taken from the verified key, never from the
// caller, so a key for org A can never address org B's resources.
func TestGatewayCrossOrgIsolation(t *testing.T) {
	store := NewMemStore()
	newTestOrg(t, store, "org-a")
	newTestOrg(t, store, "org-b")
	keys := NewKeyService(store)

	createdA, err := keys.CreateKey(context.Background(), CreateKeyRequest{OrgID: "org-a", Scopes: []string{ScopeSandboxes}})
	if err != nil {
		t.Fatalf("CreateKey A: %v", err)
	}
	createdB, err := keys.CreateKey(context.Background(), CreateKeyRequest{OrgID: "org-b", Scopes: []string{ScopeSandboxes}})
	if err != nil {
		t.Fatalf("CreateKey B: %v", err)
	}

	cp := &fakeControlPlane{respBody: []byte(`{}`)}
	gw := NewGateway(keys, nil, cp, nil)

	// Key A tries to address org B by stuffing org-b into the body. The gateway
	// must ignore that and forward org-a.
	rec := doRequest(gw, http.MethodPost, "/v1/sandboxes", createdA.RawKey, `{"org":"org-b","pool":"default"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// Key B used normally.
	rec = doRequest(gw, http.MethodPost, "/v1/sandboxes", createdB.RawKey, `{"pool":"default"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	if len(cp.got) != 2 {
		t.Fatalf("control plane saw %d requests, want 2", len(cp.got))
	}
	if cp.got[0].OrgID != "org-a" {
		t.Errorf("key A request forwarded with OrgID %q, want org-a (cross-org leak)", cp.got[0].OrgID)
	}
	if cp.got[1].OrgID != "org-b" {
		t.Errorf("key B request forwarded with OrgID %q, want org-b", cp.got[1].OrgID)
	}
}

// TestGatewayRoutesRuntimeConnectPath asserts a Connect runtime path is routed as
// the sandbox.runtime op, the request body is handed across the seam UNBUFFERED
// (BodyStream, not Body), the curated headers (X-Sandbox-Id, Content-Type) cross,
// and the client Authorization is NOT placed in the forwarded header.
func TestGatewayRoutesRuntimeConnectPath(t *testing.T) {
	f := newGatewayFixture(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/sandbox.v1.Sandbox/Exec", strings.NewReader(`{"cmd":["true"]}`))
	req.Header.Set("Authorization", "Bearer "+f.rawA)
	req.Header.Set("X-Sandbox-Id", "sb-1")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.gw.ServeHTTP(rec, req)

	if len(f.cp.got) != 1 {
		t.Fatalf("control plane saw %d requests, want 1", len(f.cp.got))
	}
	g := f.cp.got[0]
	if g.Op != "sandbox.runtime" {
		t.Errorf("op = %q, want sandbox.runtime", g.Op)
	}
	if g.OrgID != "org-a" {
		t.Errorf("orgID = %q, want org-a", g.OrgID)
	}
	if g.BodyStream == nil {
		t.Error("runtime request body was buffered into Body, want an unbuffered BodyStream")
	} else {
		b, _ := io.ReadAll(g.BodyStream)
		if string(b) != `{"cmd":["true"]}` {
			t.Errorf("streamed body = %q", b)
		}
	}
	if g.Header.Get("X-Sandbox-Id") != "sb-1" {
		t.Errorf("forwarded X-Sandbox-Id = %q", g.Header.Get("X-Sandbox-Id"))
	}
	if g.Header.Get("Authorization") != "" {
		t.Errorf("client Authorization was forwarded across the seam: %q (it must be stripped)", g.Header.Get("Authorization"))
	}
}

// TestGatewayRoutesRuntimeAliasPath asserts the /v1/sandboxes/<id>/exec friendly
// alias also routes as the runtime op.
func TestGatewayRoutesRuntimeAliasPath(t *testing.T) {
	f := newGatewayFixture(t, nil)
	rec := doRequest(f.gw, http.MethodPost, "/v1/sandboxes/sb-7/exec", f.rawA, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(f.cp.got) != 1 || f.cp.got[0].Op != "sandbox.runtime" {
		t.Errorf("op = %+v, want sandbox.runtime", f.cp.got)
	}
}

// TestGatewayStreamsRuntimeResponse asserts a control-plane response carrying a
// BodyStream is streamed to the caller with the control-plane Content-Type.
func TestGatewayStreamsRuntimeResponse(t *testing.T) {
	f := newGatewayFixture(t, nil)
	f.cp.respBody = nil
	f.cp.respStream = io.NopCloser(strings.NewReader(`{"streamed":true}`))
	f.cp.respHeader = http.Header{"Content-Type": []string{"application/connect+json"}}
	rec := doRequest(f.gw, http.MethodPost, "/sandbox.v1.Sandbox/Exec", f.rawA, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Body.String() != `{"streamed":true}` {
		t.Errorf("streamed response = %q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/connect+json" {
		t.Errorf("Content-Type = %q, want the control-plane value", ct)
	}
}

// TestGatewayTemplateEnsureRoutesAndForwards asserts POST /v1/templates is routed
// as the "template.ensure" op, the org is correctly attached from the verified key
// (not from the request body), and the control plane is reached exactly once.
func TestGatewayTemplateEnsureRoutesAndForwards(t *testing.T) {
	f := newGatewayFixture(t, nil)
	f.cp.respBody = []byte(`{"id":"python","ready":true}`)
	rec := doRequest(f.gw, http.MethodPost, "/v1/templates", f.rawA, `{"id":"python"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(f.cp.got) != 1 {
		t.Fatalf("control plane saw %d requests, want 1", len(f.cp.got))
	}
	if f.cp.got[0].Op != "template.ensure" {
		t.Errorf("op = %q, want template.ensure", f.cp.got[0].Op)
	}
	if f.cp.got[0].OrgID != "org-a" {
		t.Errorf("OrgID = %q, want org-a (must come from the verified key, not the body)", f.cp.got[0].OrgID)
	}
}

// TestGatewayTemplateEnsureRequiresAuth asserts POST /v1/templates without a
// bearer key is rejected 401 and never reaches the control plane.
func TestGatewayTemplateEnsureRequiresAuth(t *testing.T) {
	f := newGatewayFixture(t, nil)
	rec := doRequest(f.gw, http.MethodPost, "/v1/templates", "", `{"id":"python"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if code := decodeErr(t, rec); code != "unauthorized" {
		t.Errorf("error code = %q, want unauthorized", code)
	}
	if len(f.cp.got) != 0 {
		t.Error("control plane reached despite missing auth")
	}
}

// TestGatewayTemplateListRoutesAsReadOp asserts GET /v1/templates routes as
// "template.list" and accepts a read-only key (it is a read op, not a mutating
// one, so the sandbox scope is not required).
func TestGatewayTemplateListRoutesAsReadOp(t *testing.T) {
	store := NewMemStore()
	newTestOrg(t, store, "org-a")
	keys := NewKeyService(store)
	created, err := keys.CreateKey(context.Background(), CreateKeyRequest{OrgID: "org-a", Scopes: []string{ScopeReadOnly}})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	cp := &fakeControlPlane{respBody: []byte(`[]`)}
	gw := NewGateway(keys, nil, cp, nil)
	rec := doRequest(gw, http.MethodGet, "/v1/templates", created.RawKey, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(cp.got) != 1 || cp.got[0].Op != "template.list" {
		t.Errorf("forwarded op = %+v, want template.list", cp.got)
	}
}

// TestGatewayReadScopeAllowsList asserts a read-only key can list (a read op) but
// the op is correctly classified so the read scope suffices.
func TestGatewayReadScopeAllowsList(t *testing.T) {
	store := NewMemStore()
	newTestOrg(t, store, "org-a")
	keys := NewKeyService(store)
	created, err := keys.CreateKey(context.Background(), CreateKeyRequest{OrgID: "org-a", Scopes: []string{ScopeReadOnly}})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	cp := &fakeControlPlane{respBody: []byte(`[]`)}
	gw := NewGateway(keys, nil, cp, nil)
	rec := doRequest(gw, http.MethodGet, "/v1/sandboxes", created.RawKey, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(cp.got) != 1 || cp.got[0].Op != "sandbox.list" {
		t.Errorf("forwarded op = %+v, want sandbox.list", cp.got)
	}
}

// TestGatewayForkMapsToSandboxCreate asserts POST /v1/fork is routed as the
// "sandbox.create" op: the SDK fork call and the /v1/sandboxes POST both create a
// sandbox and must reach the same control-plane handler. The org is taken from the
// verified key, never from the request body.
func TestGatewayForkMapsToSandboxCreate(t *testing.T) {
	f := newGatewayFixture(t, nil)
	f.cp.respBody = []byte(`{"id":"sb-x","endpoint":"10.0.0.1:9091","token":"tok","phase":"Ready","template_id":"python","fork_time_ms":0}`)
	f.cp.respCode = http.StatusCreated
	rec := doRequest(f.gw, http.MethodPost, "/v1/fork", f.rawA, `{"template":"python","id":"sb-x"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(f.cp.got) != 1 {
		t.Fatalf("control plane saw %d requests, want 1", len(f.cp.got))
	}
	if f.cp.got[0].Op != "sandbox.create" {
		t.Errorf("op = %q, want sandbox.create (POST /v1/fork must map to sandbox.create)", f.cp.got[0].Op)
	}
	if f.cp.got[0].OrgID != "org-a" {
		t.Errorf("OrgID = %q, want org-a (must come from the verified key, not the body)", f.cp.got[0].OrgID)
	}
}

// TestGatewayForkRequiresAuth asserts POST /v1/fork without a bearer key returns
// 401 and never reaches the control plane, matching the behavior of every other
// authenticated op.
func TestGatewayForkRequiresAuth(t *testing.T) {
	f := newGatewayFixture(t, nil)
	rec := doRequest(f.gw, http.MethodPost, "/v1/fork", "", `{"template":"python","id":"sb-x"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if code := decodeErr(t, rec); code != "unauthorized" {
		t.Errorf("error code = %q, want unauthorized", code)
	}
	if len(f.cp.got) != 0 {
		t.Error("control plane reached despite missing auth on /v1/fork")
	}
}

// TestGatewayExistingSandboxesPathStillWorks is a regression guard: /v1/sandboxes
// POST must still route as sandbox.create after the /v1/fork case is added.
func TestGatewayExistingSandboxesPathStillWorks(t *testing.T) {
	f := newGatewayFixture(t, nil)
	rec := doRequest(f.gw, http.MethodPost, "/v1/sandboxes", f.rawA, `{"pool":"default"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(f.cp.got) != 1 || f.cp.got[0].Op != "sandbox.create" {
		t.Errorf("op = %+v, want sandbox.create", f.cp.got)
	}
}

// TestGatewayPauseMapsToSandboxPause asserts POST /v1/pause (the SDK
// sandbox.pause() call, body {"sandbox": "<id>"}) routes as the "sandbox.pause"
// op with the org taken from the verified key. Before #601 it fell through to
// "sandbox.post", which the control plane rejects as an unknown operation.
func TestGatewayPauseMapsToSandboxPause(t *testing.T) {
	f := newGatewayFixture(t, nil)
	f.cp.respBody = []byte(`{"status":"paused"}`)
	rec := doRequest(f.gw, http.MethodPost, "/v1/pause", f.rawA, `{"sandbox":"sb-x"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(f.cp.got) != 1 {
		t.Fatalf("control plane saw %d requests, want 1", len(f.cp.got))
	}
	if f.cp.got[0].Op != "sandbox.pause" {
		t.Errorf("op = %q, want sandbox.pause", f.cp.got[0].Op)
	}
	if f.cp.got[0].OrgID != "org-a" {
		t.Errorf("OrgID = %q, want org-a (must come from the verified key)", f.cp.got[0].OrgID)
	}
}

// TestGatewayResumeMapsToSandboxResume asserts POST /v1/resume routes as the
// "sandbox.resume" op with the org taken from the verified key.
func TestGatewayResumeMapsToSandboxResume(t *testing.T) {
	f := newGatewayFixture(t, nil)
	f.cp.respBody = []byte(`{"status":"running"}`)
	rec := doRequest(f.gw, http.MethodPost, "/v1/resume", f.rawA, `{"sandbox":"sb-x"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(f.cp.got) != 1 {
		t.Fatalf("control plane saw %d requests, want 1", len(f.cp.got))
	}
	if f.cp.got[0].Op != "sandbox.resume" {
		t.Errorf("op = %q, want sandbox.resume", f.cp.got[0].Op)
	}
	if f.cp.got[0].OrgID != "org-a" {
		t.Errorf("OrgID = %q, want org-a (must come from the verified key)", f.cp.got[0].OrgID)
	}
}

// TestGatewayPauseResumeRequireSandboxScope asserts pause and resume are
// MUTATING ops: a read-only key is rejected with forbidden and never reaches
// the control plane.
func TestGatewayPauseResumeRequireSandboxScope(t *testing.T) {
	store := NewMemStore()
	newTestOrg(t, store, "org-a")
	keys := NewKeyService(store)
	created, err := keys.CreateKey(context.Background(), CreateKeyRequest{OrgID: "org-a", Scopes: []string{ScopeReadOnly}})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	cp := &fakeControlPlane{respBody: []byte(`{}`)}
	gw := NewGateway(keys, nil, cp, nil)
	for _, path := range []string{"/v1/pause", "/v1/resume"} {
		rec := doRequest(gw, http.MethodPost, path, created.RawKey, `{"sandbox":"sb-x"}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403 for a read-only key", path, rec.Code)
		}
		if code := decodeErr(t, rec); code != "forbidden" {
			t.Errorf("%s: error code = %q, want forbidden", path, code)
		}
	}
	if len(cp.got) != 0 {
		t.Error("control plane reached despite a read-only key on pause/resume")
	}
}

// TestGatewayTemplatesPathStillWorks is a regression guard: the /v1/templates POST
// path added in #520 must still route as template.ensure after the /v1/fork case
// is added.
func TestGatewayTemplatesPathStillWorks(t *testing.T) {
	f := newGatewayFixture(t, nil)
	f.cp.respBody = []byte(`{"id":"python","ready":true}`)
	rec := doRequest(f.gw, http.MethodPost, "/v1/templates", f.rawA, `{"id":"python"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(f.cp.got) != 1 || f.cp.got[0].Op != "template.ensure" {
		t.Errorf("op = %+v, want template.ensure", f.cp.got)
	}
}
