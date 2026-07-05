package onboarding

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// maxApproveBody bounds the approve-signup request body to limit oversize
// body abuse on this internal endpoint.
const maxApproveBody = 1 << 16 // 64 KiB

// approveSignupHandler serves POST /internal/approve-signup. It is an
// internal, machine-to-machine endpoint: it is never exposed to tenants.
type approveSignupHandler struct {
	allowlist Allowlist
	email     EmailSender
	token     string
	log       *slog.Logger
	now       func() time.Time
}

// NewApproveSignupHandler returns the internal, bearer-gated approve-signup
// endpoint. token is a shared secret compared in constant time and never
// logged. When log is nil a discarding logger is used.
func NewApproveSignupHandler(allowlist Allowlist, email EmailSender, token string, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &approveSignupHandler{
		allowlist: allowlist,
		email:     email,
		token:     token,
		log:       log,
		now:       time.Now,
	}
}

func (h *approveSignupHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Constant-time bearer gate; the token is never logged.
	presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if h.token == "" || !ok || !hmac.Equal([]byte(presented), []byte(h.token)) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxApproveBody))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var req struct {
		Email string `json:"email"`
		Note  string `json:"note"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	canonical, _, err := ApproveWaitlistEntry(r.Context(), h.allowlist, h.email, req.Email, req.Note, h.now())
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidEmail):
			http.Error(w, "a valid email address is required", http.StatusBadRequest)
		case errors.Is(err, errApproveAllowlistAdd):
			h.log.Error("approve signup: add to allowlist", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		default:
			h.log.Error("approve signup: send approved email", "err", err)
			// Generic message: no email address or token is included.
			http.Error(w, "approval email could not be sent; the allowlist entry was recorded and you may retry", http.StatusInternalServerError)
		}
		return
	}

	resp, merr := json.Marshal(map[string]string{"email": canonical})
	if merr != nil {
		h.log.Error("approve signup: marshal response", "err", merr)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp)
}

// errApproveAllowlistAdd wraps a failure to add the canonical email to the
// allowlist (as opposed to a failure to SEND the approved email, which
// happens after the allowlist row already landed); ApproveSignupHandler and
// the console instance-admin waitlist adapter both distinguish the two via
// errors.Is so they can log/report which step failed.
var errApproveAllowlistAdd = errors.New("onboarding: approve waitlist entry: add to allowlist")

// ApproveWaitlistEntry is the ONE mechanism that grants a waitlisted (or any
// not-yet-allowlisted) email access to provision: it idempotently adds the
// canonical form of rawEmail to allowlist, then sends the "you're in"
// notification through email to the delivery form of rawEmail (the address
// as typed, normalized, so a plus-tagged inbox on a non-Gmail provider still
// receives it). It does NOT create an account, an org, or an invitation:
// the recipient still completes signup/verify themselves once allowed
// through the gate (see docs/saas/onboarding.md). This is the SAME
// mechanism POST /internal/approve-signup uses (approveSignupHandler calls
// it directly) and the console instance-admin waitlist-approve endpoint
// reuses (via an adapter in cmd/console), so an operator approving through
// either surface produces an identical allowlist row and email.
//
// Returns the canonical email on success, and alreadyApproved true when
// canonical already held allowlist access BEFORE this call: in that case
// neither Add nor SendApproved is called again, so a second approval of the
// same email (an operator double-clicking Approve, or re-approving a
// waitlist row that still lists the email because entries are never
// removed) never re-sends the "you're in" notification. Returns
// ErrInvalidEmail if rawEmail cannot be reduced to a canonical identity; an
// error wrapping errApproveAllowlistAdd if the allowlist write failed
// (nothing was sent); or the raw error from email.SendApproved if the
// allowlist row was added but the notification could not be sent (the
// caller may retry: Add is idempotent).
func ApproveWaitlistEntry(ctx context.Context, allowlist Allowlist, email EmailSender, rawEmail, note string, now time.Time) (canonical string, alreadyApproved bool, err error) {
	// canonicalEmail folds plus-tags (all providers) and Gmail dots so the
	// allowlist row stored here matches the identity key the Verify gate reads.
	// An operator approving u.ser+x@gmail.com must produce the same row as a
	// signup whose Verify gate checks user@gmail.com.
	canonical, valid := canonicalEmail(rawEmail)
	if !valid {
		return "", false, ErrInvalidEmail
	}
	// The allowlist row is the canonical IDENTITY; the approval email is
	// DELIVERED to the address as typed (normalized), so a plus-tagged inbox
	// on a non-Gmail provider still receives it. Fall back to canonical if
	// the delivery form somehow fails to parse (canonical already validated
	// above).
	delivery, ok := normalizeEmail(rawEmail)
	if !ok {
		delivery = canonical
	}

	// Idempotency check: a lookup failure is treated as "not yet approved"
	// (fail OPEN) rather than blocking the approval outright on a read
	// hiccup; Add below is itself idempotent, so at worst this re-sends one
	// redundant notification instead of silently refusing a legitimate
	// approval.
	if already, lookupErr := allowlist.IsAllowed(ctx, canonical); lookupErr == nil && already {
		return canonical, true, nil
	}

	if err := allowlist.Add(ctx, canonical, note, now); err != nil {
		return "", false, fmt.Errorf("%w: %w", errApproveAllowlistAdd, err)
	}
	if err := email.SendApproved(ctx, delivery); err != nil {
		return "", false, fmt.Errorf("onboarding: approve waitlist entry: send approved email: %w", err)
	}
	return canonical, false, nil
}
