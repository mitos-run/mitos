package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/quota"
)

// recordingControlPlane records every forwarded request so a test can assert the
// gateway only reached it when the enforcer allowed.
type recordingControlPlane struct{ got []saas.ForwardRequest }

func (c *recordingControlPlane) Forward(_ context.Context, req saas.ForwardRequest) (saas.ForwardResponse, error) {
	c.got = append(c.got, req)
	return saas.ForwardResponse{Status: http.StatusOK, Body: []byte(`{"ok":true}`)}, nil
}

// enforceFixture builds a gateway wired to the REAL quota enforcer plus a key for
// one org, and returns the kill-switch and suspension store so a test can drive
// the abuse and billing suspend paths. It mirrors the binary's buildQuotaEnforcer
// construction but lets the test seed the suspension store directly.
type enforceFixture struct {
	gw     *saas.Gateway
	cp     *recordingControlPlane
	raw    string
	orgID  string
	wiring quotaWiring
}

func newEnforceFixture(t *testing.T) enforceFixture {
	t.Helper()
	store := saas.NewMemStore()
	if err := store.PutOrg(context.Background(), saas.Organization{ID: "org-1", Name: "Org One"}); err != nil {
		t.Fatalf("PutOrg: %v", err)
	}
	keys := saas.NewKeyService(store)
	created, err := keys.CreateKey(context.Background(), saas.CreateKeyRequest{OrgID: "org-1", Scopes: []string{saas.ScopeSandboxes, saas.ScopeReadOnly}})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	wiring := buildQuotaEnforcer(enforcementConfig{enabled: true})
	cp := &recordingControlPlane{}
	gw := saas.NewGateway(keys, wiring.enforcer, cp, nil)
	return enforceFixture{gw: gw, cp: cp, raw: created.RawKey, orgID: "org-1", wiring: wiring}
}

func do(t *testing.T, gw *saas.Gateway, method, path, bearer, body, remoteAddr string, xff string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	return rec
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
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
		t.Errorf("error envelope missing remediation (LLM-legible rule): %s", rec.Body.String())
	}
	return env.Error.Code
}

