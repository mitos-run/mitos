package saas

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrNotFound is returned by a Store when the requested record does not exist.
// Callers map this to a public not_found / unauthorized envelope as appropriate;
// the gateway maps a missing key to unauthorized so a probe cannot tell "no such
// key" from "wrong key".
var ErrNotFound = errors.New("saas: record not found")

// ErrConflict is returned when a uniqueness invariant would be violated (for
// example a second account with the same email, or a duplicate key id).
var ErrConflict = errors.New("saas: conflict")

// ErrForbidden is returned when the actor's role does not grant the required
// permission for the requested operation.
var ErrForbidden = errors.New("saas: forbidden")

// ErrLastOwner is returned when an operation would demote or remove the last
// owner of an organization, which is not allowed: every org must retain at
// least one owner at all times.
var ErrLastOwner = errors.New("saas: cannot demote or remove the last owner of an organization")

// Store is the pluggable persistence seam for the SaaS front door. The in-memory
// implementation (MemStore) is the tested default; a Postgres implementation is
// a documented follow-up (issue #211 owns the migrations seam). The interface is
// deliberately narrow: it holds accounts, organizations, memberships, and API
// keys, and resolves a key by its hash. It NEVER sees a raw key value; the key
// service hashes before calling PutApiKey and looks up by hash on verify.
//
// All sandbox, usage, and quota data is org-scoped and lives behind this seam;
// the methods that fetch keys do so by hash (for verify) or by org (for listing
// and revocation), never globally, so a key for org A is never returned to a
// caller acting for org B.
type Store interface {
	// PutAccount stores an account. It returns ErrConflict if the email is already
	// taken by a different account.
	PutAccount(ctx context.Context, a Account) error
	// GetAccount returns the account by id, or ErrNotFound.
	GetAccount(ctx context.Context, id string) (Account, error)
	// GetAccountByEmail returns the account by email, or ErrNotFound.
	GetAccountByEmail(ctx context.Context, email string) (Account, error)

	// PutOrg stores an organization.
	PutOrg(ctx context.Context, o Organization) error
	// GetOrg returns the organization by id, or ErrNotFound.
	GetOrg(ctx context.Context, id string) (Organization, error)
	// ListOrgs returns every organization, in no particular order. It is an
	// OPERATOR/machine surface (the console's usage drawdown driver iterates
	// the orgs to settle metered usage against prepaid credit, issue #602); it
	// is never wired to a tenant-facing endpoint.
	ListOrgs(ctx context.Context) ([]Organization, error)

	// PutMembership stores a membership.
	PutMembership(ctx context.Context, m Membership) error
	// ListMemberships returns every membership for an account.
	ListMemberships(ctx context.Context, accountID string) ([]Membership, error)
	// ListOrgMembers returns every membership in an organization. It is the
	// org-scoped half of membership listing the console members view reads: a
	// caller asks for the members of an org it is authorized for, and never sees
	// another org's members. It returns an empty slice for an unknown org.
	ListOrgMembers(ctx context.Context, orgID string) ([]Membership, error)
	// SetMembershipRole updates the role of targetAccountID in orgID. It returns
	// ErrNotFound if there is no membership for that (org, account) pair. It
	// returns ErrLastOwner if the update would demote the sole remaining owner.
	SetMembershipRole(ctx context.Context, orgID, targetAccountID string, role Role) error

	// PutApiKey stores an API key. The key carries only a hash, never a raw value.
	// It returns ErrConflict if the id is already used.
	PutApiKey(ctx context.Context, k ApiKey) error
	// GetApiKeyByHash returns the key whose stored hash equals hash, or ErrNotFound.
	// This is the verify path; the key service computes the hash from the presented
	// raw key and looks it up here.
	GetApiKeyByHash(ctx context.Context, hash string) (ApiKey, error)
	// GetApiKey returns the key by id, or ErrNotFound.
	GetApiKey(ctx context.Context, id string) (ApiKey, error)
	// ListApiKeys returns every key for an organization (including revoked and
	// expired ones, for audit). The raw value is never present; only metadata and
	// the hash.
	ListApiKeys(ctx context.Context, orgID string) ([]ApiKey, error)
	// RevokeApiKey marks the key revoked at the given time. It is idempotent; a
	// second revoke is a no-op. Returns ErrNotFound for an unknown id.
	RevokeApiKey(ctx context.Context, id string, at time.Time) error
}

