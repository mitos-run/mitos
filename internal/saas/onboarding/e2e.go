package onboarding

import (
	"crypto/hmac"
	"net/http"
	"strings"
)

// E2EHandler serves GET /onboarding/e2e/token?email=<email>.
//
// It is a QA-ONLY endpoint: it is NEVER mounted in production. Four independent
// security gates enforce this at runtime (defense in depth):
//  1. MITOS_CONSOLE_E2E must be truthy (enforced by the caller; the handler is
//     never constructed or registered when the flag is off).
//  2. The caller must present the correct bearer token in constant time.
//  3. The email's domain must exactly match the configured allowlist domain.
//  4. The in-memory sink must have a recorded token for the email (set at signup).
//
// If all gates pass, the handler returns the most recent un-verified raw token
// recorded for that email by the MemE2ETokenSink. The bearer value and raw token
// are NEVER logged or echoed in error responses.
type E2EHandler struct {
	bearer       string
	domainSuffix string
	sink         E2ETokenSink
}

// NewE2EHandler returns an E2EHandler. bearer is the shared secret compared in
// constant time against the Authorization header; domainSuffix is the exact
// email domain that is allowed (e.g. "e2e.mitos.run"); sink is the in-memory
// token sink populated at signup time. The bearer value is never logged.
func NewE2EHandler(bearer, domainSuffix string, sink E2ETokenSink) *E2EHandler {
	return &E2EHandler{
		bearer:       bearer,
		domainSuffix: strings.ToLower(strings.TrimSpace(domainSuffix)),
		sink:         sink,
	}
}

// Routes registers the E2E token route on mux. It is ONLY called from
// mountOnboarding when MITOS_CONSOLE_E2E is truthy.
func (h *E2EHandler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /onboarding/e2e/token", h.getToken)
}

func (h *E2EHandler) getToken(w http.ResponseWriter, r *http.Request) {
	// Gate 2: constant-time bearer token check (Gate 1 is the flag, enforced by
	// the caller; this handler is never constructed when the flag is off).
	// The presented value is never logged. An empty configured bearer is treated
	// as misconfigured and always fails to prevent accidental open access.
	presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if h.bearer == "" || !ok || !hmac.Equal([]byte(presented), []byte(h.bearer)) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	email := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("email")))
	if email == "" {
		http.NotFound(w, r)
		return
	}

	// Gate 3: domain allowlist. Extract and compare the domain portion of the
	// email; any domain that is not an exact match returns 404 (not 403) so a
	// probe cannot distinguish "wrong domain" from "no token" for a given email.
	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		http.NotFound(w, r)
		return
	}
	domain := email[at+1:]
	if domain != h.domainSuffix {
		http.NotFound(w, r)
		return
	}

	// Gate 4: sink lookup. Returns 404 for unknown email or no token yet recorded.
	tok, ok := h.sink.Last(email)
	if !ok {
		http.NotFound(w, r)
		return
	}

	// The raw token is returned in the JSON body ONLY. It is not logged here.
	writeJSON(w, http.StatusOK, map[string]string{"token": tok})
}
