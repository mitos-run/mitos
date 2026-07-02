// Command console serves the hosted web console BACKEND-FOR-FRONTEND (BFF) for
// the SaaS offering (issue #214): an org-scoped JSON API that aggregates the
// accounts/keys (#210), usage/cost (#211), billing (#212), live sandboxes, and
// templates services into the views the console UI renders, plus a minimal
// server-rendered index that lists the org's keys and usage to PROVE the wiring.
//
// The load-bearing property is org-scoped data isolation: every endpoint reads
// the caller's org from the request context and returns ONLY that org's data.
// The SPA frontend, the real control-plane live-sandbox query, and log streaming
// are documented follow-ups (docs/saas/console.md); this binary ships the tested
// BFF they consume, wired over the in-memory stores.
//
// SECURITY: this binary's dev auth (the X-Console-Account / X-Console-Org
// headers) is a LOCAL SMOKE-TEST shim ONLY, gated behind -dev. In production the
// org and account context are attached by the #210 gateway / session auth after
// it verifies a real session, never from a client-supplied header. A key VALUE
// is never logged or returned except the one-time raw key on create. This front
// door is NOT cleared for production tenants until the #194 security review
// covers it. See docs/threat-model.md.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"html/template"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"mitos.run/mitos/cmd/console/spa"
	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/billing"
	"mitos.run/mitos/internal/saas/console"
	"mitos.run/mitos/internal/saas/oidcauth"
	"mitos.run/mitos/internal/saas/onboarding"
	"mitos.run/mitos/internal/saas/pgstore"
	"mitos.run/mitos/internal/telemetry"
	"mitos.run/mitos/internal/usage"
)

