package onboarding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/billing"
)

const (
	testE2EBearer = "test-bearer-secret"
	testE2EDomain = "e2e.mitos.run"
)

// newE2EHarness builds a Service with a MemE2ETokenSink wired in.
func newE2EHarness(t *testing.T) (*Service, *MemE2ETokenSink) {
	t.Helper()
	store := saas.NewMemStore()
	clock := staticClock()
	keys := saas.NewKeyService(store, saas.WithClock(clock))
	accounts := saas.NewAccountService(store, keys, saas.WithClock(clock))
	ledger := billing.NewMemCreditLedger()
	email := NewFakeEmailSender()
	sink := NewMemE2ETokenSink()
	n := 0
	tok := 0
	svc := NewService(accounts, store, NewMemPendingStore(), ledger, email,
		WithMode(ModeOpen),
		WithClock(clock),
		WithIDGen(func() string { n++; return "e2e-id-" + string(rune('a'+n)) }),
		WithTokenGen(func() (string, error) { tok++; return "e2e-tok-" + string(rune('0'+tok)), nil }),
		WithE2ETokenSink(sink),
	)
	return svc, sink
}

// staticClock returns a deterministic clock for e2e test helpers.
func staticClock() func() time.Time {
	ts := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return ts }
}

func e2eMux(t *testing.T, sink E2ETokenSink) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	NewE2EHandler(testE2EBearer, testE2EDomain, sink).Routes(mux)
	return mux
}

func getE2EToken(t *testing.T, mux *http.ServeMux, email, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/onboarding/e2e/token?email="+email, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

// TestE2ESinkRecordsTokenOnSignup verifies the sink captures the raw token.
func TestE2ESinkRecordsTokenOnSignup(t *testing.T) {
	svc, sink := newE2EHarness(t)
	if _, err := svc.SignUp(context.Background(), "qa@e2e.mitos.run", ""); err != nil {
		t.Fatalf("sign up: %v", err)
	}
	tok, ok := sink.Last("qa@e2e.mitos.run")
	if !ok || tok == "" {
		t.Fatal("sink must record the raw token after signup")
	}
}

// TestE2EHappyPath: flag+bearer+allowlisted domain -> 200 with token -> verify succeeds.
func TestE2EHappyPath(t *testing.T) {
	svc, sink := newE2EHarness(t)
	if _, err := svc.SignUp(context.Background(), "qa@e2e.mitos.run", ""); err != nil {
		t.Fatalf("sign up: %v", err)
	}

	mux := e2eMux(t, sink)
	rr := getE2EToken(t, mux, "qa@e2e.mitos.run", testE2EBearer)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d, want 200; body %s", rr.Code, rr.Body.String())
	}
	var out map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	rawToken := out["token"]
	if rawToken == "" {
		t.Fatal("response must contain a non-empty token field")
	}

	// The retrieved token must successfully verify.
	vr, err := svc.Verify(context.Background(), rawToken)
	if err != nil {
		t.Fatalf("verify with retrieved token: %v", err)
	}
	if vr.Account.Email != "qa@e2e.mitos.run" {
		t.Fatalf("verified account email = %q, want qa@e2e.mitos.run", vr.Account.Email)
	}
}

// TestE2EWrongBearerReturns401 verifies wrong bearer yields 401.
func TestE2EWrongBearerReturns401(t *testing.T) {
	_, sink := newE2EHarness(t)
	mux := e2eMux(t, sink)
	rr := getE2EToken(t, mux, "qa@e2e.mitos.run", "totally-wrong-bearer")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong bearer: status %d, want 401", rr.Code)
	}
}

// TestE2EEmptyBearerReturns401 verifies missing Authorization header yields 401.
func TestE2EEmptyBearerReturns401(t *testing.T) {
	_, sink := newE2EHarness(t)
	mux := e2eMux(t, sink)
	rr := getE2EToken(t, mux, "qa@e2e.mitos.run", "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing bearer: status %d, want 401", rr.Code)
	}
}

// TestE2ENonAllowlistedDomainReturns404 verifies a production-domain email yields 404.
func TestE2ENonAllowlistedDomainReturns404(t *testing.T) {
	svc, sink := newE2EHarness(t)
	// Signup with an allowlisted email so the sink has a token; then attempt to
	// fetch via a production email that is NOT on the allowlisted domain.
	if _, err := svc.SignUp(context.Background(), "qa@e2e.mitos.run", ""); err != nil {
		t.Fatalf("sign up: %v", err)
	}
	mux := e2eMux(t, sink)
	rr := getE2EToken(t, mux, "real@mitos.run", testE2EBearer)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("non-allowlisted domain: status %d, want 404", rr.Code)
	}
}

// TestE2EUnknownEmailReturns404 verifies no-token-yet yields 404.
func TestE2EUnknownEmailReturns404(t *testing.T) {
	_, sink := newE2EHarness(t)
	mux := e2eMux(t, sink)
	rr := getE2EToken(t, mux, "nobody@e2e.mitos.run", testE2EBearer)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown email: status %d, want 404", rr.Code)
	}
}

// TestE2EBearerNotLeakedInResponse asserts the bearer value is not echoed in any response body.
func TestE2EBearerNotLeakedInResponse(t *testing.T) {
	_, sink := newE2EHarness(t)
	mux := e2eMux(t, sink)
	rr := getE2EToken(t, mux, "qa@e2e.mitos.run", "totally-wrong-bearer")
	if contains(rr.Body.String(), "totally-wrong-bearer") {
		t.Fatal("bearer value must not appear in the response body")
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
