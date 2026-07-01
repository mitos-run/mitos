package onboarding

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"net/url"
	"strings"
	"time"
)

// SMTPConfig configures the real SMTP EmailSender. The password is a SECRET: it
// is read from the environment / a mounted secret by the caller and is NEVER
// logged or placed in an error message. Host, Port, From, and the verify base URL
// are plain configuration.
type SMTPConfig struct {
	// Host is the SMTP server hostname (for example smtp.example.com).
	Host string
	// Port is the SMTP submission port (commonly 587 for STARTTLS).
	Port int
	// Username is the SMTP auth username.
	Username string
	// Password is the SMTP auth password. SECRET: never logged, never in errors.
	Password string
	// From is the envelope and header From address.
	From string
	// VerifyBaseURL is the base the verify link is built from; the token is added
	// as a query parameter. For example https://app.mitos.run/auth/verify.
	VerifyBaseURL string
	// TLSServerName overrides the TLS server name used for STARTTLS verification.
	// Empty defaults to Host. Used in tests to point at 127.0.0.1 while presenting
	// a cert for another name.
	TLSServerName string
}

// addr returns the host:port dial target.
func (c SMTPConfig) addr() string {
	return net.JoinHostPort(c.Host, fmt.Sprintf("%d", c.Port))
}

// smtpDialer is the seam over the SMTP transport so the sender is unit-tested
// without a live server. The production implementation dials a real SMTP server,
// upgrades to STARTTLS, authenticates, and delivers. Tests inject a fake.
type smtpDialer func(ctx context.Context, cfg SMTPConfig, from string, to []string, msg []byte) error

// SMTPEmailSender delivers the verification email over SMTP with STARTTLS and
// username/password auth, using only the standard library (net/smtp). It builds
// the verify link from VerifyBaseURL and the one-time token. The email body
// carries no secret beyond that one-time token, and the SMTP password is never
// logged or surfaced in an error.
type SMTPEmailSender struct {
	cfg  SMTPConfig
	dial smtpDialer
}

// NewSMTPEmailSender builds the real SMTP sender. It returns an error if the
// required non-secret configuration (host, from, verify base URL) is missing, so
// a misconfiguration fails fast at startup rather than at first signup.
func NewSMTPEmailSender(cfg SMTPConfig) (*SMTPEmailSender, error) {
	if cfg.Host == "" {
		return nil, fmt.Errorf("smtp email sender: host is required")
	}
	if cfg.Port == 0 {
		cfg.Port = 587
	}
	if cfg.From == "" {
		return nil, fmt.Errorf("smtp email sender: from address is required")
	}
	if cfg.VerifyBaseURL == "" {
		return nil, fmt.Errorf("smtp email sender: verify base url is required")
	}
	if _, err := url.Parse(cfg.VerifyBaseURL); err != nil {
		return nil, fmt.Errorf("smtp email sender: invalid verify base url: %w", err)
	}
	return &SMTPEmailSender{cfg: cfg, dial: realSMTPDial}, nil
}

// SendVerification builds the verify link from the configured base URL and the
// one-time token, composes a plain-text message, and delivers it over SMTP. The
// token is treated as a secret: it appears only in the verify link in the message
// body delivered to the user's inbox, never in a log line or returned error.
func (s *SMTPEmailSender) SendVerification(ctx context.Context, email, token string) error {
	link, err := verifyLink(s.cfg.VerifyBaseURL, token)
	if err != nil {
		return fmt.Errorf("smtp email sender: build verify link: %w", err)
	}
	msg := buildVerificationMessage(s.cfg.From, email, link)
	if err := s.dial(ctx, s.cfg, s.cfg.From, []string{email}, msg); err != nil {
		// The dialer is responsible for never embedding the password in its error;
		// realSMTPDial wraps only transport-level context.
		return fmt.Errorf("smtp email sender: deliver verification: %w", err)
	}
	return nil
}

