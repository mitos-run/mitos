package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/billing"
	"mitos.run/mitos/internal/saas/onboarding"
	"mitos.run/mitos/internal/saas/orgprovision"
	"mitos.run/mitos/internal/saas/pgstore"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// mountOnboarding constructs the onboarding funnel and, when signup is enabled,
// mounts its PUBLIC unauthenticated endpoints on mux. It is the signup ->
// tenant-provisioned wiring:
//
//   - the funnel mode is ModeOpen only when MITOS_CONSOLE_SIGNUP is on (the #208
//     server-controlled gate); otherwise it stays in waitlist mode and the public
//     verify path provisions nothing;
//   - the EmailSender is the real SMTP sender when MITOS_SMTP_HOST is set, and the
//     dev log/fake sender otherwise. The active mode is logged (never the
//     password);
//   - the OrgProvisioner creates the cluster-scoped Org custom resource via a
//     controller-runtime client when MITOS_CONSOLE_ORG_TENANCY is on, so a verified
//     signup provisions the per-org namespace. With no client it is skipped with a
//     warning rather than failing signup.
//
// When pool is non-nil (durable Postgres configured), the pending-signup store
// and credit ledger are backed by Postgres so they survive console restarts.
// When pool is nil (dev / in-memory mode) the in-memory fallbacks are used; a
// pending unverified signup is cheap to lose on restart (the user re-signs up).
// Provisioned accounts/orgs always live in the durable saas.Store.
//
// sessions is the SAME store the session middleware reads; when non-nil a
// successful fresh verify mints a session and sets the mitos_session cookie so
// the new user arrives at the console already authenticated. newToken is the
// SAME generator used by the OIDC callback; the raw token is never logged.
// secure is the Secure cookie flag, matching the OIDC handler's value.
func mountOnboarding(mux *http.ServeMux, logger *slog.Logger, accounts *saas.AccountService, store saas.Store, pool *pgxpool.Pool, creditLedger billing.CreditLedger, caps signupGate, sessions saas.Sessions, newToken func() string, secure bool, allowlist onboarding.Allowlist) {
	if !caps.signupEnabled() {
		logger.Info("onboarding signup disabled (waitlist mode); public signup endpoints not mounted")
		return
	}

	email := buildEmailSender(logger)
	prov := buildOrgProvisioner(logger)

	opts := []onboarding.Option{
		onboarding.WithMode(onboarding.ModeOpen),
		onboarding.WithLogger(logger),
		onboarding.WithAllowlist(allowlist),
	}
	if prov != nil {
		opts = append(opts, onboarding.WithOrgProvisioner(prov))
	}
	// MITOS_CONSOLE_SIGNUP_CREDIT_CENTS overrides the signup credit for this
	// deployment. When set to a positive integer it takes precedence over
	// billing.DefaultSignupCredit; unset or non-positive values fall through
	// to the default. Example: MITOS_CONSOLE_SIGNUP_CREDIT_CENTS=500 yields
	// a $5.00 (500 cent) grant.
	if m, ok := signupCreditFromEnv(); ok {
		opts = append(opts, onboarding.WithSignupCredit(m))
		logger.Info("onboarding signup credit overridden via env",
			"env", "MITOS_CONSOLE_SIGNUP_CREDIT_CENTS",
			"cents", int64(m),
		)
	}

	// E2E QA seam: capture raw tokens in-process when MITOS_CONSOLE_E2E is set.
	// The sink is NEVER created and the endpoint is NEVER mounted when the flag
	// is off, so there is no path to the seam in production deployments.
	var e2eSink *onboarding.MemE2ETokenSink
	if envBool("MITOS_CONSOLE_E2E") {
		e2eSink = onboarding.NewMemE2ETokenSink()
		opts = append(opts, onboarding.WithE2ETokenSink(e2eSink))
		logger.Info("onboarding E2E token sink enabled (QA only; NEVER enable in production)")
	}

	// Select the durable pending store when a Postgres pool is available; fall
	// back to the in-memory implementation in dev mode (no DSN configured).
	// The credit ledger is the single shared instance passed in from main so
	// onboarding grants are visible in the billing view.
	var pending onboarding.PendingStore
	if pool != nil {
		pending = pgstore.NewPgPendingStore(pool)
	} else {
		pending = onboarding.NewMemPendingStore()
	}

	svc := onboarding.NewService(
		accounts,
		store,
		pending,
		creditLedger,
		email,
		opts...,
	)

	handlerOpts := []onboarding.HandlerOption{
		onboarding.WithHandlerSessions(sessions, newToken, secure),
	}

	// Friendly Captcha server-side verification: both the secret and the sitekey
	// must be set for the verifier to be active. When either is absent the
	// handler passes through (self-host / pre-launch unaffected). The secret is
	// never logged.
	{
		fcSecret := os.Getenv("MITOS_CONSOLE_FRIENDLY_CAPTCHA_SECRET")
		fcSiteKey := os.Getenv("MITOS_CONSOLE_FRIENDLY_CAPTCHA_SITEKEY")
		if fcSecret != "" && fcSiteKey != "" {
			fcBaseURL := os.Getenv("MITOS_CONSOLE_FRIENDLY_CAPTCHA_URL")
			handlerOpts = append(handlerOpts, onboarding.WithCaptcha(
				onboarding.NewFriendlyCaptcha(fcSecret, fcSiteKey, fcBaseURL, http.DefaultClient),
			))
			logger.Info("onboarding Friendly Captcha verification enabled")
		}
	}

	if d, err := onboarding.LoadDisposable(os.Getenv("MITOS_CONSOLE_DISPOSABLE_ALLOW")); err != nil {
		logger.Warn("disposable-domain check could not load; signup will proceed without it", "err", err.Error())
	} else {
		handlerOpts = append(handlerOpts, onboarding.WithDisposable(d))
	}

	// MITOS_CONSOLE_TRUSTED_PROXY_HOPS sets how many reverse proxies sit in front
	// of the console. With 0 (the default, safe for self-hosters) X-Forwarded-For
	// is ignored and the velocity key is the direct RemoteAddr, preventing clients
	// from minting fresh rate-limit buckets by spoofing XFF. Set to 1 for the
	// hosted deployment that sits behind a single gateway.
	if s := strings.TrimSpace(os.Getenv("MITOS_CONSOLE_TRUSTED_PROXY_HOPS")); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			if n > 0 {
				handlerOpts = append(handlerOpts, onboarding.WithTrustedProxyHops(n))
			}
		} else {
			logger.Warn("MITOS_CONSOLE_TRUSTED_PROXY_HOPS is not a valid integer; defaulting to 0 (X-Forwarded-For ignored)")
		}
	}

	// Per-IP velocity cap: MITOS_CONSOLE_SIGNUP_IP_LIMIT (int, default 10) and
	// MITOS_CONSOLE_SIGNUP_IP_WINDOW (duration string, default "1h"). A limit of
	// 0 disables the cap. Malformed values fall back to safe defaults so a bad
	// env variable never breaks all signup.
	{
		limit := 10
		if s := strings.TrimSpace(os.Getenv("MITOS_CONSOLE_SIGNUP_IP_LIMIT")); s != "" {
			if n, err := strconv.Atoi(s); err == nil {
				limit = n
			} else {
				logger.Warn("MITOS_CONSOLE_SIGNUP_IP_LIMIT is not a valid integer; defaulting to 10")
			}
		}
		if limit > 0 {
			window := time.Hour
			if s := strings.TrimSpace(os.Getenv("MITOS_CONSOLE_SIGNUP_IP_WINDOW")); s != "" {
				if d, err := time.ParseDuration(s); err == nil {
					window = d
				} else {
					logger.Warn("MITOS_CONSOLE_SIGNUP_IP_WINDOW is not a valid duration; defaulting to 1h")
				}
			}
			handlerOpts = append(handlerOpts, onboarding.WithVelocity(onboarding.NewVelocity(limit, window)))
		}
	}

	h := onboarding.NewHandler(svc, logger, handlerOpts...)
	h.Routes(mux)
	logger.Info("onboarding signup endpoints mounted",
		"mode", "open",
		"org_provisioner", prov != nil,
		"session_cookie", sessions != nil,
	)

	// Mount the E2E token retrieval endpoint ONLY when all three conditions hold:
	// the flag is on, a bearer token is configured, and a domain suffix is configured.
	// Bearer and domain values are read from env but never logged.
	if e2eSink != nil {
		bearer := os.Getenv("MITOS_CONSOLE_E2E_TOKEN")
		domain := os.Getenv("MITOS_CONSOLE_E2E_DOMAIN")
		switch {
		case bearer == "":
			logger.Warn("MITOS_CONSOLE_E2E set but MITOS_CONSOLE_E2E_TOKEN is empty; E2E endpoint NOT mounted")
		case domain == "":
			logger.Warn("MITOS_CONSOLE_E2E set but MITOS_CONSOLE_E2E_DOMAIN is empty; E2E endpoint NOT mounted")
		default:
			onboarding.NewE2EHandler(bearer, domain, e2eSink).Routes(mux)
			logger.Info("onboarding E2E token endpoint mounted (QA only; bearer and domain gates active)")
		}
	}
}

