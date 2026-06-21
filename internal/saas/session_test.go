package saas

import (
	"context"
	"errors"
	"testing"
)

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
