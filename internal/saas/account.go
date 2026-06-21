package saas

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// AccountService is the application-level surface over the Store and KeyService:
// it provisions accounts (each with a Personal organization, Daytona-style),
// resolves an account's organizations, and is the object the CLI key-management
// verbs (create, list, revoke) talk to. It is the seam the hosted onboarding
// funnel (issue #215) and the web console (issue #214) build on.
type AccountService struct {
	store Store
	keys  *KeyService
	now   func() time.Time
	idgen func() string
}

// NewAccountService builds an account service over store and the key service.
func NewAccountService(store Store, keys *KeyService, opts ...KeyServiceOption) *AccountService {
	// Reuse the KeyService options struct for clock/idgen so callers configure one
	// place. Build a throwaway KeyService to extract the resolved now/idgen.
	cfg := &KeyService{now: time.Now, idgen: randomID}
	for _, o := range opts {
		o(cfg)
	}
	return &AccountService{store: store, keys: keys, now: cfg.now, idgen: cfg.idgen}
}

// SignUp provisions a new account and its Personal organization, makes the
// account the owner of that org, and returns both. It is idempotent on email in
// the sense that a duplicate email returns ErrConflict rather than a second
// account.
func (s *AccountService) SignUp(ctx context.Context, email string) (Account, Organization, error) {
	if email == "" {
		return Account{}, Organization{}, fmt.Errorf("sign up: email is required")
	}
	if _, err := s.store.GetAccountByEmail(ctx, email); err == nil {
		return Account{}, Organization{}, ErrConflict
	}
	now := s.now()
	org := Organization{
		ID:        s.idgen(),
		Name:      "Personal",
		CreatedAt: now,
		Personal:  true,
	}
	acct := Account{
		ID:            s.idgen(),
		Email:         email,
		CreatedAt:     now,
		PersonalOrgID: org.ID,
	}
	if err := s.store.PutOrg(ctx, org); err != nil {
		return Account{}, Organization{}, fmt.Errorf("sign up: store org: %w", err)
	}
	if err := s.store.PutAccount(ctx, acct); err != nil {
		return Account{}, Organization{}, fmt.Errorf("sign up: store account: %w", err)
	}
	mem := Membership{AccountID: acct.ID, OrgID: org.ID, Role: RoleOwner, CreatedAt: now}
	if err := s.store.PutMembership(ctx, mem); err != nil {
		return Account{}, Organization{}, fmt.Errorf("sign up: store membership: %w", err)
	}
	return acct, org, nil
}

// Organizations returns the organizations an account belongs to, sorted by id so
// the result is stable.
func (s *AccountService) Organizations(ctx context.Context, accountID string) ([]Organization, error) {
	members, err := s.store.ListMemberships(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("list memberships: %w", err)
	}
	out := make([]Organization, 0, len(members))
	for _, m := range members {
		org, err := s.store.GetOrg(ctx, m.OrgID)
		if err != nil {
			continue
		}
		out = append(out, org)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// isMember reports whether accountID belongs to orgID. It is the authorization
// guard the CLI key verbs use so an account cannot manage another org's keys.
func (s *AccountService) isMember(ctx context.Context, accountID, orgID string) bool {
	members, err := s.store.ListMemberships(ctx, accountID)
	if err != nil {
		return false
	}
	for _, m := range members {
		if m.OrgID == orgID {
			return true
		}
	}
	return false
}

// CreateKey mints a scoped key for an org on behalf of accountID, enforcing that
// the account is a member of the org. The raw key is returned exactly once.
func (s *AccountService) CreateKey(ctx context.Context, accountID string, req CreateKeyRequest) (CreatedKey, error) {
	if !s.isMember(ctx, accountID, req.OrgID) {
		return CreatedKey{}, fmt.Errorf("create key: account is not a member of org %s: %w", req.OrgID, ErrKeyWrongOrg)
	}
	return s.keys.CreateKey(ctx, req)
}

// ListKeys returns the key metadata for an org on behalf of accountID, enforcing
// membership. The returned records never carry a raw key value, only the masked
// prefix and the hash; callers (the CLI, the console) display the prefix.
func (s *AccountService) ListKeys(ctx context.Context, accountID, orgID string) ([]ApiKey, error) {
	if !s.isMember(ctx, accountID, orgID) {
		return nil, fmt.Errorf("list keys: account is not a member of org %s: %w", orgID, ErrKeyWrongOrg)
	}
	keys, err := s.store.ListApiKeys(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].CreatedAt.Before(keys[j].CreatedAt) })
	return keys, nil
}

// ListMembers returns the memberships of orgID on behalf of accountID,
// enforcing that the account is itself a member of the org. It is the
// org-scoped members view the console reads: an account that does not belong to
// the org cannot enumerate its members. The result is sorted by account id for a
// stable listing.
func (s *AccountService) ListMembers(ctx context.Context, accountID, orgID string) ([]Membership, error) {
	if !s.isMember(ctx, accountID, orgID) {
		return nil, fmt.Errorf("list members: account is not a member of org %s: %w", orgID, ErrKeyWrongOrg)
	}
	members, err := s.store.ListOrgMembers(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	sort.Slice(members, func(i, j int) bool { return members[i].AccountID < members[j].AccountID })
	return members, nil
}

// RevokeKey revokes a key by id on behalf of accountID, enforcing that the key
// belongs to an org the account is a member of. This prevents an account from
// revoking another org's key even if it learns the key id.
func (s *AccountService) RevokeKey(ctx context.Context, accountID, keyID string) error {
	rec, err := s.store.GetApiKey(ctx, keyID)
	if err != nil {
		return fmt.Errorf("revoke key: %w", err)
	}
	if !s.isMember(ctx, accountID, rec.OrgID) {
		return fmt.Errorf("revoke key: account is not a member of org %s: %w", rec.OrgID, ErrKeyWrongOrg)
	}
	return s.store.RevokeApiKey(ctx, keyID, s.now())
}
