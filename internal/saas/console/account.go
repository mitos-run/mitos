package console

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"mitos.run/mitos/internal/apierr"
	"mitos.run/mitos/internal/saas"
)

// SessionRecord is the console-local shape of one session record. It mirrors the
// fields the BFF needs from saas.Session but lives in this package so the seam
// interface (SessionLister) does not import the production SessionStore directly.
type SessionRecord struct {
	ID        string
	AccountID string
	Label     string
	CreatedAt time.Time
}

// SessionLister is the narrow seam the account-sessions endpoints read. It is
// satisfied by *saas.SessionStore (via a thin adapter in cmd/console) and by
// MemSessionLister in tests. Every method MUST be account-scoped: ListByAccount
// returns only the named account's sessions; Revoke refuses a session that
// belongs to a different account.
type SessionLister interface {
	// ListByAccount returns all sessions for accountID, account-scoped: sessions
	// belonging to any other account are never returned.
	ListByAccount(accountID string) []SessionRecord
	// Revoke removes the session identified by sessionID from accountID's session
	// set. Returns ErrNotFound (the saas package error) if the session does not
	// exist or belongs to a different account.
	Revoke(accountID, sessionID string) error
	// RevokeAll removes every session belonging to accountID. Sessions belonging
	// to other accounts are not affected.
	RevokeAll(accountID string)
}

// MemSessionLister is the in-memory tested default for SessionLister. It stores
// sessions per account and enforces account isolation on every access.
type MemSessionLister struct {
	mu   sync.Mutex
	recs map[string]SessionRecord // session id -> record
}

// NewMemSessionLister returns an empty in-memory session lister.
func NewMemSessionLister() *MemSessionLister {
	return &MemSessionLister{recs: map[string]SessionRecord{}}
}

// Add stores a session record and returns its ID (convenience for test setup).
func (m *MemSessionLister) Add(r SessionRecord) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recs[r.ID] = r
	return r.ID
}

// ListByAccount returns all sessions for accountID. The list is never empty but
// may contain zero elements; sessions for other accounts are never included.
func (m *MemSessionLister) ListByAccount(accountID string) []SessionRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []SessionRecord
	for _, r := range m.recs {
		if r.AccountID == accountID {
			out = append(out, r)
		}
	}
	return out
}

// Revoke removes the session if it exists and belongs to accountID. Returns
// saas.ErrNotFound if the session is absent or belongs to a different account.
func (m *MemSessionLister) Revoke(accountID, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.recs[sessionID]
	if !ok || r.AccountID != accountID {
		return saas.ErrNotFound
	}
	delete(m.recs, sessionID)
	return nil
}

// RevokeAll removes all sessions belonging to accountID and leaves every other
// account's sessions intact.
func (m *MemSessionLister) RevokeAll(accountID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, r := range m.recs {
		if r.AccountID == accountID {
			delete(m.recs, id)
		}
	}
}

// AccountView is the console-safe shape of an account's profile. It carries no
// secret and no hash; it is safe to return to the account owner.
type AccountView struct {
	AccountID   string       `json:"account_id"`
	Email       string       `json:"email"`
	DisplayName string       `json:"display_name"`
	Timezone    string       `json:"timezone"`
	Locale      string       `json:"locale"`
	Memberships []MemberView `json:"memberships"`
}

// SessionView is the console-safe shape of one session. Current is true when
// the session is the one the caller is currently authenticated through; because
// the console BFF does not (yet) surface the active session id, it always
// defaults to false in this slice. The flag is reserved for the follow-up that
// threads the session id through the auth middleware.
type SessionView struct {
	ID        string    `json:"id"`
	Label     string    `json:"label"`
	CreatedAt time.Time `json:"created_at"`
	Current   bool      `json:"current"`
}

// accountView builds an AccountView from a saas.Account and its memberships,
// joining each membership's org HomeRegion from orgs (keyed by org id). orgs
// is best-effort: a membership whose org is missing from the map (a lookup
// failure upstream) simply gets an empty HomeRegion rather than failing the
// whole view.
func accountView(acct saas.Account, mems []saas.Membership, orgs map[string]saas.Organization) AccountView {
	mv := make([]MemberView, 0, len(mems))
	for _, m := range mems {
		mv = append(mv, MemberView{
			AccountID:  m.AccountID,
			OrgID:      m.OrgID,
			Role:       m.Role,
			CreatedAt:  m.CreatedAt,
			HomeRegion: orgs[m.OrgID].HomeRegion,
		})
	}
	return AccountView{
		AccountID:   acct.ID,
		Email:       acct.Email,
		DisplayName: acct.DisplayName,
		Timezone:    acct.Timezone,
		Locale:      acct.Locale,
		Memberships: mv,
	}
}

// handleGetAccount returns the caller's profile and memberships. The account id
// is always taken from the request context; it is never read from the path,
// query, or body, which is the account-isolation guarantee.
func (c *Console) handleGetAccount(w http.ResponseWriter, r *http.Request) {
	accountID, _, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	acct, mems, err := c.deps.Accounts.Profile(r.Context(), accountID)
	if err != nil {
		c.failAccount(w, err, "the account profile could not be read")
		return
	}
	// Resolve orgs (issue #712's HomeRegion join) from the memberships already
	// read above, so the account view does not re-list memberships.
	orgs := c.deps.Accounts.OrganizationsFor(r.Context(), mems)
	writeJSON(w, http.StatusOK, accountView(acct, mems, orgs))
}

