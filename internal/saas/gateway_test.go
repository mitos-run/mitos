package saas

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mitos.run/mitos/internal/apierr"
)

// fakeControlPlane records every ForwardRequest it receives so a test can assert
// exactly which org the gateway attached. It returns a canned response.
type fakeControlPlane struct {
	got      []ForwardRequest
	respBody []byte
	respCode int
	err      error
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
	return ForwardResponse{Status: code, Body: f.respBody}, nil
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
