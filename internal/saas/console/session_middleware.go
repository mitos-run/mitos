package console

import (
	"net/http"

	"mitos.run/mitos/internal/apierr"
	"mitos.run/mitos/internal/saas"
)

// SessionCookieName is the browser session cookie the console sets after an OIDC
// login and reads on every subsequent request.
const SessionCookieName = "mitos_session"

// unauthorizedSession is the console-surface 401. The shared apierr catalogue
// entry for unauthorized carries the SANDBOX API remediation (the per-sandbox
// token Secret), which is the wrong next action on a console endpoint; here
// the remediation names the SPA sign-in path instead, per the
// remediation-matches-the-surface rule (issue #631). The message and
// remediation are identical for every refusal so the body never reveals
// whether a session existed; only the cause distinguishes what the caller
// already knows (whether it sent a cookie).
func unauthorizedSession(cause string) apierr.Error {
	return apierr.Error{
		Code:        string(apierr.CodeUnauthorized),
		Message:     "the console session is missing or invalid",
		Cause:       cause,
		Remediation: "Sign in at /login to establish a console session, then retry the request.",
		Status:      http.StatusUnauthorized,
	}
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
			cookie, err := r.Cookie(SessionCookieName)
			if err != nil || cookie.Value == "" {
				apierr.Encode(w, unauthorizedSession("no session cookie is present"))
				return
			}
			acct, orgs, err := sessions.Resolve(r.Context(), cookie.Value)
			if err != nil || len(orgs) == 0 {
				apierr.Encode(w, unauthorizedSession("the session is invalid or has no organization"))
				return
			}
			ctx := WithCaller(r.Context(), acct.ID, orgs[0].ID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
