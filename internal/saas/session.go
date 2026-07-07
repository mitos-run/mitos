package saas

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// ErrSessionInvalid is returned when a session token does not resolve to an
// account. It never reveals whether the token was malformed or simply unknown.
var ErrSessionInvalid = errors.New("saas: session token is invalid")

// Session is the non-secret record the store keeps per issued session. It
// carries no token value and no token hash; it is safe to return to the
// session owner.
type Session struct {
	ID        string
	AccountID string
	CreatedAt time.Time
	Label     string
}

// Sessions is the browser-session backend. The in-memory SessionStore and the
// durable pgstore.PgSessionStore both implement it.
type Sessions interface {
	IssueSession(accountID, token, label string) string
	Issue(accountID, token string)
	Resolve(token string) (string, error)
	ListByAccount(accountID string) []Session
	Revoke(accountID, sessionID string) error
	RevokeAll(accountID string)
}

// SessionStore maps an opaque session token to the account behind it. Like API
// keys, session tokens are stored hashed, never in the clear, and resolved in
// constant time. This is the seam the browser OAuth login flow (a documented
// follow-up) plugs into; the token-based CLI login in this slice issues a
// session token directly. The in-memory implementation is the tested default.
type SessionStore struct {
	mu       sync.RWMutex
	byHash   map[string]string    // session-token hash -> account id
	records  map[string]Session   // session id -> Session record
	hashByID map[string]string    // session id -> token hash
	createdH map[string]time.Time // session-token hash -> issue time (for max-age)
	nextID   uint64               // counter for deterministic, test-safe session ids
	maxAge   time.Duration        // absolute session lifetime; 0 disables expiry
	now      func() time.Time     // clock seam; defaults to time.Now
}

// DefaultSessionMaxAge is the absolute lifetime a session token is valid for
// unless overridden with WithSessionMaxAge. After this age a token no longer
// resolves, so a leaked token cannot stay valid indefinitely (issue #733).
const DefaultSessionMaxAge = 30 * 24 * time.Hour

// SessionStoreOption configures a SessionStore at construction.
type SessionStoreOption func(*SessionStore)

// WithSessionMaxAge sets the absolute session lifetime. A non-positive value
// disables expiry (a session then resolves until explicitly revoked).
func WithSessionMaxAge(d time.Duration) SessionStoreOption {
	return func(s *SessionStore) { s.maxAge = d }
}

// withSessionClock overrides the clock; used by tests to drive expiry
// deterministically.
func withSessionClock(fn func() time.Time) SessionStoreOption {
	return func(s *SessionStore) { s.now = fn }
}

// compile-time assertion that SessionStore satisfies Sessions.
var _ Sessions = (*SessionStore)(nil)

// NewSessionStore returns an empty in-memory session store. By default it
// enforces DefaultSessionMaxAge; pass WithSessionMaxAge to change or disable it.
func NewSessionStore(opts ...SessionStoreOption) *SessionStore {
	s := &SessionStore{
		byHash:   map[string]string{},
		records:  map[string]Session{},
		hashByID: map[string]string{},
		createdH: map[string]time.Time{},
		maxAge:   DefaultSessionMaxAge,
		now:      time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s
}

// IssueSession records that token authenticates accountID and attaches a
// human-readable label (e.g. "browser", "cli"). The raw token is hashed; the
// store never holds it in the clear. Returns the opaque session id.
func (s *SessionStore) IssueSession(accountID, token, label string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := fmt.Sprintf("sess-%d", s.nextID)
	h := hashSession(token)
	created := s.now()
	s.byHash[h] = accountID
	s.records[id] = Session{
		ID:        id,
		AccountID: accountID,
		CreatedAt: created,
		Label:     label,
	}
	s.hashByID[id] = h
	s.createdH[h] = created
	return id
}

// Issue records that token authenticates accountID. This is a backward-
// compatible wrapper around IssueSession with a default label so existing
// callers (e.g. oidc.go LoginManager) compile unchanged.
func (s *SessionStore) Issue(accountID, token string) {
	s.IssueSession(accountID, token, "session")
}

// Resolve returns the account id for a session token, or ErrSessionInvalid.
// The lookup is by exact hash, so a forged or revoked token simply fails to
// resolve; the constant-time compare guards the hash equality.
func (s *SessionStore) Resolve(token string) (string, error) {
	h := hashSession(token)
	s.mu.RLock()
	id, ok := s.byHash[h]
	created, hasCreated := s.createdH[h]
	s.mu.RUnlock()
	if !ok {
		return "", ErrSessionInvalid
	}
	// Defense in depth: confirm the stored key equals the recomputed hash in
	// constant time so the path is timing-independent.
	if subtle.ConstantTimeCompare([]byte(h), []byte(hashSession(token))) != 1 {
		return "", ErrSessionInvalid
	}
	// Absolute max-age: a session older than maxAge no longer resolves and is
	// lazily reaped so a leaked token cannot live forever (issue #733, item 2).
	if s.maxAge > 0 && hasCreated && s.now().Sub(created) > s.maxAge {
		s.expireByHash(h)
		return "", ErrSessionInvalid
	}
	return id, nil
}

// expireByHash deletes the session identified by its token hash. It is used to
// lazily reap a session that has passed its absolute max-age on Resolve.
func (s *SessionStore) expireByHash(h string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byHash, h)
	delete(s.createdH, h)
	for id, hh := range s.hashByID {
		if hh == h {
			delete(s.hashByID, id)
			delete(s.records, id)
			break
		}
	}
}

