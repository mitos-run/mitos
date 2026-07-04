package main

import (
	"context"
	"encoding/json"
	"errors"
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
	return newEnforceFixtureLive(t, nil)
}

// newEnforceFixtureLive is newEnforceFixture with an injected live-usage
// source, mirroring main's wiring of the cluster-backed live counter.
func newEnforceFixtureLive(t *testing.T, live quota.LiveUsageSource) enforceFixture {
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
	wiring := buildQuotaEnforcer(enforcementConfig{enabled: true, live: live})
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

// TestEnforceConcurrencyCapDeniesAtCap asserts an org already AT its tier's
// concurrency cap (free tier: 2) is denied a create with the typed
// quota_exceeded envelope and never reaches the control plane, while the same
// org one below the cap is admitted. This is the issue #615 seam-2 behavior:
// the cap is enforced at the gateway from the live count, nowhere else.
func TestEnforceConcurrencyCapDeniesAtCap(t *testing.T) {
	free := quota.DefaultTiers()[quota.TierFree]
	atCap := quota.LiveUsageFunc(func(_ context.Context, _ string) (quota.LiveUsage, error) {
		return quota.LiveUsage{ConcurrentSandboxes: free.MaxConcurrentSandboxes}, nil
	})
	f := newEnforceFixtureLive(t, atCap)
	rec := do(t, f.gw, http.MethodPost, "/v1/sandboxes", f.raw, `{"pool":"default"}`, "203.0.113.5:1234", "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("at-cap create status = %d, want 429; body = %s", rec.Code, rec.Body.String())
	}
	if code := errCode(t, rec); code != "quota_exceeded" {
		t.Errorf("at-cap error code = %q, want quota_exceeded", code)
	}
	if len(f.cp.got) != 0 {
		t.Error("control plane reached for an at-cap create")
	}

	belowCap := quota.LiveUsageFunc(func(_ context.Context, _ string) (quota.LiveUsage, error) {
		return quota.LiveUsage{ConcurrentSandboxes: free.MaxConcurrentSandboxes - 1}, nil
	})
	f2 := newEnforceFixtureLive(t, belowCap)
	rec2 := do(t, f2.gw, http.MethodPost, "/v1/sandboxes", f2.raw, `{"pool":"default"}`, "203.0.113.5:1234", "")
	if rec2.Code != http.StatusOK {
		t.Fatalf("below-cap create status = %d, want 200; body = %s", rec2.Code, rec2.Body.String())
	}
	if len(f2.cp.got) != 1 {
		t.Fatalf("control plane saw %d requests for a below-cap create, want 1", len(f2.cp.got))
	}
}

// TestEnforceConcurrencyCapDoesNotBlockReads asserts the live cap only gates
// creates: an at-cap org can still list and read its sandboxes (no dead end;
// the caller can see and terminate what is running to get back under the cap).
func TestEnforceConcurrencyCapDoesNotBlockReads(t *testing.T) {
	free := quota.DefaultTiers()[quota.TierFree]
	atCap := quota.LiveUsageFunc(func(_ context.Context, _ string) (quota.LiveUsage, error) {
		return quota.LiveUsage{ConcurrentSandboxes: free.MaxConcurrentSandboxes}, nil
	})
	f := newEnforceFixtureLive(t, atCap)
	rec := do(t, f.gw, http.MethodGet, "/v1/sandboxes", f.raw, "", "203.0.113.5:1234", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("at-cap list status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

// TestEnforceLiveErrorDeniesCreate asserts a live-usage read failure DENIES a
// create (fail closed): an unreachable cluster must never read as "zero live
// sandboxes" on the anti-abuse path.
func TestEnforceLiveErrorDeniesCreate(t *testing.T) {
	broken := quota.LiveUsageFunc(func(_ context.Context, _ string) (quota.LiveUsage, error) {
		return quota.LiveUsage{}, errors.New("apiserver unavailable")
	})
	f := newEnforceFixtureLive(t, broken)
	rec := do(t, f.gw, http.MethodPost, "/v1/sandboxes", f.raw, `{"pool":"default"}`, "203.0.113.5:1234", "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("live-error create status = %d, want 429 (deny); body = %s", rec.Code, rec.Body.String())
	}
	if len(f.cp.got) != 0 {
		t.Error("control plane reached although the live count was unreadable")
	}
}

// TestBuildQuotaEnforcerModeNamesLivePosture asserts the startup mode string
// names whether the live concurrency cap is enforced, so the posture is never
// silent: with an injected live source it names live enforcement; without one
// it names the gap.
func TestBuildQuotaEnforcerModeNamesLivePosture(t *testing.T) {
	withLive := buildQuotaEnforcer(enforcementConfig{enabled: true, live: quota.LiveUsageFunc(
		func(_ context.Context, _ string) (quota.LiveUsage, error) { return quota.LiveUsage{}, nil },
	)})
	if !strings.Contains(withLive.mode, "live concurrency cap") {
		t.Errorf("with-live mode = %q, want it to name the live concurrency cap", withLive.mode)
	}
	withoutLive := buildQuotaEnforcer(enforcementConfig{enabled: true})
	if !strings.Contains(withoutLive.mode, "NOT enforced") {
		t.Errorf("without-live mode = %q, want it to name the unenforced live caps", withoutLive.mode)
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
	// Without an injected durable store the fallback is in-process and the mode
	// string must say so, so the dev-only posture is never silent.
	if !strings.Contains(w.mode, "NOT durable") {
		t.Errorf("mem-fallback mode = %q, want it to name the non-durable dev posture", w.mode)
	}
}

// TestBuildQuotaEnforcerUsesInjectedSuspensionStore asserts the wiring enforces
// against the INJECTED suspension store (main passes the durable Postgres store
// behind the TTL cache when a database is configured): a suspension already
// present in that store, as written by another replica or a previous process
// lifetime, is denied here, and the kill-switch writes back to the same store.
func TestBuildQuotaEnforcerUsesInjectedSuspensionStore(t *testing.T) {
	ctx := context.Background()
	shared := quota.NewMemSuspensionStore() // stands in for the shared durable store.
	// "Another replica" (or a pre-restart lifetime) already suspended the org.
	if err := shared.Suspend(ctx, quota.Suspension{OrgID: "org-1", Reason: quota.ReasonEmergencyStop, Note: "big red button"}); err != nil {
		t.Fatalf("pre-seed suspend: %v", err)
	}

	w := buildQuotaEnforcer(enforcementConfig{enabled: true, suspensions: shared})
	if w.suspensions != quota.SuspensionStore(shared) {
		t.Fatalf("wiring store = %T, want the injected store", w.suspensions)
	}
	if err := w.enforcer.Check(ctx, "org-1", "sandbox.status"); err == nil {
		t.Fatal("a suspension pre-seeded in the shared store must deny, got allow")
	}
	// The kill-switch lifts through the SAME store, so the org is admitted again.
	if lifted, err := w.killSwitch.Lift(ctx, "org-1"); err != nil || !lifted {
		t.Fatalf("lift = %v, %v; want true, nil", lifted, err)
	}
	if err := w.enforcer.Check(ctx, "org-1", "sandbox.status"); err != nil {
		t.Fatalf("after lift the org must be admitted, got %v", err)
	}
	if !strings.Contains(w.mode, "durable") {
		t.Errorf("injected-store mode = %q, want it to name the durable posture", w.mode)
	}
}
