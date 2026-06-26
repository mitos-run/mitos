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

	"mitos.run/mitos/cmd/console/spa"
	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/billing"
	"mitos.run/mitos/internal/saas/console"
	"mitos.run/mitos/internal/saas/oidcauth"
	"mitos.run/mitos/internal/saas/pgstore"
)

func main() {
	addr := flag.String("addr", ":8090", "console listen address")
	dev := flag.Bool("dev", false, "enable the local dev auth shim (X-Console-Account / X-Console-Org headers); NEVER enable in production")
	databaseDSN := flag.String("database-dsn", "", "Postgres DSN for durable persistence (accounts, orgs, memberships, API keys). Falls back to the "+pgstore.EnvDSN+" env var. Empty means in-memory persistence (DEV ONLY). The value is a secret and is never logged.")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Durable Postgres when a DSN is configured (flag or MITOS_DATABASE_DSN),
	// in-memory otherwise (dev only). The DSN value is never logged. The real
	// billing service, control-plane live-sandbox query, and SandboxTemplate
	// lister remain documented follow-ups (docs/saas/console.md).
	store, closeStore, err := pgstore.ResolveStore(context.Background(), *databaseDSN, logger)
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
	bill := setupBilling(logger, statusStore)

	// sessionStore is created before console.New so it can be passed into
	// Deps.Sessions in the production branch. In dev mode it is unused (the
	// console's noopSessionLister default is fine for local smoke testing).
	sessionStore := saas.NewSessionStore()

	con := console.New(console.Deps{
		Accounts: accounts,
		// The usage store the console reads: the controller's internal usage API
		// (the SAME usage the collector recorded) when configured, in-memory in dev.
		Usage: buildUsageStore(logger),
		// The proof-snapshot and fork-tree sources: org-scoped cluster queries when
		// in a cluster, in-memory otherwise.
		Instruments: buildInstruments(logger),
		ForkTree:    buildForkTree(logger),
		Billing: console.BillingReader{
			Ledger: billing.NewMemCreditLedger(),
			Status: statusStore,
			Caps:   billing.NewMemSpendCapStore(),
			Rates:  billing.DefaultRates(),
		},
		// The active secret backend selected from config (kube / openbao), falling
		// back to in-memory in dev. Capabilities advertise the same providers.
		Secrets: buildSecretStore(logger, caps),
		// The live-sandbox control: the org-scoped cluster query when in a cluster,
		// in-memory otherwise. Shares the org→namespace boundary with secrets.
		Sandboxes: buildSandboxControl(logger),
		// The manage-subscription portal link (provider-neutral); nil keeps the
		// console's no-portal default (community edition).
		Portal: bill.portal,
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
		sessions := saas.NewSessionService(sessionStore, accounts)
		mux.Handle("/console/", console.SessionMiddleware(sessions)(con))
		mux.Handle("/", spa.Handler())
		if issuer := os.Getenv("MITOS_CONSOLE_OIDC_ISSUER"); issuer != "" {
			mountAuth(mux, logger, accounts, sessionStore, issuer)
		} else {
			logger.Warn("MITOS_CONSOLE_OIDC_ISSUER unset; /auth login flow not mounted (BFF requires a session)")
		}
	}
	// The billing webhook is PUBLIC by design: it is authenticated by the
	// provider's signature, not a session, so it is mounted OUTSIDE the session
	// middleware. It verifies the signature before touching any billing state.
	if bill.webhook != nil {
		mux.Handle("/webhooks/billing", bill.webhook)
		logger.Info("billing webhook mounted at /webhooks/billing")
	}
	// The identity resolve endpoint is an INTERNAL machine-to-machine endpoint,
	// bearer-gated by a shared secret. It is mounted OUTSIDE the session
	// middleware (no browser session involved) and OUTSIDE the dev/prod
	// conditional (the same binary serves both). The token is read from the
	// environment; if unset, the endpoint is not mounted and a warning is logged.
	// The token value is never logged.
	if token := os.Getenv("MITOS_IDENTITY_RESOLVE_TOKEN"); token != "" {
		mux.Handle("POST /internal/identity/resolve", saas.NewIdentityResolveHandler(accounts, token, logger))
		logger.Info("identity resolve endpoint mounted")
	} else {
		logger.Warn("MITOS_IDENTITY_RESOLVE_TOKEN unset; POST /internal/identity/resolve not mounted")
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

// sessionStoreAdapter bridges *saas.SessionStore to the console.SessionLister
// seam. It translates saas.Session to console.SessionRecord so the console
// package does not import the production store directly.
type sessionStoreAdapter struct{ s *saas.SessionStore }

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

// mountAuth discovers the OIDC issuer and mounts /auth/login, /auth/callback,
// and /auth/logout. The LoginManager issues sessions into the SAME store the
// SessionMiddleware reads. If issuer discovery fails the console still serves
// (the operator can fix the issuer and restart); login is simply unavailable.
func mountAuth(mux *http.ServeMux, logger *slog.Logger, accounts *saas.AccountService, store *saas.SessionStore, issuer string) {
	verifier, exch, err := oidcauth.NewProvider(context.Background(), oidcauth.ProviderConfig{
		IssuerURL:    issuer,
		ClientID:     os.Getenv("MITOS_CONSOLE_OIDC_CLIENT_ID"),
		ClientSecret: os.Getenv("MITOS_CONSOLE_OIDC_CLIENT_SECRET"),
		RedirectURL:  os.Getenv("MITOS_CONSOLE_OIDC_REDIRECT_URL"),
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