// TestEnforceWithinQuotaPasses asserts an org within quota is forwarded to the
// control plane.
func TestEnforceWithinQuotaPasses(t *testing.T) {
	f := newEnforceFixture(t)
	rec := do(t, f.gw, http.MethodGet, "/v1/sandboxes", f.raw, "", "203.0.113.5:1234", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("within-quota status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(f.cp.got) != 1 {
		t.Fatalf("control plane saw %d requests, want 1", len(f.cp.got))
	}
}

// TestEnforceSuspendedReturns403 asserts a suspended org is blocked with a 403
// forbidden envelope and never reaches the control plane (the kill-switch path).
func TestEnforceSuspendedReturns403(t *testing.T) {
	f := newEnforceFixture(t)
	if err := f.wiring.killSwitch.Suspend(context.Background(), "org-1", quota.ReasonAbuseSignal, "egress spike", true); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	rec := do(t, f.gw, http.MethodGet, "/v1/sandboxes", f.raw, "", "203.0.113.5:1234", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("suspended status = %d, want 403; body = %s", rec.Code, rec.Body.String())
	}
	if code := errCode(t, rec); code != "forbidden" {
		t.Errorf("suspended error code = %q, want forbidden", code)
	}
	if len(f.cp.got) != 0 {
		t.Error("control plane reached for a suspended org")
	}
}

// TestEnforceRateLimitReturns429 asserts that draining the per-org request-rate
// bucket yields a 429. The free tier allows 60 requests/min; the 61st within the
// same instant is rate-limited. This exercises the over-quota/over-rate path
// end to end through the gateway with the real enforcer.
func TestEnforceRateLimitReturns429(t *testing.T) {
	f := newEnforceFixture(t)
	var rec *httptest.ResponseRecorder
	for i := 0; i < 200; i++ {
		rec = do(t, f.gw, http.MethodGet, "/v1/sandboxes", f.raw, "", "203.0.113.5:1234", "")
		if rec.Code != http.StatusOK {
			break
		}
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after draining the bucket status = %d, want 429", rec.Code)
	}
	if code := errCode(t, rec); code != "rate_limited" {
		t.Errorf("rate-limited error code = %q, want rate_limited", code)
	}
}

// TestEnforceKillSwitchViaBillingSuspender asserts the billing suspender (the
// past-due / spend-cap driver) writes to the SAME store the enforcer reads, so a
// billing suspension blocks the org at the gateway.
func TestEnforceKillSwitchViaBillingSuspender(t *testing.T) {
	f := newEnforceFixture(t)
	// The billing path drives the kill-switch via the billing.Suspender seam.
	if err := f.wiring.billingSuspender.Suspend(context.Background(), "org-1", "dunning", "payment retries exhausted", true); err != nil {
		t.Fatalf("billing suspend: %v", err)
	}
	rec := do(t, f.gw, http.MethodGet, "/v1/sandboxes", f.raw, "", "203.0.113.5:1234", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("billing-suspended status = %d, want 403", rec.Code)
	}
	if len(f.cp.got) != 0 {
		t.Error("control plane reached for a billing-suspended org")
	}
}

// TestEnforceKillSwitchViaAbuseSignal asserts the automated abuse-signal path
// (ProcessSignals) suspends an org through the kill-switch and the gateway then
// blocks it. This proves abuse signal -> suspend -> block end to end in process.
func TestEnforceKillSwitchViaAbuseSignal(t *testing.T) {
	f := newEnforceFixture(t)
	sig := staticAbuseSignal{orgs: map[string]string{"org-1": "crypto-mining egress fingerprint"}}
	suspended, err := f.wiring.killSwitch.ProcessSignals(context.Background(), sig)
	if err != nil || len(suspended) != 1 || suspended[0] != "org-1" {
		t.Fatalf("ProcessSignals = %v, %v; want [org-1], nil", suspended, err)
	}
	rec := do(t, f.gw, http.MethodGet, "/v1/sandboxes", f.raw, "", "203.0.113.5:1234", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("abuse-suspended status = %d, want 403", rec.Code)
	}
}

// staticAbuseSignal flags a fixed set of orgs for the abuse-signal test.
type staticAbuseSignal struct{ orgs map[string]string }

func (s staticAbuseSignal) FiredOrgs(_ context.Context) (map[string]string, error) {
	return s.orgs, nil
}

// TestEnforceXFFDoesNotBypassPerIPLimit asserts an attacker rotating the
// X-Forwarded-For header cannot dodge the per-IP rate limit when the gateway does
// NOT trust XFF (zero trusted hops, the default): every request shares the same
// RemoteAddr-derived per-IP bucket, so the limit still fires. This matches the
// gateway's existing trust model (RemoteAddr is the trusted source).
func TestEnforceXFFDoesNotBypassPerIPLimit(t *testing.T) {
	f := newEnforceFixture(t)
	// Every request comes from the same RemoteAddr but rotates a forged XFF. With
	// zero trusted hops the per-IP bucket keys on RemoteAddr, so the spoof is inert
	// and the per-org-or-per-IP rate limit still trips.
	var rec *httptest.ResponseRecorder
	for i := 0; i < 200; i++ {
		forged := "10.99.99." + string(rune('0'+i%10))
		rec = do(t, f.gw, http.MethodGet, "/v1/sandboxes", f.raw, "", "203.0.113.9:2222", forged)
		if rec.Code != http.StatusOK {
			break
		}
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("rotating a forged X-Forwarded-For bypassed the rate limit: final status = %d, want 429", rec.Code)
	}
}

// TestBuildQuotaEnforcerDisabledFallback asserts that with enforcement disabled
// the wiring is the permissive AllowAllQuota and carries no kill-switch, and that
// the mode names the bypass for the startup log.
func TestBuildQuotaEnforcerDisabledFallback(t *testing.T) {
	w := buildQuotaEnforcer(enforcementConfig{enabled: false})
	if _, ok := w.enforcer.(saas.AllowAllQuota); !ok {
		t.Fatalf("disabled enforcer = %T, want saas.AllowAllQuota", w.enforcer)
	}
	if w.killSwitch != nil || w.billingSuspender != nil || w.suspensions != nil {
		t.Error("disabled wiring should carry no kill-switch, suspender, or store")
	}
	if !strings.Contains(strings.ToUpper(w.mode), "DISABLED") {
		t.Errorf("disabled mode = %q, want it to name the bypass", w.mode)
	}
	// A disabled enforcer allows every request.
	if err := w.enforcer.Check(context.Background(), "org-x", "sandbox.create"); err != nil {
		t.Errorf("disabled enforcer denied a request: %v", err)
	}
}

// TestBuildQuotaEnforcerEnabledRealEnforcer asserts that with enforcement enabled
// the wiring builds the real adapter and a kill-switch whose store the enforcer
// reads, so a suspension through the kill-switch is observed by a subsequent
// enforcer Check (the store is shared).
func TestBuildQuotaEnforcerEnabledRealEnforcer(t *testing.T) {
	w := buildQuotaEnforcer(enforcementConfig{enabled: true})
	if _, ok := w.enforcer.(quota.GatewayAdapter); !ok {
		t.Fatalf("enabled enforcer = %T, want quota.GatewayAdapter", w.enforcer)
	}
	if w.killSwitch == nil || w.billingSuspender == nil || w.suspensions == nil {
		t.Fatal("enabled wiring must carry the kill-switch, billing suspender, and shared store")
	}
	// Within quota: a status read is allowed.
	if err := w.enforcer.Check(context.Background(), "org-1", "sandbox.status"); err != nil {
		t.Fatalf("within-quota check denied: %v", err)
	}
	// Suspend via the kill-switch; the SAME store backs the enforcer, so the next
	// check is denied.
	if err := w.killSwitch.Suspend(context.Background(), "org-1", quota.ReasonManual, "test", true); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if err := w.enforcer.Check(context.Background(), "org-1", "sandbox.status"); err == nil {
		t.Fatal("after kill-switch suspend, enforcer must deny (shared store), got allow")
	}
}