// signupGate is the minimal capability surface mountOnboarding reads, so the
// wiring does not depend on the full Capabilities struct.
type signupGate interface{ signupEnabled() bool }

// capsGate adapts the console capabilities Signup flag.
type capsGate struct{ signup bool }

func (g capsGate) signupEnabled() bool { return g.signup }

// buildEmailSender returns the real SMTP sender when MITOS_SMTP_HOST is set, and
// the dev log/fake sender otherwise. The SMTP password is read from the
// environment and is NEVER logged. The active mode is logged at startup.
func buildEmailSender(logger *slog.Logger) onboarding.EmailSender {
	host := os.Getenv("MITOS_SMTP_HOST")
	if host == "" {
		logger.Warn("MITOS_SMTP_HOST unset; using the DEV log email sender (no real verification email is delivered)")
		return devLogEmailSender{log: logger}
	}
	port := 587
	if p := os.Getenv("MITOS_SMTP_PORT"); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			port = n
		}
	}
	sender, err := onboarding.NewSMTPEmailSender(onboarding.SMTPConfig{
		Host:          host,
		Port:          port,
		Username:      os.Getenv("MITOS_SMTP_USERNAME"),
		Password:      os.Getenv("MITOS_SMTP_PASSWORD"),
		From:          os.Getenv("MITOS_SMTP_FROM"),
		VerifyBaseURL: os.Getenv("MITOS_ONBOARDING_VERIFY_URL"),
		// InviteBaseURL is a sibling of VerifyBaseURL for the invite accept
		// flow (e.g. https://app.mitos.run/invite/accept). Left unset,
		// SendInvite fails closed with a configuration error rather than a
		// broken link; a deployment that never enables team invites need not
		// set this.
		InviteBaseURL: os.Getenv("MITOS_ONBOARDING_INVITE_URL"),
	})
	if err != nil {
		// A misconfigured SMTP block falls back to the dev sender with a warning
		// rather than failing the whole console; the warning never contains the
		// password. The error from NewSMTPEmailSender only reports missing non-secret
		// fields (host/from/verify url).
		logger.Warn("SMTP email sender misconfigured; falling back to the DEV log sender", "err", err.Error())
		return devLogEmailSender{log: logger}
	}
	logger.Info("onboarding using the real SMTP email sender",
		"host", host,
		"port", port,
		"from", os.Getenv("MITOS_SMTP_FROM"),
	)
	return sender
}

