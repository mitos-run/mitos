package onboarding

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"

	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/console"
)

// maxSignupBody bounds the SignUp request body to defeat a slow-loris / oversize
// body abuse vector on this PUBLIC endpoint.
const maxSignupBody = 1 << 14 // 16 KiB

// HandlerOption configures a Handler.
type HandlerOption func(*Handler)

// WithHandlerSessions enables browser session issuance on a successful fresh
// verify. When set, a verify that provisions a new account (AlreadyDone false)
// mints a session token via sessions.IssueSession and sets the mitos_session
// cookie on the response so the new user is logged in without a second sign-in.
// The cookie flags match what the OIDC callback sets: HttpOnly, SameSite=Lax,
// and Secure driven by the same secure argument the OIDC handler receives.
// If sessions or newToken are nil this option is a no-op; the JSON response is
// still written normally. The raw session token is never logged.
func WithHandlerSessions(sessions saas.Sessions, newToken func() string, secure bool) HandlerOption {
	return func(h *Handler) {
		h.sessions = sessions
		h.newToken = newToken
		h.secure = secure
	}
}

// Handler serves the PUBLIC, unauthenticated onboarding endpoints:
//
//	POST /onboarding/signup   {"email": "..."} -> 202 (always the same shape)
//	POST /onboarding/verify   {"token": "..."} -> 200 with the account/org/key
//	GET  /onboarding/verify?token=... -> 200 (browser-friendly link target)
//
// SignUp NEVER reveals whether an email already has an account: it returns the
// same accepted response in every case so a probe cannot enumerate accounts. The
// raw verify token is delivered only to the user's inbox by the EmailSender; it
// is never returned by SignUp and never logged. Verify is the only place a token
// is accepted, and a bad / expired / used token yields a generic failure.
type Handler struct {
	svc      *Service
	log      *slog.Logger
	sessions saas.Sessions // optional; nil skips session cookie
	newToken func() string // optional; nil skips session cookie
	secure   bool
}

// NewHandler builds the onboarding HTTP handler over svc. If log is nil a
// discarding logger is used. The handler logs counts and non-secret status only,
// never an email or a token.
func NewHandler(svc *Service, log *slog.Logger, opts ...HandlerOption) *Handler {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	h := &Handler{svc: svc, log: log}
	for _, o := range opts {
		o(h)
	}
	return h
}

// Routes registers the onboarding routes on mux. It is the single place a binary
// mounts the public funnel so the route set stays consistent.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /onboarding/signup", h.signUp)
	mux.HandleFunc("POST /onboarding/verify", h.verify)
	mux.HandleFunc("GET /onboarding/verify", h.verify)
}

