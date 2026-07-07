package saas

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestSessionStoreResolveRejectsExpired asserts that with an absolute max-age
// configured, a session older than the max-age no longer resolves, so a leaked
// session token cannot stay valid forever (issue #733, item 2). A session inside
// the window still resolves.
func TestSessionStoreResolveRejectsExpired(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	now := base
	s := NewSessionStore(WithSessionMaxAge(24*time.Hour), withSessionClock(func() time.Time { return now }))
	s.IssueSession("acct-1", "tok", "browser")

	// Inside the window: resolves.
	now = base.Add(23 * time.Hour)
	if id, err := s.Resolve("tok"); err != nil || id != "acct-1" {
		t.Fatalf("in-window Resolve = (%q, %v), want (acct-1, nil)", id, err)
	}

	// Past the window: rejected as invalid.
	now = base.Add(25 * time.Hour)
	if _, err := s.Resolve("tok"); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("expired Resolve err = %v, want ErrSessionInvalid", err)
	}
}

// TestSessionStoreDefaultMaxAgeEnforced asserts the default constructor enforces
// an absolute max-age (self-host gets expiry without extra wiring).
func TestSessionStoreDefaultMaxAgeEnforced(t *testing.T) {
	if DefaultSessionMaxAge <= 0 {
		t.Fatalf("DefaultSessionMaxAge = %v, want a positive absolute ceiling", DefaultSessionMaxAge)
	}
	base := time.Unix(2_000_000, 0)
	now := base
	s := NewSessionStore(withSessionClock(func() time.Time { return now }))
	s.IssueSession("acct-1", "tok", "browser")
	now = base.Add(DefaultSessionMaxAge + time.Hour)
	if _, err := s.Resolve("tok"); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("past-default Resolve err = %v, want ErrSessionInvalid", err)
	}
}

// TestSessionResolvesToAccountAndOrgs asserts an issued session token resolves to
// its account and that account's orgs.
func TestSessionResolvesToAccountAndOrgs(t *testing.T) {
	store := NewMemStore()
	keys := NewKeyService(store)
	accts := NewAccountService(store, keys)
	acct, org, err := accts.SignUp(context.Background(), "user@example.com")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	sessions := NewSessionStore()
	sessions.Issue(acct.ID, "sess-token")
	svc := NewSessionService(sessions, accts)

	gotAcct, orgs, err := svc.Resolve(context.Background(), "sess-token")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if gotAcct.Email != "user@example.com" {
		t.Errorf("resolved email = %q, want user@example.com", gotAcct.Email)
	}
	if len(orgs) != 1 || orgs[0].ID != org.ID {
		t.Errorf("resolved orgs = %+v, want personal org %q", orgs, org.ID)
	}
}

// TestSessionRejectsForgedToken asserts an unknown token does not resolve.
func TestSessionRejectsForgedToken(t *testing.T) {
	sessions := NewSessionStore()
	svc := NewSessionService(sessions, NewAccountService(NewMemStore(), NewKeyService(NewMemStore())))
	if _, _, err := svc.Resolve(context.Background(), "never-issued"); !errors.Is(err, ErrSessionInvalid) {
		t.Errorf("Resolve(forged) err = %v, want ErrSessionInvalid", err)
	}
}

// TestSessionStoresTokenHashedNotInClear asserts the raw token is not held in the
// store.
func TestSessionStoresTokenHashedNotInClear(t *testing.T) {
	sessions := NewSessionStore()
	sessions.Issue("acct-1", "secret-session")
	for h := range sessions.byHash {
		if h == "secret-session" {
			t.Error("session store holds the raw token in the clear")
		}
	}
}

// TestSessionListAndRevoke verifies per-account session listing and revocation,
// including cross-account isolation and token invalidation on revoke.
func TestSessionListAndRevoke(t *testing.T) {
	store := NewSessionStore()
	id1 := store.IssueSession("acctA", "tokA1", "browser")
	_ = store.IssueSession("acctA", "tokA2", "cli")
	store.IssueSession("acctB", "tokB1", "browser")

	a := store.ListByAccount("acctA")
	if len(a) != 2 {
		t.Fatalf("acctA sessions = %d, want 2", len(a))
	}
	// acctA never sees acctB.
	for _, s := range a {
		if s.AccountID != "acctA" {
			t.Fatalf("leaked session for %s", s.AccountID)
		}
	}
	// Revoking a session invalidates its token.
	if err := store.Revoke("acctA", id1); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := store.Resolve("tokA1"); err == nil {
		t.Fatalf("revoked token must not resolve")
	}
	// The other session still resolves.
	if _, err := store.Resolve("tokA2"); err != nil {
		t.Fatalf("tokA2 should still resolve: %v", err)
	}
	// acctA cannot revoke acctB's session.
	bsel := store.ListByAccount("acctB")
	if err := store.Revoke("acctA", bsel[0].ID); err == nil {
		t.Fatalf("cross-account revoke must fail")
	}
}