// buildOrgProvisioner returns an OrgProvisioner backed by an in-cluster
// controller-runtime client when MITOS_CONSOLE_ORG_TENANCY is enabled. With the
// flag off, or when no in-cluster config is available (pure dev), it returns nil
// so a verified signup skips namespace provisioning with a warning rather than
// failing.
func buildOrgProvisioner(logger *slog.Logger) onboarding.OrgProvisioner {
	if !envBool("MITOS_CONSOLE_ORG_TENANCY") {
		logger.Info("MITOS_CONSOLE_ORG_TENANCY off; verified signups will NOT provision a tenant namespace")
		return nil
	}
	cfg, err := ctrl.GetConfig()
	if err != nil {
		logger.Warn("no Kubernetes config available; verified signups will NOT provision a tenant namespace", "err", err.Error())
		return nil
	}
	c, err := client.New(cfg, client.Options{Scheme: onboardingScheme()})
	if err != nil {
		logger.Warn("could not build Kubernetes client; verified signups will NOT provision a tenant namespace", "err", err.Error())
		return nil
	}
	logger.Info("onboarding org provisioning enabled; verified signups create the cluster-scoped Org custom resource")
	return orgprovision.New(c)
}

// onboardingScheme builds the scheme the org provisioner needs: client-go core
// plus mitos.run/v1 (the Org kind).
func onboardingScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1.AddToScheme(scheme))
	return scheme
}

// signupCreditFromEnv reads MITOS_CONSOLE_SIGNUP_CREDIT_CENTS and returns the
// billing.Money value (minor-unit cents) and true when the env is set to a
// positive integer. Invalid, zero, or negative values return 0, false so the
// caller falls through to billing.DefaultSignupCredit.
//
// billing.Money is an int64 count of cents (100 = $1.00), so
// MITOS_CONSOLE_SIGNUP_CREDIT_CENTS=500 yields billing.Money(500) = $5.00.
func signupCreditFromEnv() (billing.Money, bool) {
	s := strings.TrimSpace(os.Getenv("MITOS_CONSOLE_SIGNUP_CREDIT_CENTS"))
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, false
	}
	return billing.Money(n), true
}

// devLogEmailSender is the development EmailSender: it logs that a verification
// email WOULD have been sent, WITHOUT logging the token (the token is a secret).
// It is the default when SMTP is not configured, so local runs work without a
// mail server while never leaking the token.
type devLogEmailSender struct{ log *slog.Logger }

func (d devLogEmailSender) SendVerification(_ context.Context, _ string, _ string) error {
	// Never log the email or the token; only that a send occurred.
	d.log.Info("dev email sender: verification email suppressed (configure SMTP to deliver real mail)")
	return nil
}

func (d devLogEmailSender) SendApproved(_ context.Context, _ string) error {
	// Never log the email; only that a send occurred.
	d.log.Info("dev email sender: approved email suppressed (configure SMTP to deliver real mail)")
	return nil
}

func (d devLogEmailSender) SendInvite(_ context.Context, _, _, _, _ string) error {
	// Never log the email, org name, inviter name, or token; only that a
	// send occurred.
	d.log.Info("dev email sender: invite email suppressed (configure SMTP to deliver real mail)")
	return nil
}