// ListByAccount returns all sessions for accountID, most-recent-first. It
// never returns sessions belonging to any other account.
func (s *SessionStore) ListByAccount(accountID string) []Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Session
	for _, rec := range s.records {
		if rec.AccountID == accountID {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

// Revoke removes the session identified by sessionID from accountID's session
// set and deletes its token hash so Resolve of that token now returns
// ErrSessionInvalid. If sessionID does not exist or belongs to a different
// account, ErrNotFound is returned; in that case no state is modified.
func (s *SessionStore) Revoke(accountID, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[sessionID]
	if !ok || rec.AccountID != accountID {
		return ErrNotFound
	}
	h := s.hashByID[sessionID]
	delete(s.byHash, h)
	delete(s.createdH, h)
	delete(s.hashByID, sessionID)
	delete(s.records, sessionID)
	return nil
}

// RevokeAll removes every session belonging to accountID and invalidates their
// tokens. Sessions belonging to other accounts are not affected.
func (s *SessionStore) RevokeAll(accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, rec := range s.records {
		if rec.AccountID != accountID {
			continue
		}
		h := s.hashByID[id]
		delete(s.byHash, h)
		delete(s.createdH, h)
		delete(s.hashByID, id)
		delete(s.records, id)
	}
}

// hashSession hashes a session token for at-rest storage. Sessions are not
// salted with the key pepper because they are short-lived and store-local; the
// sha256 is sufficient to avoid holding the raw token.
func hashSession(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// SessionService ties a Sessions store to an AccountService so a session token
// can be resolved to its account and that account's organizations. It is the
// object the CLI bridge (cmd/mitos) and the future web console (issue #214)
// use to back the token-based auth surface.
type SessionService struct {
	sessions Sessions
	accounts *AccountService
}

// NewSessionService builds a session service.
func NewSessionService(sessions Sessions, accounts *AccountService) *SessionService {
	return &SessionService{sessions: sessions, accounts: accounts}
}

// Resolve returns the account and its organizations for a session token.
func (s *SessionService) Resolve(ctx context.Context, token string) (Account, []Organization, error) {
	accountID, err := s.sessions.Resolve(token)
	if err != nil {
		return Account{}, nil, err
	}
	acct, err := s.accounts.store.GetAccount(ctx, accountID)
	if err != nil {
		return Account{}, nil, ErrSessionInvalid
	}
	orgs, err := s.accounts.Organizations(ctx, accountID)
	if err != nil {
		return Account{}, nil, err
	}
	return acct, orgs, nil
}

// AccountFor resolves just the account id behind a session token, for the key
// management verbs that then delegate to the membership-guarded AccountService.
func (s *SessionService) AccountFor(token string) (string, error) {
	return s.sessions.Resolve(token)
}

// Accounts exposes the underlying AccountService so a bridge can call the
// membership-guarded key verbs with the resolved account id.
func (s *SessionService) Accounts() *AccountService { return s.accounts }
