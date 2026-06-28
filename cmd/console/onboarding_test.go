package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/billing"
)

// TestMountOnboardingGatedBySignup asserts the public signup endpoint is mounted
// only when signup is enabled. With signup off (waitlist), the route is absent.
func TestMountOnboardingGatedBySignup(t *testing.T) {
	t.Setenv("MITOS_SMTP_HOST", "")
	t.Setenv("MITOS_CONSOLE_ORG_TENANCY", "")
	logger := slog.New(slog.NewTextHandler(new(bytes.Buffer), nil))
	store := saas.NewMemStore()
	keys := saas.NewKeyService(store)
	accounts := saas.NewAccountService(store, keys)

	sessions := saas.NewSessionStore()
	newTok := func() string { return "test-session-token" }

	// Disabled: no route. Pass nil pool to use the in-memory fallback stores.
	muxOff := http.NewServeMux()
	mountOnboarding(muxOff, logger, accounts, store, nil, billing.NewMemCreditLedger(), capsGate{signup: false}, sessions, newTok, false)
	roff := httptest.NewRequest(http.MethodPost, "/onboarding/signup", strings.NewReader(`{"email":"a@b.com"}`))
	rroff := httptest.NewRecorder()
	muxOff.ServeHTTP(rroff, roff)
	if rroff.Code != http.StatusNotFound {
		t.Fatalf("signup-off: expected 404 (route not mounted), got %d", rroff.Code)
	}

	// Enabled: route is mounted and accepts. Pass nil pool to use the in-memory fallback stores.
	muxOn := http.NewServeMux()
	mountOnboarding(muxOn, logger, accounts, store, nil, billing.NewMemCreditLedger(), capsGate{signup: true}, sessions, newTok, false)
	ron := httptest.NewRequest(http.MethodPost, "/onboarding/signup", strings.NewReader(`{"email":"a@b.com"}`))
	ron.Header.Set("Content-Type", "application/json")
	rron := httptest.NewRecorder()
	muxOn.ServeHTTP(rron, ron)
	if rron.Code != http.StatusAccepted {
		t.Fatalf("signup-on: expected 202, got %d (body %s)", rron.Code, rron.Body.String())
	}
}

// TestDevLogEmailSenderNeverLogsToken asserts the dev fallback sender logs that a
// send occurred but NEVER writes the email or the token.
func TestDevLogEmailSenderNeverLogsToken(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	sender := devLogEmailSender{log: logger}
	if err := sender.SendVerification(context.Background(), "secret@example.com", "super-secret-token"); err != nil {
		t.Fatalf("send: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "super-secret-token") {
		t.Fatalf("dev sender logged the token: %q", out)
	}
	if strings.Contains(out, "secret@example.com") {
		t.Fatalf("dev sender logged the email: %q", out)
	}
}

// TestBuildEmailSenderFallsBackWhenSMTPUnset asserts that with no SMTP host the
// builder returns the dev sender rather than nil or a panic.
func TestBuildEmailSenderFallsBackWhenSMTPUnset(t *testing.T) {
	t.Setenv("MITOS_SMTP_HOST", "")
	logger := slog.New(slog.NewTextHandler(new(bytes.Buffer), nil))
	if _, ok := buildEmailSender(logger).(devLogEmailSender); !ok {
		t.Fatal("expected dev log email sender when SMTP is not configured")
	}
}

// TestBuildOrgProvisionerNilWhenTenancyOff asserts the provisioner is nil when
// MITOS_CONSOLE_ORG_TENANCY is off, so signups do not try to reach an apiserver.
func TestBuildOrgProvisionerNilWhenTenancyOff(t *testing.T) {
	t.Setenv("MITOS_CONSOLE_ORG_TENANCY", "")
	logger := slog.New(slog.NewTextHandler(new(bytes.Buffer), nil))
	if buildOrgProvisioner(logger) != nil {
		t.Fatal("expected nil provisioner when org tenancy is off")
	}
}

// TestE2EEndpointNotMountedWhenFlagOff asserts GET /onboarding/e2e/token
// returns 404 when MITOS_CONSOLE_E2E is unset, regardless of signup state.
func TestE2EEndpointNotMountedWhenFlagOff(t *testing.T) {
	t.Setenv("MITOS_CONSOLE_E2E", "")
	t.Setenv("MITOS_SMTP_HOST", "")
	t.Setenv("MITOS_CONSOLE_ORG_TENANCY", "")
	logger := slog.New(slog.NewTextHandler(new(bytes.Buffer), nil))
	store := saas.NewMemStore()
	keys := saas.NewKeyService(store)
	accounts := saas.NewAccountService(store, keys)
	sessions := saas.NewSessionStore()
	newTok := func() string { return "test-session-token" }

	mux := http.NewServeMux()
	mountOnboarding(mux, logger, accounts, store, nil, billing.NewMemCreditLedger(), capsGate{signup: true}, sessions, newTok, false)

	req := httptest.NewRequest(http.MethodGet, "/onboarding/e2e/token?email=qa@e2e.mitos.run", nil)
	req.Header.Set("Authorization", "Bearer any-bearer")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("E2E endpoint: status %d, want 404 (not mounted when flag off)", rr.Code)
	}
}

