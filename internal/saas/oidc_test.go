package saas

import (
	"context"
	"errors"
	"testing"
)

// fakeVerifier is a test IDTokenVerifier: it maps a raw token string to fixed
// claims or an error, so the login logic is tested without a live OIDC provider.
type fakeVerifier struct {
	claims OIDCClaims
	err    error
}

func (f fakeVerifier) Verify(_ context.Context, _ string) (OIDCClaims, error) {
	return f.claims, f.err
}

func newLogin(t *testing.T, v IDTokenVerifier) (*LoginManager, *SessionService) {
	t.Helper()
	store := NewMemStore()
	keys := NewKeyService(store)
	accounts := NewAccountService(store, keys)
	sessions := NewSessionStore()
	seq := 0
	lm := NewLoginManager(v, accounts, sessions, func() string {
		seq++
		return "tok-" + string(rune('a'+seq))
	})
	return lm, NewSessionService(sessions, accounts)
}

// TestLoginFirstTimeCreatesAccountAndSession asserts that a first OIDC login with
// a verified email auto-creates the account (and its org, the self-host
// single-org default) and issues a session that resolves back to it.
func TestLoginFirstTimeCreatesAccountAndSession(t *testing.T) {
	lm, svc := newLogin(t, fakeVerifier{claims: OIDCClaims{Subject: "sub-1", Email: "dev@example.com", EmailVerified: true}})
	acct, token, err := lm.SignIn(context.Background(), "raw-id-token")
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	if acct.Email != "dev@example.com" {
		t.Errorf("account email = %q, want dev@example.com", acct.Email)
	}
	gotAcct, orgs, err := svc.Resolve(context.Background(), token)
	if err != nil {
		t.Fatalf("resolve issued session: %v", err)
	}
	if gotAcct.ID != acct.ID {
		t.Errorf("session resolved to %q, want %q", gotAcct.ID, acct.ID)
	}
	if len(orgs) != 1 {
		t.Errorf("first login should bind exactly one org, got %d", len(orgs))
	}
}

// TestLoginReturningUserReusesAccount asserts a second login with the same email
// reuses the existing account rather than creating a duplicate.
func TestLoginReturningUserReusesAccount(t *testing.T) {
	lm, _ := newLogin(t, fakeVerifier{claims: OIDCClaims{Subject: "sub-1", Email: "dev@example.com", EmailVerified: true}})
	first, _, err := lm.SignIn(context.Background(), "t1")
	if err != nil {
		t.Fatalf("first SignIn: %v", err)
	}
	second, _, err := lm.SignIn(context.Background(), "t2")
	if err != nil {
		t.Fatalf("second SignIn: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("returning login created a new account: %q != %q", first.ID, second.ID)
	}
}

// TestLoginRejectsUnverifiedEmail asserts an OIDC identity whose email is not
// verified is refused and no session is issued.
func TestLoginRejectsUnverifiedEmail(t *testing.T) {
	lm, _ := newLogin(t, fakeVerifier{claims: OIDCClaims{Subject: "sub-1", Email: "dev@example.com", EmailVerified: false}})
	if _, _, err := lm.SignIn(context.Background(), "t"); err == nil {
		t.Fatal("SignIn accepted an unverified email; want rejection")
	}
}

// TestLoginRejectsInvalidToken asserts a verifier error surfaces as a login
// failure with no account side effects.
func TestLoginRejectsInvalidToken(t *testing.T) {
	lm, _ := newLogin(t, fakeVerifier{err: errors.New("bad signature")})
	if _, _, err := lm.SignIn(context.Background(), "t"); err == nil {
		t.Fatal("SignIn accepted an invalid token; want rejection")
	}
}