// signUp handles POST /onboarding/signup. It validates and normalizes the email,
// reads the optional use-case slug (uc), calls the service, and ALWAYS returns
// the same accepted response regardless of whether the email already exists, so
// no account enumeration is possible. An absent or invalid uc is silently
// dropped to ""; it never causes a request failure.
func (h *Handler) signUp(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
		UC    string `json:"uc"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return // decodeJSON already wrote the error
	}
	email, ok := normalizeEmail(req.Email)
	if !ok {
		// A malformed email is a client error and reveals nothing about accounts.
		writeJSONError(w, http.StatusBadRequest, "a valid email address is required")
		return
	}

	_, err := h.svc.SignUp(r.Context(), email, req.UC)
	// Account enumeration guard: a duplicate email (ErrConflict) returns the SAME
	// accepted response as a fresh signup. Any OTHER error is a genuine server
	// fault and is surfaced (without the email) so it is not silently swallowed.
	if err != nil && !errors.Is(err, saas.ErrConflict) {
		h.log.Error("onboarding signup", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "could not start onboarding; please try again")
		return
	}
	h.log.Info("onboarding signup accepted")

	// Uniform body: never leak whether a verification email was actually sent.
	writeJSON(w, http.StatusAccepted, map[string]string{
		"status":  "accepted",
		"message": "if that address can sign up, a verification email is on its way",
	})
}

// verify handles POST/GET /onboarding/verify. It reads the token from the JSON
// body (POST) or the query string (GET, the email-link target), calls the
// service, and returns the provisioned account/org and the one-time first key on
// success. A bad / expired / used token yields a generic failure that reveals
// nothing about the token.
func (h *Handler) verify(w http.ResponseWriter, r *http.Request) {
	token := ""
	if r.Method == http.MethodGet {
		token = r.URL.Query().Get("token")
	} else {
		var req struct {
			Token string `json:"token"`
		}
		if err := decodeJSON(w, r, &req); err != nil {
			return
		}
		token = req.Token
	}
	token = strings.TrimSpace(token)
	if token == "" {
		writeJSONError(w, http.StatusBadRequest, "a verification token is required")
		return
	}

	res, err := h.svc.Verify(r.Context(), token)
	if err != nil {
		// All token-resolution failures (invalid, expired, waitlist) collapse to a
		// single generic response so a probe cannot tell them apart. A genuine
		// downstream provisioning fault is a 500. The error is logged WITHOUT the
		// token (the service errors never carry it).
		if errors.Is(err, ErrTokenInvalid) || errors.Is(err, ErrTokenExpired) || errors.Is(err, ErrWaitlistMode) {
			h.log.Info("onboarding verify rejected", "reason", classifyVerifyErr(err))
			writeJSONError(w, http.StatusBadRequest, "this verification link is invalid or has expired")
			return
		}
		h.log.Error("onboarding verify", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "could not complete verification; please try again")
		return
	}

	// A not-allowed email is on the waitlist: provision nothing, mint no session,
	// return no key. The response carries only the waitlisted flag; it never leaks
	// an account id or email (the result has none).
	if res.Waitlisted {
		writeJSON(w, http.StatusOK, map[string]any{"waitlisted": true})
		return
	}

	// Mint a browser session for a freshly provisioned account so the new user
	// lands in the console without a second sign-in. The raw token is never
	// logged. The cookie is set before WriteHeader so the header is flushed
	// together with the JSON body. For an idempotent re-verify (AlreadyDone),
	// no new session is issued: the existing session (if any) remains valid.
	if !res.AlreadyDone && h.sessions != nil && h.newToken != nil {
		sessToken := h.newToken()
		h.sessions.IssueSession(res.Account.ID, sessToken, "browser")
		http.SetCookie(w, &http.Cookie{
			Name:     console.SessionCookieName,
			Value:    sessToken,
			Path:     "/",
			HttpOnly: true,
			Secure:   h.secure,
			SameSite: http.SameSiteLaxMode,
		})
	}

	// The raw first key is shown EXACTLY ONCE here (empty on an idempotent
	// re-verify). The account email is the caller's own, returned to confirm.
	// useCase is always included (empty string when none was provided at signup).
	out := map[string]any{
		"accountId":   res.Account.ID,
		"orgId":       res.Org.ID,
		"email":       res.Account.Email,
		"alreadyDone": res.AlreadyDone,
		"useCase":     res.UseCase,
	}
	if res.FirstKey.RawKey != "" {
		out["apiKey"] = res.FirstKey.RawKey
		out["apiKeyId"] = res.FirstKey.Record.ID
	}
	writeJSON(w, http.StatusOK, out)
}

// classifyVerifyErr maps a verify error to a short non-secret reason for the log.
func classifyVerifyErr(err error) string {
	switch {
	case errors.Is(err, ErrTokenExpired):
		return "expired"
	case errors.Is(err, ErrWaitlistMode):
		return "waitlist"
	default:
		return "invalid"
	}
}

// normalizeEmail trims, lowercases, and validates an email address. It returns
// the normalized address and whether it is structurally valid. Normalization
// (lowercasing) makes the no-enumeration guarantee robust: Foo@x and foo@x map to
// the same stored account.
func normalizeEmail(raw string) (string, bool) {
	e := strings.TrimSpace(raw)
	if e == "" || len(e) > 254 {
		return "", false
	}
	addr, err := mail.ParseAddress(e)
	if err != nil {
		return "", false
	}
	// Reject a display-name form ("Foo <foo@x>"); we want the bare address.
	if addr.Name != "" {
		return "", false
	}
	at := strings.LastIndex(addr.Address, "@")
	if at <= 0 || at == len(addr.Address)-1 {
		return "", false
	}
	return strings.ToLower(addr.Address), true
}

// decodeJSON reads a bounded JSON body into v, rejecting unknown fields and
// oversize bodies. On error it writes a 400 and returns the error so the caller
// stops.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxSignupBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
