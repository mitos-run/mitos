package saas

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

const maxResolveBody = 1 << 16 // 64 KiB

type identityResolveHandler struct {
	accounts *AccountService
	token    string
	log      *slog.Logger
}

// NewIdentityResolveHandler returns an http.Handler that serves
// POST /internal/identity/resolve. It resolves an email address to an account
// ID and the org IDs that account belongs to.
//
// The token is a bearer credential compared in constant time; it is never
// logged or placed in error messages.
//
// Email-to-org mappings are never logged; only counts are written to the log.
// If log is nil, a discard logger is used.
func NewIdentityResolveHandler(accounts *AccountService, token string, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &identityResolveHandler{accounts: accounts, token: token, log: log}
}

func (h *identityResolveHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Constant-time bearer gate; the token is never logged.
	presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if h.token == "" || !ok || subtle.ConstantTimeCompare([]byte(presented), []byte(h.token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxResolveBody))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var req struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if req.Email == "" {
		http.Error(w, "email is required", http.StatusBadRequest)
		return
	}

	acct, err := h.accounts.FindOrCreateByEmail(r.Context(), req.Email)
	if err != nil {
		h.log.Error("identity resolve: find or create account", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	orgs, err := h.accounts.Organizations(r.Context(), acct.ID)
	if err != nil {
		h.log.Error("identity resolve: list organizations", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Log counts only; never log email-to-org mapping.
	h.log.Info("identity resolved", "orgs", len(orgs))

	orgIDs := make([]string, len(orgs))
	for i, org := range orgs {
		orgIDs[i] = org.ID
	}

	resp, err := json.Marshal(struct {
		AccountID string   `json:"accountId"`
		OrgIDs    []string `json:"orgIds"`
	}{
		AccountID: acct.ID,
		OrgIDs:    orgIDs,
	})
	if err != nil {
		h.log.Error("identity resolve: marshal response", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(resp) //nolint:errcheck // write to http.ResponseWriter; error is handled by transport
}
