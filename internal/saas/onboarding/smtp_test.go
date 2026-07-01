package onboarding

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func newTestSMTP(t *testing.T) (*SMTPEmailSender, *capturedSMTP) {
	t.Helper()
	s, err := NewSMTPEmailSender(SMTPConfig{
		Host:          "smtp.example.com",
		Port:          587,
		Username:      "postmaster@example.com",
		Password:      "s3cr3t-smtp-password",
		From:          "no-reply@mitos.run",
		VerifyBaseURL: "https://app.mitos.run/auth/verify",
	})
	if err != nil {
		t.Fatalf("new smtp sender: %v", err)
	}
	cap := &capturedSMTP{}
	s.dial = cap.dial
	return s, cap
}

type capturedSMTP struct {
	cfg  SMTPConfig
	from string
	to   []string
	msg  []byte
	err  error
}

func (c *capturedSMTP) dial(_ context.Context, cfg SMTPConfig, from string, to []string, msg []byte) error {
	c.cfg, c.from, c.to, c.msg = cfg, from, to, msg
	return c.err
}

func TestSMTPSenderAddressesMessageAndCarriesToken(t *testing.T) {
	s, cap := newTestSMTP(t)
	if err := s.SendVerification(context.Background(), "user@example.com", "tok-abc123"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if cap.from != "no-reply@mitos.run" {
		t.Fatalf("envelope from %q, want no-reply@mitos.run", cap.from)
	}
	if len(cap.to) != 1 || cap.to[0] != "user@example.com" {
		t.Fatalf("envelope to %v, want [user@example.com]", cap.to)
	}
	body := string(cap.msg)
	if !strings.Contains(body, "To: user@example.com") {
		t.Fatalf("message missing To header: %q", body)
	}
	if !strings.Contains(body, "From: no-reply@mitos.run") {
		t.Fatalf("message missing From header: %q", body)
	}
	if !strings.Contains(body, "token=tok-abc123") {
		t.Fatalf("message missing verify token link: %q", body)
	}
}

// TestSMTPSenderNeverLeaksPassword asserts the password appears nowhere in the
// composed message or in an error returned on transport failure.
func TestSMTPSenderNeverLeaksPassword(t *testing.T) {
	const password = "s3cr3t-smtp-password"
	s, cap := newTestSMTP(t)

	// Success path: password must not be in the message bytes or the config the
	// dialer logs (it is in cfg, but the body it transmits must not echo it).
	if err := s.SendVerification(context.Background(), "u@example.com", "tok-1"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if strings.Contains(string(cap.msg), password) {
		t.Fatal("password leaked into the email body")
	}

	// Failure path: a transport error must not contain the password.
	cap.err = errors.New("connection refused")
	err := s.SendVerification(context.Background(), "u@example.com", "tok-2")
	if err == nil {
		t.Fatal("expected delivery error")
	}
	if strings.Contains(err.Error(), password) {
		t.Fatalf("password leaked into error: %v", err)
	}
}

func TestNewSMTPSenderValidatesConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  SMTPConfig
	}{
		{"no host", SMTPConfig{From: "a@b.c", VerifyBaseURL: "https://x/y"}},
		{"no from", SMTPConfig{Host: "h", VerifyBaseURL: "https://x/y"}},
		{"no verify url", SMTPConfig{Host: "h", From: "a@b.c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewSMTPEmailSender(tc.cfg); err == nil {
				t.Fatal("expected config validation error")
			}
		})
	}
}

func TestVerifyLinkPreservesExistingQuery(t *testing.T) {
	link, err := verifyLink("https://app.mitos.run/verify?ref=email", "tok-xyz")
	if err != nil {
		t.Fatalf("verify link: %v", err)
	}
	if !strings.Contains(link, "ref=email") || !strings.Contains(link, "token=tok-xyz") {
		t.Fatalf("link dropped a query parameter: %q", link)
	}
}

// TestConsoleOrigin pins the scheme+host derivation: the verify path and query
// must be stripped (a substring check in the message test would pass even if they
// were not, so this asserts the exact origin), and a parse failure falls back to
// the raw value so the caller still has a usable URL.
func TestConsoleOrigin(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"https://app.mitos.run/auth/verify", "https://app.mitos.run"},
		{"https://app.mitos.run/auth/verify?token=abc", "https://app.mitos.run"},
		{"https://app.mitos.run", "https://app.mitos.run"},
		{"http://localhost:8090/auth/verify", "http://localhost:8090"},
		{"not-a-url", "not-a-url"},
	}
	for _, c := range cases {
		if got := consoleOrigin(c.in); got != c.want {
			t.Fatalf("consoleOrigin(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestSMTPSenderApprovedComposesHeaders asserts that SendApproved composes a
// message with the correct From/To/Subject headers and a sign-in URL in the
// body, and that the envelope addresses match.
func TestSMTPSenderApprovedComposesHeaders(t *testing.T) {
	s, cap := newTestSMTP(t)
	if err := s.SendApproved(context.Background(), "user@example.com"); err != nil {
		t.Fatalf("send approved: %v", err)
	}
	if cap.from != "no-reply@mitos.run" {
		t.Fatalf("envelope from %q, want no-reply@mitos.run", cap.from)
	}
	if len(cap.to) != 1 || cap.to[0] != "user@example.com" {
		t.Fatalf("envelope to %v, want [user@example.com]", cap.to)
	}
	body := string(cap.msg)
	if !strings.Contains(body, "To: user@example.com") {
		t.Fatalf("message missing To header: %q", body)
	}
	if !strings.Contains(body, "From: no-reply@mitos.run") {
		t.Fatalf("message missing From header: %q", body)
	}
	if !strings.Contains(body, "Subject: You are in: start running forks on Mitos") {
		t.Fatalf("message missing Subject header: %q", body)
	}
	// The body must contain the sign-in URL derived from the console origin
	// (scheme + host of VerifyBaseURL, no path).
	if !strings.Contains(body, "https://app.mitos.run") {
		t.Fatalf("message missing sign-in URL: %q", body)
	}
}

// TestSMTPSenderApprovedNeverLeaksEmailOnError asserts the recipient email
// address does not appear in an error returned on a dialer failure.
func TestSMTPSenderApprovedNeverLeaksEmailOnError(t *testing.T) {
	s, cap := newTestSMTP(t)
	cap.err = errors.New("connection refused")
	err := s.SendApproved(context.Background(), "user@example.com")
	if err == nil {
		t.Fatal("expected delivery error on dialer failure")
	}
	if strings.Contains(err.Error(), "user@example.com") {
		t.Fatalf("email address leaked into error: %v", err)
	}
}