// updateProfileRequest is the PATCH /console/account body. Only non-empty fields
// are applied; an empty field leaves the stored value unchanged.
type updateProfileRequest struct {
	DisplayName string `json:"display_name"`
	Timezone    string `json:"timezone"`
	Locale      string `json:"locale"`
}

// handlePatchAccount applies a partial update to the caller's own profile and
// returns the updated view. The account id is always from context.
func (c *Console) handlePatchAccount(w http.ResponseWriter, r *http.Request) {
	accountID, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	var req updateProfileRequest
	if err := decodeBody(r, &req); err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidJSON).
			WithCause("the profile-update body is not valid JSON"))
		return
	}
	updated, err := c.deps.Accounts.UpdateProfile(r.Context(), accountID, saas.ProfileUpdate{
		DisplayName: req.DisplayName,
		Timezone:    req.Timezone,
		Locale:      req.Locale,
	})
	if err != nil {
		c.failAccount(w, err, "the account profile could not be updated")
		return
	}
	// Re-read memberships so the returned view is complete.
	_, mems, err := c.deps.Accounts.Profile(r.Context(), accountID)
	if err != nil {
		c.failAccount(w, err, "the account profile could not be read after update")
		return
	}
	// Target is deliberately empty: the actor IS the subject of a profile
	// update, so a Target duplicating ActorID adds nothing.
	c.audit(r.Context(), AuditEvent{
		OrgID:      orgID,
		ActorID:    accountID,
		Action:     "profile.update",
		TargetType: "profile",
		Detail:     "updated account profile",
		At:         c.deps.Now(),
	})
	orgs := c.deps.Accounts.OrganizationsFor(r.Context(), mems)
	writeJSON(w, http.StatusOK, accountView(updated, mems, orgs))
}

// handleListSessions returns the caller's sessions. The account id is from
// context only; sessions belonging to other accounts are never included.
func (c *Console) handleListSessions(w http.ResponseWriter, r *http.Request) {
	accountID, _, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	recs := c.deps.Sessions.ListByAccount(accountID)
	out := make([]SessionView, 0, len(recs))
	for _, rec := range recs {
		out = append(out, SessionView{
			ID:        rec.ID,
			Label:     rec.Label,
			CreatedAt: rec.CreatedAt,
			Current:   false,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

// handleRevokeSession revokes a single session belonging to the caller. The
// account id is from context only; an attempt to revoke another account's
// session returns 404 (the session is indistinguishable from not found).
func (c *Console) handleRevokeSession(w http.ResponseWriter, r *http.Request) {
	accountID, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	sessionID := r.PathValue("id")
	// Resolve the session's own label before revoking it, best-effort, so the
	// audit event's TargetName is more legible than the bare session id.
	var targetName string
	for _, s := range c.deps.Sessions.ListByAccount(accountID) {
		if s.ID == sessionID {
			targetName = s.Label
			break
		}
	}
	if err := c.deps.Sessions.Revoke(accountID, sessionID); err != nil {
		if errors.Is(err, saas.ErrNotFound) {
			apierr.Encode(w, apierr.Get(apierr.CodeNotFound).
				WithCause("the session does not exist or does not belong to this account"))
			return
		}
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).
			WithCause("the session could not be revoked"))
		return
	}
	c.audit(r.Context(), AuditEvent{
		OrgID:      orgID,
		ActorID:    accountID,
		Action:     "session.revoke",
		Target:     sessionID,
		TargetType: "session",
		TargetName: targetName,
		Detail:     "revoked session " + sessionID,
		At:         c.deps.Now(),
	})
	writeJSON(w, http.StatusOK, map[string]any{"revoked": sessionID})
}

// handleRevokeAllSessions revokes every session belonging to the caller. It
// never touches sessions that belong to other accounts.
func (c *Console) handleRevokeAllSessions(w http.ResponseWriter, r *http.Request) {
	accountID, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	c.deps.Sessions.RevokeAll(accountID)
	// Target is deliberately empty: this is a bulk action on the caller's own
	// sessions, so there is no single target id to duplicate against ActorID.
	c.audit(r.Context(), AuditEvent{
		OrgID:      orgID,
		ActorID:    accountID,
		Action:     "session.revoke_all",
		TargetType: "session",
		Detail:     "revoked all sessions",
		At:         c.deps.Now(),
	})
	writeJSON(w, http.StatusOK, map[string]any{"revoked_all": true})
}

// noopSessionLister is the zero-value SessionLister: returns no sessions, and
// Revoke always returns ErrNotFound. It is the fallback when Deps.Sessions is
// nil so the BFF is safe to instantiate without a real session store.
type noopSessionLister struct{}

func (noopSessionLister) ListByAccount(_ string) []SessionRecord { return nil }
func (noopSessionLister) Revoke(_, _ string) error               { return saas.ErrNotFound }
func (noopSessionLister) RevokeAll(_ string)                     {}

// Compile-time interface checks.
var _ SessionLister = (*MemSessionLister)(nil)
var _ SessionLister = noopSessionLister{}

// currentSessionKey is an unexported context key for threading the active
// session id through the auth middleware so the sessions list can mark it
// current. The gateway sets this alongside WithCaller; the follow-up that wires
// the real session id can use WithCurrentSession below.
type currentSessionKey struct{}

// WithCurrentSession returns a context carrying the active session id. This is
// called by the session middleware (the follow-up wiring) so the sessions list
// handler can mark the caller's current session.
func WithCurrentSession(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, currentSessionKey{}, sessionID)
}
