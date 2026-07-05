package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/billing"
	"mitos.run/mitos/internal/saas/console"
	"mitos.run/mitos/internal/saas/onboarding"
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
	mountOnboarding(muxOff, logger, accounts, store, onboarding.NewMemPendingStore(), billing.NewMemCreditLedger(), capsGate{signup: false}, sessions, newTok, false, nil)
	roff := httptest.NewRequest(http.MethodPost, "/onboarding/signup", strings.NewReader(`{"email":"a@b.com"}`))
	rroff := httptest.NewRecorder()
	muxOff.ServeHTTP(rroff, roff)
	if rroff.Code != http.StatusNotFound {
		t.Fatalf("signup-off: expected 404 (route not mounted), got %d", rroff.Code)
	}

	// Enabled: route is mounted and accepts. Pass nil pool to use the in-memory fallback stores.
	muxOn := http.NewServeMux()
	mountOnboarding(muxOn, logger, accounts, store, onboarding.NewMemPendingStore(), billing.NewMemCreditLedger(), capsGate{signup: true}, sessions, newTok, false, nil)
	ron := httptest.NewRequest(http.MethodPost, "/onboarding/signup", strings.NewReader(`{"email":"a@b.com"}`))
	ron.Header.Set("Content-Type", "application/json")
	rron := httptest.NewRecorder()
	muxOn.ServeHTTP(rron, ron)
	if rron.Code != http.StatusAccepted {
		t.Fatalf("signup-on: expected 202, got %d (body %s)", rron.Code, rron.Body.String())
	}
}

// TestMountOnboardingWaitlistOnlyWhenSignupDisabled asserts that with signup
// disabled, mountOnboarding mounts POST /onboarding/waitlist (issue #718: the
// gap where waitlist mode had no HTTP intake at all), and that it records a
// waitlist entry the SAME onboarding.PendingStore console.Deps.Waitlist
// reads. It also asserts the endpoint is absent when signup is enabled (the
// full funnel is the intake in that mode).
func TestMountOnboardingWaitlistOnlyWhenSignupDisabled(t *testing.T) {
	t.Setenv("MITOS_SMTP_HOST", "")
	t.Setenv("MITOS_CONSOLE_ORG_TENANCY", "")
	logger := slog.New(slog.NewTextHandler(new(bytes.Buffer), nil))
	store := saas.NewMemStore()
	keys := saas.NewKeyService(store)
	accounts := saas.NewAccountService(store, keys)
	sessions := saas.NewSessionStore()
	newTok := func() string { return "test-session-token" }
	pending := onboarding.NewMemPendingStore()

	muxOff := http.NewServeMux()
	mountOnboarding(muxOff, logger, accounts, store, pending, billing.NewMemCreditLedger(), capsGate{signup: false}, sessions, newTok, false, nil)

	req := httptest.NewRequest(http.MethodPost, "/onboarding/waitlist", strings.NewReader(`{"email":"waiter@example.com"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	muxOff.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("waitlist join: status %d, want 202; body %s", rr.Code, rr.Body.String())
	}

	entries, err := pending.Waitlist(context.Background())
	if err != nil {
		t.Fatalf("Waitlist: %v", err)
	}
	if len(entries) != 1 || entries[0].Email != "waiter@example.com" {
		t.Fatalf("waitlist entries = %+v", entries)
	}

	// A duplicate join is a no-op (idempotent) and still returns 202.
	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/onboarding/waitlist", strings.NewReader(`{"email":"waiter@example.com"}`))
	req2.Header.Set("Content-Type", "application/json")
	muxOff.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusAccepted {
		t.Fatalf("duplicate waitlist join: status %d, want 202", rr2.Code)
	}
	entries, err = pending.Waitlist(context.Background())
	if err != nil {
		t.Fatalf("Waitlist (after duplicate): %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("waitlist entries after duplicate = %+v, want exactly one", entries)
	}

	// Not mounted when signup is enabled: the full funnel is the intake.
	muxOn := http.NewServeMux()
	mountOnboarding(muxOn, logger, accounts, store, onboarding.NewMemPendingStore(), billing.NewMemCreditLedger(), capsGate{signup: true}, sessions, newTok, false, nil)
	reqOn := httptest.NewRequest(http.MethodPost, "/onboarding/waitlist", strings.NewReader(`{"email":"a@b.com"}`))
	reqOn.Header.Set("Content-Type", "application/json")
	rrOn := httptest.NewRecorder()
	muxOn.ServeHTTP(rrOn, reqOn)
	if rrOn.Code != http.StatusNotFound {
		t.Fatalf("waitlist-with-signup-on: expected 404 (route not mounted), got %d", rrOn.Code)
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
	mountOnboarding(mux, logger, accounts, store, onboarding.NewMemPendingStore(), billing.NewMemCreditLedger(), capsGate{signup: true}, sessions, newTok, false, nil)

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
	mountOnboarding(mux, logger, accounts, store, onboarding.NewMemPendingStore(), billing.NewMemCreditLedger(), capsGate{signup: true}, sessions, newTok, false, nil)

	// Route is mounted; wrong bearer returns 401 (not 404).
	req := httptest.NewRequest(http.MethodGet, "/onboarding/e2e/token?email=qa@e2e.mitos.run", nil)
	req.Header.Set("Authorization", "Bearer wrong-bearer")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("E2E endpoint mounted but wrong bearer: status %d, want 401", rr.Code)
	}
}

// TestWaitlistAdapterListAndApprove asserts waitlistAdapter (the seam wired
// into console.Deps.Waitlist) round-trips the underlying PendingStore's
// waitlist entries and approves through the SAME allowlist/email mechanism
// POST /internal/approve-signup uses.
func TestWaitlistAdapterListAndApprove(t *testing.T) {
	pending := onboarding.NewMemPendingStore()
	allowlist := onboarding.NewMemAllowlist(nil)
	email := onboarding.NewFakeEmailSender()
	now := func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

	if err := pending.AddWaitlist(context.Background(), onboarding.WaitlistEntry{
		Email: "waiting@example.com", CreatedAt: now(),
	}); err != nil {
		t.Fatalf("AddWaitlist: %v", err)
	}

	a := waitlistAdapter{pending: pending, allowlist: allowlist, email: email, now: now}
	entries, err := a.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Email != "waiting@example.com" {
		t.Fatalf("entries = %+v", entries)
	}

	if alreadyApproved, err := a.Approve(context.Background(), "waiting@example.com"); err != nil {
		t.Fatalf("Approve: %v", err)
	} else if alreadyApproved {
		t.Fatal("a fresh approval must not report alreadyApproved")
	}
	if ok, _ := allowlist.IsAllowed(context.Background(), "waiting@example.com"); !ok {
		t.Fatal("Approve did not add the email to the allowlist")
	}
	if !email.Approved("waiting@example.com") {
		t.Fatal("Approve did not send the approved notification")
	}
}

// TestWaitlistAdapterApproveNotConfigured asserts a zero-value adapter (no
// allowlist/email wired) fails closed with console.ErrWaitlistNotConfigured
// rather than silently succeeding.
func TestWaitlistAdapterApproveNotConfigured(t *testing.T) {
	a := waitlistAdapter{pending: onboarding.NewMemPendingStore(), now: time.Now}
	if _, err := a.Approve(context.Background(), "x@example.com"); !errors.Is(err, console.ErrWaitlistNotConfigured) {
		t.Fatalf("err = %v, want console.ErrWaitlistNotConfigured", err)
	}
}
