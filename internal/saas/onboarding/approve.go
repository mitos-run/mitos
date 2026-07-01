package onboarding

import (
	"crypto/hmac"
	"encoding/json"
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

	canonical, valid := normalizeEmail(req.Email)
	if !valid {
		http.Error(w, "a valid email address is required", http.StatusBadRequest)
		return
	}

	if err := h.allowlist.Add(r.Context(), canonical, req.Note, h.now()); err != nil {
		h.log.Error("approve signup: add to allowlist", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := h.email.SendApproved(r.Context(), canonical); err != nil {
		h.log.Error("approve signup: send approved email", "err", err)
		// Generic message: no email address or token is included.
		http.Error(w, "approval email could not be sent; the allowlist entry was recorded and you may retry", http.StatusInternalServerError)
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
