package saas

import (
	"crypto/hmac"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

type sessionResolveHandler struct {
	sessions *SessionService
	token    string
	log      *slog.Logger
}

// NewSessionResolveHandler returns an http.Handler that serves
// POST /internal/session/resolve. It resolves the raw value of a mitos_session
// cookie to an account ID, a primary org ID, and the full org list.
//
// The bearer token is compared in constant time and is never logged.
// The session value is never logged. Internal errors are wrapped with %w but
// only a terse message is returned to the caller.
// If log is nil, a discard logger is used.
func NewSessionResolveHandler(sessions *SessionService, token string, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &sessionResolveHandler{sessions: sessions, token: token, log: log}
}

func (h *sessionResolveHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Constant-time bearer gate; the token is never logged.
	presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if h.token == "" || !ok || !hmac.Equal([]byte(presented), []byte(h.token)) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxResolveBody))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var req struct {
		Session string `json:"session"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if req.Session == "" {
		http.Error(w, "session is required", http.StatusBadRequest)
		return
	}

	// The session value is never passed to the logger.
	acct, orgs, err := h.sessions.Resolve(r.Context(), req.Session)
	if err != nil {
		if errors.Is(err, ErrSessionInvalid) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.log.Error("session resolve: resolve session", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Pick the personal org as orgId; fall back to the first org in the list.
	orgID := ""
	if len(orgs) > 0 {
		orgID = orgs[0].ID
		for _, org := range orgs {
			if org.Personal {
				orgID = org.ID
				break
			}
		}
	}

	type orgEntry struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	entries := make([]orgEntry, len(orgs))
	for i, org := range orgs {
		entries[i] = orgEntry{ID: org.ID, Name: org.Name}
	}

	// Log count only; the session value and account identity are never logged.
	h.log.Info("session resolved", "orgs", len(orgs))

	resp, err := json.Marshal(struct {
		AccountID string     `json:"accountId"`
		OrgID     string     `json:"orgId"`
		Orgs      []orgEntry `json:"orgs"`
	}{
		AccountID: acct.ID,
		OrgID:     orgID,
		Orgs:      entries,
	})
	if err != nil {
		h.log.Error("session resolve: marshal response", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(resp) //nolint:errcheck // write to http.ResponseWriter; error is handled by transport
}
