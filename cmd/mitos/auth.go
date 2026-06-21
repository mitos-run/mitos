package main

import (
	"context"
	"time"

	"mitos.run/mitos/internal/agentcli"
	"mitos.run/mitos/internal/saas"
)

// authBackend wraps a *agentcli.ClusterBackend with an Auth() method so the
// agentcli dispatcher can reach the `auth` verbs. The cluster backend itself has
// no account surface (that is the hosted offering, not the cluster); this wrapper
// attaches a local, ephemeral account service so `mitos auth` works for local
// development and to exercise the path end to end. A real hosted account client
// replaces the local session service as a documented follow-up.
type authBackend struct {
	*agentcli.ClusterBackend
	auth agentcli.AuthService
}

// Auth implements the optional authProvider capability the agentcli dispatcher
// looks for.
func (b *authBackend) Auth() agentcli.AuthService { return b.auth }

// withLocalAuth wraps a cluster backend with a local, ephemeral account service
// so the auth verbs are reachable. The session service is in-memory and per
// process; it is the seam a hosted client plugs into.
func withLocalAuth(cb *agentcli.ClusterBackend) *authBackend {
	store := saas.NewMemStore()
	keys := saas.NewKeyService(store)
	accts := saas.NewAccountService(store, keys)
	sessions := saas.NewSessionStore()
	return &authBackend{ClusterBackend: cb, auth: newAuthBridge(saas.NewSessionService(sessions, accts))}
}

// authBridge adapts a saas.SessionService to the agentcli.AuthService interface
// the `mitos auth` commands dispatch to. It resolves a session token to an
// account, then delegates the org-scoped key verbs to the membership-guarded
// saas.AccountService, so the CLI cannot reach another org's keys: every verb is
// scoped by the resolved account's memberships.
//
// This is the seam where a real hosted account client plugs in (a documented
// follow-up). The bridge keeps the saas types out of the agentcli package and
// the agentcli types out of saas, so neither imports the other.
type authBridge struct {
	sessions *saas.SessionService
}

// newAuthBridge builds a bridge over a session service.
func newAuthBridge(s *saas.SessionService) agentcli.AuthService {
	return &authBridge{sessions: s}
}

func (b *authBridge) WhoAmI(ctx context.Context, token string) (agentcli.Principal, error) {
	acct, orgs, err := b.sessions.Resolve(ctx, token)
	if err != nil {
		return agentcli.Principal{}, err
	}
	ids := make([]string, 0, len(orgs))
	for _, o := range orgs {
		ids = append(ids, o.ID)
	}
	return agentcli.Principal{Email: acct.Email, OrgIDs: ids}, nil
}

func (b *authBridge) CreateKey(ctx context.Context, token, orgID, name string, scopes []string, ttl time.Duration) (agentcli.CreatedKey, error) {
	accountID, err := b.sessions.AccountFor(token)
	if err != nil {
		return agentcli.CreatedKey{}, err
	}
	created, err := b.sessions.Accounts().CreateKey(ctx, accountID, saas.CreateKeyRequest{
		OrgID:  orgID,
		Name:   name,
		Scopes: scopes,
		TTL:    ttl,
	})
	if err != nil {
		return agentcli.CreatedKey{}, err
	}
	return agentcli.CreatedKey{RawKey: created.RawKey, Info: keyInfo(created.Record)}, nil
}

func (b *authBridge) ListKeys(ctx context.Context, token, orgID string) ([]agentcli.KeyInfo, error) {
	accountID, err := b.sessions.AccountFor(token)
	if err != nil {
		return nil, err
	}
	recs, err := b.sessions.Accounts().ListKeys(ctx, accountID, orgID)
	if err != nil {
		return nil, err
	}
	out := make([]agentcli.KeyInfo, 0, len(recs))
	for _, r := range recs {
		out = append(out, keyInfo(r))
	}
	return out, nil
}

func (b *authBridge) RevokeKey(ctx context.Context, token, keyID string) error {
	accountID, err := b.sessions.AccountFor(token)
	if err != nil {
		return err
	}
	return b.sessions.Accounts().RevokeKey(ctx, accountID, keyID)
}

// keyInfo projects a saas.ApiKey to the CLI-facing, non-secret KeyInfo. The raw
// value is never present on an ApiKey, so this projection cannot leak a secret.
func keyInfo(k saas.ApiKey) agentcli.KeyInfo {
	return agentcli.KeyInfo{
		ID:        k.ID,
		OrgID:     k.OrgID,
		Name:      k.Name,
		Prefix:    k.Prefix,
		Scopes:    k.Scopes,
		CreatedAt: k.CreatedAt,
		ExpiresAt: k.ExpiresAt,
		Revoked:   k.IsRevoked(),
	}
}