// MemStore is the in-memory Store used as the tested default and by the unit
// suite. It is safe for concurrent use. It is NOT a production store: it holds
// everything in maps and loses all data on process exit. The Postgres store is a
// documented follow-up; this is the seam it plugs into.
type MemStore struct {
	mu       sync.RWMutex
	accounts map[string]Account
	byEmail  map[string]string // email -> account id
	orgs     map[string]Organization
	members  map[string][]Membership // account id -> memberships
	keys     map[string]ApiKey       // key id -> key
	byHash   map[string]string       // hash -> key id
}

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore {
	return &MemStore{
		accounts: map[string]Account{},
		byEmail:  map[string]string{},
		orgs:     map[string]Organization{},
		members:  map[string][]Membership{},
		keys:     map[string]ApiKey{},
		byHash:   map[string]string{},
	}
}

func (s *MemStore) PutAccount(_ context.Context, a Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.byEmail[a.Email]; ok && existing != a.ID {
		return ErrConflict
	}
	s.accounts[a.ID] = a
	s.byEmail[a.Email] = a.ID
	return nil
}

func (s *MemStore) GetAccount(_ context.Context, id string) (Account, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.accounts[id]
	if !ok {
		return Account{}, ErrNotFound
	}
	return a, nil
}

func (s *MemStore) GetAccountByEmail(_ context.Context, email string) (Account, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byEmail[email]
	if !ok {
		return Account{}, ErrNotFound
	}
	return s.accounts[id], nil
}

func (s *MemStore) PutOrg(_ context.Context, o Organization) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.orgs[o.ID] = o
	return nil
}

func (s *MemStore) GetOrg(_ context.Context, id string) (Organization, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.orgs[id]
	if !ok {
		return Organization{}, ErrNotFound
	}
	return o, nil
}

func (s *MemStore) ListOrgs(_ context.Context) ([]Organization, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Organization, 0, len(s.orgs))
	for _, o := range s.orgs {
		out = append(out, o)
	}
	return out, nil
}

func (s *MemStore) PutMembership(_ context.Context, m Membership) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.members[m.AccountID]
	for i, existing := range list {
		if existing.OrgID == m.OrgID {
			list[i] = m
			s.members[m.AccountID] = list
			return nil
		}
	}
	s.members[m.AccountID] = append(list, m)
	return nil
}

func (s *MemStore) ListMemberships(_ context.Context, accountID string) ([]Membership, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := s.members[accountID]
	out := make([]Membership, len(list))
	copy(out, list)
	return out, nil
}

func (s *MemStore) ListOrgMembers(_ context.Context, orgID string) ([]Membership, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []Membership{}
	for _, list := range s.members {
		for _, m := range list {
			if m.OrgID == orgID {
				out = append(out, m)
			}
		}
	}
	return out, nil
}

func (s *MemStore) SetMembershipRole(_ context.Context, orgID, targetAccountID string, role Role) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// If the new role is not owner, check that at least one other owner remains.
	if role != RoleOwner {
		ownerCount := 0
		targetIsOwner := false
		for _, list := range s.members {
			for _, m := range list {
				if m.OrgID == orgID && m.Role == RoleOwner {
					ownerCount++
					if m.AccountID == targetAccountID {
						targetIsOwner = true
					}
				}
			}
		}
		if targetIsOwner && ownerCount <= 1 {
			return ErrLastOwner
		}
	}

	list, ok := s.members[targetAccountID]
	if !ok {
		return ErrNotFound
	}
	for i, m := range list {
		if m.OrgID == orgID {
			list[i].Role = role
			s.members[targetAccountID] = list
			return nil
		}
	}
	return ErrNotFound
}

func (s *MemStore) PutApiKey(_ context.Context, k ApiKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.keys[k.ID]; ok {
		return ErrConflict
	}
	s.keys[k.ID] = k
	s.byHash[k.Hash] = k.ID
	return nil
}

func (s *MemStore) GetApiKeyByHash(_ context.Context, hash string) (ApiKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byHash[hash]
	if !ok {
		return ApiKey{}, ErrNotFound
	}
	return s.keys[id], nil
}

func (s *MemStore) GetApiKey(_ context.Context, id string) (ApiKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.keys[id]
	if !ok {
		return ApiKey{}, ErrNotFound
	}
	return k, nil
}

func (s *MemStore) ListApiKeys(_ context.Context, orgID string) ([]ApiKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []ApiKey{}
	for _, k := range s.keys {
		if k.OrgID == orgID {
			out = append(out, k)
		}
	}
	return out, nil
}

func (s *MemStore) RevokeApiKey(_ context.Context, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		return ErrNotFound
	}
	if k.IsRevoked() {
		return nil
	}
	k.RevokedAt = at
	s.keys[id] = k
	return nil
}
