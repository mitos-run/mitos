package console

import (
	"net/http"

	"mitos.run/mitos/internal/apierr"
	"mitos.run/mitos/internal/saas"
)

// SessionCookieName is the legacy (unprefixed) browser session cookie name. It
// stays the fallback for plain-http self-host and dev, where the __Host- prefix
// cannot be used because the browser only honors that prefix on a Secure cookie.
const SessionCookieName = "mitos_session"

// HostPrefixedSessionCookieName is the hardened session cookie name. The __Host-
// prefix makes the browser pin the cookie to the exact host with Path=/ and no
// Domain attribute, so a compromised sibling subdomain cannot set or overwrite
// it (cookie tossing). This matches the expose subsystem's __Host-mitos_expose
// standard. Writers use it only on a Secure connection (issue #733, item 1).
const HostPrefixedSessionCookieName = "__Host-mitos_session"

// SessionCookieNameFor returns the cookie name a writer should set on a
// connection whose transport security is `secure`: the hardened __Host- name
// when secure, the legacy name otherwise.
func SessionCookieNameFor(secure bool) string {
	if secure {
		return HostPrefixedSessionCookieName
	}
	return SessionCookieName
}

// ReadSessionCookie returns the session token from r, preferring the hardened
// __Host- cookie and falling back to the legacy name so a session issued before
// the rollout still resolves. It returns "" when neither cookie is present.
func ReadSessionCookie(r *http.Request) string {
	if c, err := r.Cookie(HostPrefixedSessionCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	return ""
}

// sessionUnauthorized is the console's 401 for a request with no or an invalid
// browser session. It reuses the generic unauthorized code but replaces the
// catalogue message and remediation, which are written for the per-sandbox
// bearer token ("the <name>-sandbox-token Secret") and are nonsense on a console
// endpoint. The remediation points at the login the console actually uses, so a
// human or agent that reads it takes the action that resolves the failure (the
// #28 LLM-legible error rule). The caller adds the specific cause.
func sessionUnauthorized() apierr.Error {
	return apierr.Get(apierr.CodeUnauthorized).
		WithMessage("the request is not authenticated: no valid console session").
		WithRemediation("Sign in at /login to start a session; the browser then sends the mitos_session cookie on every console request automatically.")
}

// SessionMiddleware is the PRODUCTION session-auth layer the BFF runs behind,
// replacing cmd/console's -dev header shim. It reads the session cookie,
// resolves it to an account and that account's organizations via the
// SessionService, and attaches the caller (account + active org) with
// WithCaller so every downstream endpoint is org-scoped. A request with no or an
// invalid cookie is refused with 401 and never reaches the BFF.
//
// Active-org selection defaults to the first organization (the single-org
// self-host default). Multi-org selection (an org-switcher honoring a chosen org
// constrained to membership) is a documented follow-up.
func SessionMiddleware(sessions *saas.SessionService) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := ReadSessionCookie(r)
			if token == "" {
				apierr.Encode(w, sessionUnauthorized().
					WithCause("no session cookie is present"))
				return
			}
			acct, orgs, err := sessions.Resolve(r.Context(), token)
			if err != nil || len(orgs) == 0 {
				apierr.Encode(w, sessionUnauthorized().
					WithCause("the session is invalid or has no organization"))
				return
			}
			ctx := WithCaller(r.Context(), acct.ID, orgs[0].ID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