// TestSignupCreditFromEnv asserts that MITOS_CONSOLE_SIGNUP_CREDIT_CENTS=500
// produces billing.Money(500) (500 cents = $5.00) and that invalid or unset
// values return false so billing.DefaultSignupCredit applies.
func TestSignupCreditFromEnv(t *testing.T) {
	cases := []struct {
		name      string
		val       string
		wantMoney billing.Money
		wantOK    bool
	}{
		{"set to 500", "500", billing.Money(500), true},
		{"set to 100", "100", billing.Money(100), true},
		{"set to 10000", "10000", billing.Money(10000), true},
		{"zero", "0", 0, false},
		{"negative", "-1", 0, false},
		{"invalid", "abc", 0, false},
		{"empty", "", 0, false},
		{"whitespace", "  500  ", billing.Money(500), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MITOS_CONSOLE_SIGNUP_CREDIT_CENTS", tc.val)
			got, ok := signupCreditFromEnv()
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got != tc.wantMoney {
				t.Fatalf("money = %v, want %v", got, tc.wantMoney)
			}
		})
	}
}

// TestE2EEndpointMountedWhenFlagOn asserts GET /onboarding/e2e/token is reachable
// (401 for missing bearer) when MITOS_CONSOLE_E2E is truthy.
func TestE2EEndpointMountedWhenFlagOn(t *testing.T) {
	t.Setenv("MITOS_CONSOLE_E2E", "1")
	t.Setenv("MITOS_CONSOLE_E2E_TOKEN", "my-qa-bearer")
	t.Setenv("MITOS_CONSOLE_E2E_DOMAIN", "e2e.mitos.run")
	t.Setenv("MITOS_SMTP_HOST", "")
	t.Setenv("MITOS_CONSOLE_ORG_TENANCY", "")
	logger := slog.New(slog.NewTextHandler(new(bytes.Buffer), nil))
	store := saas.NewMemStore()
	keys := saas.NewKeyService(store)
	accounts := saas.NewAccountService(store, keys)
	sessions := saas.NewSessionStore()
	newTok := func() string { return "test-session-token" }

	mux := http.NewServeMux()
	mountOnboarding(mux, logger, accounts, store, nil, billing.NewMemCreditLedger(), capsGate{signup: true}, sessions, newTok, false)

	// Route is mounted; wrong bearer returns 401 (not 404).
	req := httptest.NewRequest(http.MethodGet, "/onboarding/e2e/token?email=qa@e2e.mitos.run", nil)
	req.Header.Set("Authorization", "Bearer wrong-bearer")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("E2E endpoint mounted but wrong bearer: status %d, want 401", rr.Code)
	}
}
