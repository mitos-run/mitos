package saas

import (
	"context"
	"errors"
)

// ErrLoginRejected is returned when an OIDC sign-in is refused: the token failed
// verification or the identity is not usable (e.g. an unverified email). It does
// not distinguish the cases, so a caller cannot probe why a login failed.
var ErrLoginRejected = errors.New("saas: oidc login rejected")

// OIDCClaims is the verified subset of an OIDC ID token the login flow needs.
// Only a verified email establishes an account, so an unverified email is
// refused regardless of the rest of the token.
type OIDCClaims struct {
	Subject       string
	Email         string
	EmailVerified bool
}

// IDTokenVerifier verifies a raw OIDC ID token and returns its claims. The REAL
// implementation wraps an OIDC provider's verifier (coreos/go-oidc) configured
// from the operator's issuer (self-host) or our hosted IdP; it is wired in
// cmd/console as a Phase C follow-up. Tests and the tested default use a fake.
// Keeping verification behind this seam lets the session/account-binding logic
// be tested without a live provider.
type IDTokenVerifier interface {
	Verify(ctx context.Context, rawIDToken string) (OIDCClaims, error)
}

// LoginManager turns a verified OIDC ID token into a browser session. On a first
// login it find-or-creates the account by verified email (which auto-creates the
// account's org — the self-host single-org default), then issues a session token
// recorded in the SessionStore. It never holds the raw token in the clear.
type LoginManager struct {
	verifier IDTokenVerifier
	accounts *AccountService
	sessions Sessions
	newToken func() string
}

// NewLoginManager builds a LoginManager. newToken mints an opaque session token
// (random in production; deterministic in tests).
func NewLoginManager(v IDTokenVerifier, accounts *AccountService, sessions Sessions, newToken func() string) *LoginManager {
	return &LoginManager{verifier: v, accounts: accounts, sessions: sessions, newToken: newToken}
}

// SignIn verifies rawIDToken, find-or-creates the account behind its verified
// email, issues a session token, and returns the account and the raw token (to
// be set as the browser session cookie exactly once). A token that fails
// verification or carries an unverified/empty email is refused with
// ErrLoginRejected and has no account side effects.
func (m *LoginManager) SignIn(ctx context.Context, rawIDToken string) (Account, string, error) {
	claims, err := m.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return Account{}, "", ErrLoginRejected
	}
	if claims.Email == "" || !claims.EmailVerified {
		return Account{}, "", ErrLoginRejected
	}
	acct, err := m.accounts.FindOrCreateByEmail(ctx, claims.Email)
	if err != nil {
		return Account{}, "", err
	}
	token := m.newToken()
	m.sessions.Issue(acct.ID, token)
	return acct, token, nil
}