// SendApproved composes the "you are in" approval email and delivers it over
// SMTP. The recipient email is treated as PII: it is never logged and never
// embedded in a returned error.
func (s *SMTPEmailSender) SendApproved(ctx context.Context, email string) error {
	// Approval adds the allowlist row; it does NOT provision. The user finishes by
	// signing up again (now past the gate), which issues a fresh verify link and
	// provisions. So the CTA points at the sign-up page, not the console root.
	signupURL := consoleOrigin(s.cfg.VerifyBaseURL) + "/signup"
	msg := buildApprovedMessage(s.cfg.From, email, signupURL)
	if err := s.dial(ctx, s.cfg, s.cfg.From, []string{email}, msg); err != nil {
		return fmt.Errorf("smtp email sender: deliver approved: %w", err)
	}
	return nil
}

// consoleOrigin returns the scheme and host of rawURL (no path, no query). When
// parsing fails it returns rawURL as-is so the caller still has a usable URL.
func consoleOrigin(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	return u.Scheme + "://" + u.Host
}

// buildApprovedMessage composes a minimal RFC 5322 plain-text approval email.
// The message carries no secret. Voice: plain, accessible, confident, peer of
// the best labs. No em or en dashes.
func buildApprovedMessage(from, to, signupURL string) []byte {
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: You are in: finish signing up for Mitos\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString("Your Mitos account is approved.\r\n\r\n")
	b.WriteString("Finish signing up to activate your account and run your first fork:\r\n\r\n")
	b.WriteString(signupURL + "\r\n\r\n")
	b.WriteString("Use the same email address you signed up with. We will send a link to confirm\r\n")
	b.WriteString("your email, and then your dashboard, first fork, and API keys are ready.\r\n\r\n")
	b.WriteString("Mitos gives you a persistent control plane that runs your forks, agents, and\r\n")
	b.WriteString("automations around the clock.\r\n\r\n")
	b.WriteString("Welcome aboard.\r\n")
	b.WriteString("The Mitos team\r\n")
	return []byte(b.String())
}

// verifyLink appends the token to the base URL as a query parameter, preserving
// any existing query. The token is url-encoded.
func verifyLink(base, token string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("token", token)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// buildVerificationMessage composes a minimal RFC 5322 plain-text message. The
// only sensitive content is the one-time verify link in the body.
func buildVerificationMessage(from, to, link string) []byte {
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: Verify your Mitos email\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString("Welcome to Mitos.\r\n\r\n")
	b.WriteString("Confirm your email address to finish creating your account:\r\n\r\n")
	b.WriteString(link + "\r\n\r\n")
	b.WriteString("This link is single-use and expires soon. If you did not request it, ignore this email.\r\n")
	return []byte(b.String())
}

// realSMTPDial performs the production SMTP delivery: connect, EHLO, STARTTLS,
// AUTH, and send. It uses only the standard library. On any failure it returns a
// transport-level error that never contains the password.
func realSMTPDial(ctx context.Context, cfg SMTPConfig, from string, to []string, msg []byte) error {
	d := net.Dialer{Timeout: 30 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", cfg.addr())
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	c, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp client: %w", err)
	}
	defer func() { _ = c.Close() }()

	serverName := cfg.TLSServerName
	if serverName == "" {
		serverName = cfg.Host
	}
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("starttls: %w", err)
		}
	}
	if cfg.Username != "" {
		auth := smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
		if err := c.Auth(auth); err != nil {
			// Do NOT wrap %w here: the std smtp Auth error can echo server text but
			// never the password; still, keep the message generic so no credential
			// material is surfaced.
			return fmt.Errorf("smtp auth failed for user %q", cfg.Username)
		}
	}
	if err := c.Mail(from); err != nil {
		return fmt.Errorf("mail from: %w", err)
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return fmt.Errorf("rcpt to: %w", err)
		}
	}
	wc, err := c.Data()
	if err != nil {
		return fmt.Errorf("data: %w", err)
	}
	if _, err := wc.Write(msg); err != nil {
		_ = wc.Close()
		return fmt.Errorf("write body: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("close body: %w", err)
	}
	return c.Quit()
}
