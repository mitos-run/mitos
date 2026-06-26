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

	// Disabled: no route.
	muxOff := http.NewServeMux()
	mountOnboarding(muxOff, logger, accounts, store, capsGate{signup: false})
	roff := httptest.NewRequest(http.MethodPost, "/onboarding/signup", strings.NewReader(`{"email":"a@b.com"}`))
	rroff := httptest.NewRecorder()
	muxOff.ServeHTTP(rroff, roff)
	if rroff.Code != http.StatusNotFound {
		t.Fatalf("signup-off: expected 404 (route not mounted), got %d", rroff.Code)
	}

	// Enabled: route is mounted and accepts.
	muxOn := http.NewServeMux()
	mountOnboarding(muxOn, logger, accounts, store, capsGate{signup: true})
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