func main() {
	addr := flag.String("addr", ":8090", "console listen address")
	dev := flag.Bool("dev", false, "enable the local dev auth shim (X-Console-Account / X-Console-Org headers); NEVER enable in production")
	databaseDSN := flag.String("database-dsn", "", "Postgres DSN for durable persistence (accounts, orgs, memberships, API keys). Falls back to the "+pgstore.EnvDSN+" env var. Empty means in-memory persistence (DEV ONLY). The value is a secret and is never logged.")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Product telemetry is OPT-IN and OFF by default. FromEnv returns a no-op
	// emitter unless MITOS_TELEMETRY_ENABLED is truthy AND an endpoint is set, and
	// is force-disabled by DO_NOT_TRACK. The salt and any token are secrets and are
	// never logged. The onboarding funnel forwards signup.started / signup.verified
	// to it via onboarding.NewTelemetryRecorder once the self-serve funnel is wired
	// (it is a tested core today); the emitter is constructed here so the startup
	// log line and shutdown flush exist in the one place the binary owns lifecycle.
	tel := telemetry.FromEnv(logger)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tel.Shutdown(ctx)
	}()

	// Durable Postgres when a DSN is configured (flag or MITOS_DATABASE_DSN),
	// in-memory otherwise (dev only). The DSN value is never logged. The real
	// billing service, control-plane live-sandbox query, and SandboxTemplate
	// lister remain documented follow-ups (docs/saas/console.md).
	// pool is non-nil when durable Postgres is configured; nil means in-memory.
	store, pool, closeStore, err := pgstore.ResolveStoreWithPool(context.Background(), *databaseDSN, logger)
	if err != nil {
		log.Fatalf("persistence: %v", err)
	}
	defer closeStore()
	keys := saas.NewKeyService(store)
	accounts := saas.NewAccountService(store, keys)

	caps := capabilitiesFromEnv()
	// One status store is shared by the BFF billing view and the billing webhook
	// so a provider event (payment failed / canceled) is reflected in the console.
	statusStore := billing.NewMemStatusStore()

	// creditLedger is the single shared instance used by the onboarding grant
	// path, the billing view, AND the billing webhook (so a cleared top-up from
	// the provider is immediately visible in the console). It is constructed
	// BEFORE setupBilling so the same instance can be forwarded into the webhook
	// handler without building a second ledger.
	var creditLedger billing.CreditLedger
	if pool != nil {
		creditLedger = pgstore.NewPgCreditLedger(pool)
	} else {
		creditLedger = billing.NewMemCreditLedger()
	}

	// Billing rate table: MITOS_CONSOLE_RATES (a JSON object mapping onto
	// billing.Rates; Helm value console.billing.rates) REPLACES the built-in
	// illustrative defaults when set. A malformed value fails startup (fail
	// closed) rather than silently pricing usage at the wrong rates; the parse
	// error carries the remediation text. The SAME table prices the billing
	// view, the drawdown driver, and (via ToPriceList) the display cost
	// estimate, so they never drift. See docs/saas/pricing.md.
	rates, err := billing.ParseRatesConfig(os.Getenv("MITOS_CONSOLE_RATES"))
	if err != nil {
		log.Fatalf("MITOS_CONSOLE_RATES: %v", err)
	}

	bill := setupBilling(logger, statusStore, creditLedger)

	// sessionStore is created before console.New so it can be passed into
	// Deps.Sessions in the production branch. When pool is non-nil (durable
	// Postgres configured) the durable PgSessionStore is used; otherwise the
	// in-memory SessionStore is the dev fallback.
	var sessionStore saas.Sessions
	if pool != nil {
		sessionStore = pgstore.NewPgSessionStore(pool)
	} else {
		sessionStore = saas.NewSessionStore()
	}

	// spendCapStore is the durable per-org spend-cap store when Postgres is
	// configured, in-memory otherwise. This closes the money-safety gap where a
	// redeploy silently dropped every configured cap.
	var spendCapStore billing.SpendCapStore
	if pool != nil {
		spendCapStore = pgstore.NewPgSpendCapStore(pool)
	} else {
		spendCapStore = billing.NewMemSpendCapStore()
	}

	// The usage store the console reads: the controller's internal usage API
	// (the SAME usage the collector recorded) when configured, in-memory in dev.
	// Shared by the BFF usage view and the drawdown driver below.
	usageStore := buildUsageStore(logger)

	con := console.New(console.Deps{
		Accounts: accounts,
		Usage:    usageStore,
		// The proof-snapshot and fork-tree sources: org-scoped cluster queries when
		// in a cluster, in-memory otherwise.
		Instruments: buildInstruments(logger),
		ForkTree:    buildForkTree(logger),
		Billing: console.BillingReader{
			Ledger: creditLedger,
			Status: statusStore,
			Caps:   spendCapStore,
			Rates:  rates,
		},
		// The display price list derives from the SAME configured rate table
		// the ledger bills with, so the estimate a user sees matches what is
		// charged.
		Prices: rates.ToPriceList(),
		// The active secret backend selected from config (kube / openbao), falling
		// back to in-memory in dev. Capabilities advertise the same providers.
		Secrets: buildSecretStore(logger, caps),
		// The live-sandbox control: the org-scoped cluster query when in a cluster,
		// in-memory otherwise. Shares the org→namespace boundary with secrets.
		Sandboxes: buildSandboxControl(logger),
		// The manage-subscription portal link (provider-neutral); nil keeps the
		// console's no-portal default (community edition).
		Portal: bill.portal,
		// The prepaid credit top-up seam + its provider product/currency; nil
		// keeps the console's no-top-up default (community edition), and an empty
		// product id makes the endpoint return 400 until configured.
		TopUp:          bill.topUp,
		TopUpProductID: bill.topUpProductID,
		TopUpCurrency:  bill.topUpCurrency,
		// Edition + feature flags from the server-controlled environment the chart
		// sets; the SAME binary serves both editions.
		Capabilities: caps,
		// Wire the real session store so /console/account/sessions reflects live
		// sessions. The adapter translates saas.Session to console.SessionRecord.
		// Both dev and production share the same store; in dev the store is empty
		// (no real session middleware issues tokens), which is fine for local
		// smoke testing.
		Sessions: sessionStoreAdapter{s: sessionStore},
		Log:      logger,
	})

	// Usage drawdown driver (issue #602): the periodic loop that settles each
	// org's metered usage against its prepaid credit via
	// billing.Service.Drawdown. Without it the collector records usage but
	// credits never move. Enabled by default (every 5m) when the usage store is
	// the live controller-API-backed one; off when the store is the in-memory
	// dev fallback (nothing real to settle); MITOS_CONSOLE_DRAWDOWN_INTERVAL
	// overrides (a Go duration, or 0/off to disable). The service is built over
	// the SAME credit ledger and status store the BFF billing view reads, and
	// prices with the SAME rate table (billing.DefaultRates unless
	// MITOS_CONSOLE_RATES overrides it; docs/saas/pricing.md). No billing
	// provider is involved: Drawdown only prices the record and debits prepaid
	// credit, idempotently per (org, sandbox, window). The driver logs counts
	// only, never balances or costs.
	_, usageLive := usageStore.(*usage.HTTPStore)
	ddInterval, err := drawdownInterval(os.Getenv("MITOS_CONSOLE_DRAWDOWN_INTERVAL"), usageLive)
	if err != nil {
		log.Fatalf("drawdown: %v", err)
	}
	if ddInterval > 0 {
		dd := billing.NewService(billing.Config{Ledger: creditLedger, Status: statusStore, Rates: rates})
		startDrawdownDriver(context.Background(), logger, ddInterval, store, usageStore, dd)
		logger.Info("usage drawdown driver enabled", "interval", ddInterval.String())
	} else {
		logger.Info("usage drawdown driver disabled (in-memory usage store or interval 0/off)")
	}

	// sessionSvc is constructed before the dev/prod branch so the internal
	// machine-to-machine endpoints (mounted unconditionally below) can share it
	// with the production session middleware without re-allocating.
	sessionSvc := saas.NewSessionService(sessionStore, accounts)

	mux := http.NewServeMux()
	// The BFF API. In production it is mounted behind the session middleware that
	// resolves the OIDC session cookie to the verified org context; here the dev
	// shim does it from headers instead.
	if *dev {
		logger.Warn("console dev auth shim enabled; do not run this in production")
		mux.Handle("/console/", devAuth(con))
		mux.HandleFunc("/", indexPage(logger))
	} else {
		// Production: the session middleware attaches the verified caller from the
		// OIDC session cookie, and the embedded SPA is served at the root. ONE
		// binary serves the BFF and the UI. The login flow and the middleware share
		// ONE session store so a session issued at /auth/callback resolves here.
		sessionMW := console.SessionMiddleware(sessionSvc)
		mux.Handle("/console/", sessionMW(con))
		mux.Handle("/", spa.Handler())
		// Run with Mitos endpoints sit behind the same session auth so each call
		// carries the verified org. Opt-in via MITOS_CONSOLE_RUN_WITH_MITOS.
		mountRunWithMitos(mux, sessionMW, logger)
		if issuer := os.Getenv("MITOS_CONSOLE_OIDC_ISSUER"); issuer != "" {
			mountAuth(mux, logger, accounts, sessionStore, issuer)
		} else {
			logger.Warn("MITOS_CONSOLE_OIDC_ISSUER unset; /auth login flow not mounted (BFF requires a session)")
		}
	}
	// The onboarding funnel is PUBLIC and unauthenticated by design: signup and
	// verify are how a brand-new user with no session creates an account. They are
	// mounted OUTSIDE the session middleware. They are gated by the same
	// server-controlled signup flag (caps.Signup, the #208 gate); when off, nothing
	// is mounted and the funnel stays in waitlist mode. The SMTP password and the
	// verify token are never logged.
	// Pass the SAME session store and token generator the OIDC callback uses so
	// a verified signup issues a session cookie with the same contract.
	// Secure mirrors mountAuth (hardcoded true: the console is always TLS in
	// production; -dev runs without cookies anyway since no session middleware
	// is active in that branch).
	// Auto-allow domains for the signup allowlist gate. Comma-separated, lowercased;
	// default mitos.run so a mitos.run address never needs a manual approval.
	autoAllow := parseAutoAllowDomains(os.Getenv("MITOS_CONSOLE_AUTOALLOW_DOMAINS"))
	var allowlist onboarding.Allowlist
	if pool != nil {
		allowlist = pgstore.NewPgAllowlist(pool, autoAllow)
	} else {
		logger.Warn("allowlist is in-memory (dev only); approved entries do not survive restarts")
		allowlist = onboarding.NewMemAllowlist(autoAllow)
	}
	mountOnboarding(mux, logger, accounts, store, pool, creditLedger, capsGate{signup: caps.Signup}, sessionStore, newSessionToken, true, allowlist)

	// The billing webhook is PUBLIC by design: it is authenticated by the
	// provider's signature, not a session, so it is mounted OUTSIDE the session
	// middleware. It verifies the signature before touching any billing state.
	if bill.webhook != nil {
		mux.Handle("/webhooks/billing", bill.webhook)
		logger.Info("billing webhook mounted at /webhooks/billing")
	}
	// GET /auth/connectors is a PUBLIC, pre-auth endpoint that returns the list
	// of configured social-login providers (e.g. ["github"]). The SPA fetches
	// it before any session exists so the Login/Signup pages can show only the
	// buttons for providers that are actually configured. The response carries
	// NO org data: only provider names that came from a server-controlled env
	// var. It is mounted OUTSIDE the session middleware so no cookie is needed.
	// The endpoint is intentionally minimal: {"connectors":["github"]} or
	// {"connectors":[]} when none are configured.
	mux.HandleFunc("GET /auth/connectors", newAuthConnectorsHandler(caps))

	// The identity resolve endpoint is an INTERNAL machine-to-machine endpoint,
	// bearer-gated by a shared secret. It is mounted OUTSIDE the session
	// middleware (no browser session involved) and OUTSIDE the dev/prod
	// conditional (the same binary serves both). The token is read from the
	// environment; if unset, the endpoint is not mounted and a warning is logged.
	// The token value is never logged.
	if token := os.Getenv("MITOS_IDENTITY_RESOLVE_TOKEN"); token != "" {
		mux.Handle("POST /internal/identity/resolve", saas.NewIdentityResolveHandler(accounts, token, logger))
		logger.Info("identity resolve endpoint mounted")
		mux.Handle("POST /internal/session/resolve", saas.NewSessionResolveHandler(sessionSvc, token, logger))
		logger.Info("session resolve endpoint mounted")
	} else {
		logger.Warn("MITOS_IDENTITY_RESOLVE_TOKEN unset; POST /internal/identity/resolve not mounted")
		logger.Warn("MITOS_IDENTITY_RESOLVE_TOKEN unset; POST /internal/session/resolve not mounted")
	}
	// The approve-signup endpoint is an INTERNAL machine-to-machine endpoint,
	// bearer-gated by a dedicated shared secret (MITOS_CONSOLE_APPROVE_SIGNUP_TOKEN).
	// It canonicalizes the email, adds an allowlist row (idempotent), and sends the
	// "you are in" email. Mounted OUTSIDE the session middleware. The token value is
	// never logged. When unset, the endpoint is not mounted and a warning is logged.
	if approveToken := os.Getenv("MITOS_CONSOLE_APPROVE_SIGNUP_TOKEN"); approveToken != "" {
		mux.Handle("POST /internal/approve-signup", onboarding.NewApproveSignupHandler(
			allowlist,
			buildEmailSender(logger),
			approveToken,
			logger,
		))
		logger.Info("approve-signup endpoint mounted")
	} else {
		logger.Warn("MITOS_CONSOLE_APPROVE_SIGNUP_TOKEN unset; POST /internal/approve-signup not mounted")
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	logger.Info("console listening", "addr", *addr, "dev", *dev)
	srv := &http.Server{Addr: *addr, Handler: mux}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// sessionStoreAdapter bridges saas.Sessions to the console.SessionLister
// seam. It translates saas.Session to console.SessionRecord so the console
// package does not import the production store directly.
type sessionStoreAdapter struct{ s saas.Sessions }

func (a sessionStoreAdapter) ListByAccount(accountID string) []console.SessionRecord {
	recs := a.s.ListByAccount(accountID)
	out := make([]console.SessionRecord, len(recs))
	for i, r := range recs {
		out[i] = console.SessionRecord{
			ID:        r.ID,
			AccountID: r.AccountID,
			Label:     r.Label,
			CreatedAt: r.CreatedAt,
		}
	}
	return out
}

func (a sessionStoreAdapter) Revoke(accountID, sessionID string) error {
	return a.s.Revoke(accountID, sessionID)
}

func (a sessionStoreAdapter) RevokeAll(accountID string) { a.s.RevokeAll(accountID) }

// oidcScopes returns the OAuth scopes the console requests beyond openid (which
// NewProvider always adds). SignIn rejects any identity without a verified email,
// so the email scope MUST be requested or every login is rejected; default to
// "email profile". MITOS_CONSOLE_OIDC_SCOPES (space-separated) overrides for IdPs
// that name these differently.
func oidcScopes() []string {
	if v := strings.TrimSpace(os.Getenv("MITOS_CONSOLE_OIDC_SCOPES")); v != "" {
		return strings.Fields(v)
	}
	return []string{"email", "profile"}
}

// mountAuth discovers the OIDC issuer and mounts /auth/login, /auth/callback,
// and /auth/logout. The LoginManager issues sessions into the SAME store the
// SessionMiddleware reads. If issuer discovery fails the console still serves
// (the operator can fix the issuer and restart); login is simply unavailable.
func mountAuth(mux *http.ServeMux, logger *slog.Logger, accounts *saas.AccountService, store saas.Sessions, issuer string) {
	verifier, exch, err := oidcauth.NewProvider(context.Background(), oidcauth.ProviderConfig{
		IssuerURL:    issuer,
		ClientID:     os.Getenv("MITOS_CONSOLE_OIDC_CLIENT_ID"),
		ClientSecret: os.Getenv("MITOS_CONSOLE_OIDC_CLIENT_SECRET"),
		RedirectURL:  os.Getenv("MITOS_CONSOLE_OIDC_REDIRECT_URL"),
		Scopes:       oidcScopes(),
	})
	if err != nil {
		logger.Error("oidc provider discovery failed; /auth not mounted", "issuer", issuer, "err", err.Error())
		return
	}
	lm := saas.NewLoginManager(verifier, accounts, store, newSessionToken)
	ah := oidcauth.NewHandlers(oidcauth.Config{
		Exchanger:          exch,
		Login:              lm,
		CookieName:         console.SessionCookieName,
		RedirectAfterLogin: "/",
		Secure:             true,
	})
	mux.HandleFunc("/auth/login", ah.Login)
	mux.HandleFunc("/auth/callback", ah.Callback)
	mux.HandleFunc("/auth/logout", ah.Logout)
	logger.Info("oidc login flow mounted", "issuer", issuer)
}

// newSessionToken mints an opaque session token. It is stored hashed by the
// SessionStore; the raw value is delivered to the browser as the session cookie.
func newSessionToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// devAuth is the LOCAL-ONLY dev shim: it reads the caller account and org from
// request headers and attaches them to the context the BFF reads. This trusts
// the client and is ONLY for local smoke testing. The real auth (the #210
// gateway / session) verifies a session and attaches a context the client cannot
// forge. The shim never logs a header value beyond the non-secret ids.
func devAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acct := r.Header.Get("X-Console-Account")
		org := r.Header.Get("X-Console-Org")
		ctx := console.WithCaller(r.Context(), acct, org)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// indexLinks are the BFF endpoints the minimal index lists. The index is a
// static shell proving the BFF wiring; it is NOT the SPA (the documented
// follow-up). It renders no secret.
var indexLinks = []string{
	"/console/keys",
	"/console/usage",
	"/console/billing",
	"/console/sandboxes",
	"/console/members",
	"/console/audit",
	"/console/templates",
}

// indexTmpl renders the minimal index from the link list. It is built with a
// plain quoted string (no raw string literal) so the HTML stays trivially
// reviewable.
var indexTmpl = template.Must(template.New("index").Parse(
	"<!doctype html><html><head><title>Mitos console</title></head><body>" +
		"<h1>Mitos console (dev)</h1>" +
		"<p>This is the minimal wiring proof. The full SPA is a documented follow-up " +
		"(docs/saas/console.md). Set the dev headers and call the BFF directly.</p>" +
		"<ul>{{range .Links}}<li><a href=\"{{.}}\">{{.}}</a></li>{{end}}</ul>" +
		"</body></html>"))

func indexPage(logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := indexTmpl.Execute(w, struct{ Links []string }{Links: indexLinks}); err != nil {
			logger.Error("render index", "err", err.Error())
		}
	}
}

// parseAutoAllowDomains splits raw on commas, trims spaces, lowercases, and drops
// empty entries. An empty input returns the default ["mitos.run"] so a mitos.run
// address never requires a manual approval in a fresh deployment. The domains are
// not secret and are not logged at startup.
func parseAutoAllowDomains(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return []string{"mitos.run"}
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if d := strings.ToLower(strings.TrimSpace(p)); d != "" {
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return []string{"mitos.run"}
	}
	return out
}
