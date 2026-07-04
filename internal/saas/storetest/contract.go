// Package storetest holds the shared behavioral contract for saas.Store. It is
// run against BOTH the in-memory MemStore (the reference implementation) and the
// Postgres PgStore so the durable store is proven equivalent to the reference,
// behavior for behavior. The contract lives in a non-test .go file so both the
// saas package test and the pgstore package test can import and run it without
// duplicating assertions.
package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"mitos.run/mitos/internal/saas"
)

// RunContract exercises the full saas.Store contract against the store the
// factory returns. The factory must return a FRESH, empty store on each call so
// the subtests do not see each other's data. Every subtest gets its own store.
func RunContract(t *testing.T, factory func(t *testing.T) saas.Store) {
	t.Helper()

	t.Run("AccountPutGetAndByEmail", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		a := saas.Account{ID: "acc-1", Email: "a@example.com", CreatedAt: created, PersonalOrgID: "org-1", DisplayName: "Ada", Timezone: "Europe/Berlin", Locale: "en-GB"}
		if err := s.PutAccount(ctx, a); err != nil {
			t.Fatalf("PutAccount: %v", err)
		}
		got, err := s.GetAccount(ctx, "acc-1")
		if err != nil {
			t.Fatalf("GetAccount: %v", err)
		}
		if got != a {
			t.Errorf("GetAccount round trip mismatch:\n got %+v\nwant %+v", got, a)
		}
		byEmail, err := s.GetAccountByEmail(ctx, "a@example.com")
		if err != nil {
			t.Fatalf("GetAccountByEmail: %v", err)
		}
		if byEmail != a {
			t.Errorf("GetAccountByEmail mismatch:\n got %+v\nwant %+v", byEmail, a)
		}
	})

	t.Run("AccountNotFound", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		if _, err := s.GetAccount(ctx, "nope"); !errors.Is(err, saas.ErrNotFound) {
			t.Errorf("GetAccount unknown: got %v, want ErrNotFound", err)
		}
		if _, err := s.GetAccountByEmail(ctx, "nope@example.com"); !errors.Is(err, saas.ErrNotFound) {
			t.Errorf("GetAccountByEmail unknown: got %v, want ErrNotFound", err)
		}
	})

	t.Run("AccountDuplicateEmailConflict", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		if err := s.PutAccount(ctx, saas.Account{ID: "acc-1", Email: "dup@example.com"}); err != nil {
			t.Fatalf("PutAccount first: %v", err)
		}
		err := s.PutAccount(ctx, saas.Account{ID: "acc-2", Email: "dup@example.com"})
		if !errors.Is(err, saas.ErrConflict) {
			t.Errorf("second account with same email: got %v, want ErrConflict", err)
		}
	})

	t.Run("AccountUpdateSameIDSameEmailOK", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		if err := s.PutAccount(ctx, saas.Account{ID: "acc-1", Email: "x@example.com", DisplayName: "before"}); err != nil {
			t.Fatalf("PutAccount: %v", err)
		}
		// Same id, same email, different field: update, not a conflict.
		if err := s.PutAccount(ctx, saas.Account{ID: "acc-1", Email: "x@example.com", DisplayName: "after"}); err != nil {
			t.Fatalf("PutAccount update: %v", err)
		}
		got, err := s.GetAccount(ctx, "acc-1")
		if err != nil {
			t.Fatalf("GetAccount: %v", err)
		}
		if got.DisplayName != "after" {
			t.Errorf("update not applied: DisplayName=%q", got.DisplayName)
		}
	})

	t.Run("OrgPutGetNotFound", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		created := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
		o := saas.Organization{ID: "org-1", Name: "Acme", CreatedAt: created, Personal: true}
		if err := s.PutOrg(ctx, o); err != nil {
			t.Fatalf("PutOrg: %v", err)
		}
		got, err := s.GetOrg(ctx, "org-1")
		if err != nil {
			t.Fatalf("GetOrg: %v", err)
		}
		if got != o {
			t.Errorf("GetOrg mismatch:\n got %+v\nwant %+v", got, o)
		}
		if _, err := s.GetOrg(ctx, "missing"); !errors.Is(err, saas.ErrNotFound) {
			t.Errorf("GetOrg unknown: got %v, want ErrNotFound", err)
		}
	})

	t.Run("ListOrgsReturnsEveryOrg", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		empty, err := s.ListOrgs(ctx)
		if err != nil {
			t.Fatalf("ListOrgs on empty store: %v", err)
		}
		if len(empty) != 0 {
			t.Errorf("ListOrgs on empty store returned %d orgs, want 0", len(empty))
		}
		created := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
		want := map[string]saas.Organization{
			"org-a": {ID: "org-a", Name: "Acme", CreatedAt: created, Personal: true},
			"org-b": {ID: "org-b", Name: "Bravo", CreatedAt: created},
		}
		for _, o := range want {
			if err := s.PutOrg(ctx, o); err != nil {
				t.Fatalf("PutOrg %s: %v", o.ID, err)
			}
		}
		got, err := s.ListOrgs(ctx)
		if err != nil {
			t.Fatalf("ListOrgs: %v", err)
		}
		if len(got) != len(want) {
			t.Fatalf("ListOrgs returned %d orgs, want %d", len(got), len(want))
		}
		for _, o := range got {
			w, ok := want[o.ID]
			if !ok {
				t.Errorf("ListOrgs returned unexpected org %q", o.ID)
				continue
			}
			if o != w {
				t.Errorf("ListOrgs org %s mismatch:\n got %+v\nwant %+v", o.ID, o, w)
			}
		}
	})

	t.Run("MembershipPutListAndReplace", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		m := saas.Membership{AccountID: "acc-1", OrgID: "org-1", Role: saas.RoleMember, CreatedAt: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)}
		if err := s.PutMembership(ctx, m); err != nil {
			t.Fatalf("PutMembership: %v", err)
		}
		list, err := s.ListMemberships(ctx, "acc-1")
		if err != nil {
			t.Fatalf("ListMemberships: %v", err)
		}
		if len(list) != 1 || list[0] != m {
			t.Fatalf("ListMemberships:\n got %+v\nwant [%+v]", list, m)
		}
		// Re-put for the same (org, account) replaces, never duplicates.
		m2 := m
		m2.Role = saas.RoleOwner
		if err := s.PutMembership(ctx, m2); err != nil {
			t.Fatalf("PutMembership replace: %v", err)
		}
		list, err = s.ListMemberships(ctx, "acc-1")
		if err != nil {
			t.Fatalf("ListMemberships after replace: %v", err)
		}
		if len(list) != 1 {
			t.Fatalf("replace duplicated membership: got %d rows", len(list))
		}
		if list[0].Role != saas.RoleOwner {
			t.Errorf("role not updated: got %q", list[0].Role)
		}
	})

	t.Run("ListMembershipsEmptyForUnknownAccount", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		list, err := s.ListMemberships(ctx, "nobody")
		if err != nil {
			t.Fatalf("ListMemberships: %v", err)
		}
		if len(list) != 0 {
			t.Errorf("expected empty, got %+v", list)
		}
	})

	t.Run("ListOrgMembersIsolation", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		// Two orgs, distinct members. Org A must never return Org B's members.
		must(t, s.PutMembership(ctx, saas.Membership{AccountID: "acc-1", OrgID: "org-A", Role: saas.RoleOwner}))
		must(t, s.PutMembership(ctx, saas.Membership{AccountID: "acc-2", OrgID: "org-A", Role: saas.RoleMember}))
		must(t, s.PutMembership(ctx, saas.Membership{AccountID: "acc-3", OrgID: "org-B", Role: saas.RoleOwner}))

		a, err := s.ListOrgMembers(ctx, "org-A")
		if err != nil {
			t.Fatalf("ListOrgMembers A: %v", err)
		}
		if len(a) != 2 {
			t.Fatalf("org A members: got %d, want 2 (%+v)", len(a), a)
		}
		for _, m := range a {
			if m.OrgID != "org-A" {
				t.Errorf("org A listing leaked %q member %q", m.OrgID, m.AccountID)
			}
		}
		b, err := s.ListOrgMembers(ctx, "org-B")
		if err != nil {
			t.Fatalf("ListOrgMembers B: %v", err)
		}
		if len(b) != 1 || b[0].AccountID != "acc-3" {
			t.Errorf("org B members: got %+v, want [acc-3]", b)
		}
		empty, err := s.ListOrgMembers(ctx, "org-unknown")
		if err != nil {
			t.Fatalf("ListOrgMembers unknown: %v", err)
		}
		if len(empty) != 0 {
			t.Errorf("unknown org should have no members, got %+v", empty)
		}
	})

	t.Run("SetMembershipRoleNotFound", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		err := s.SetMembershipRole(ctx, "org-1", "ghost", saas.RoleMember)
		if !errors.Is(err, saas.ErrNotFound) {
			t.Errorf("SetMembershipRole on missing membership: got %v, want ErrNotFound", err)
		}
	})

	t.Run("SetMembershipRolePromoteAndDemote", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		must(t, s.PutMembership(ctx, saas.Membership{AccountID: "acc-1", OrgID: "org-1", Role: saas.RoleOwner}))
		must(t, s.PutMembership(ctx, saas.Membership{AccountID: "acc-2", OrgID: "org-1", Role: saas.RoleMember}))
		// Promote acc-2 to owner.
		if err := s.SetMembershipRole(ctx, "org-1", "acc-2", saas.RoleOwner); err != nil {
			t.Fatalf("promote: %v", err)
		}
		// Now demote acc-1: allowed, acc-2 is still an owner.
		if err := s.SetMembershipRole(ctx, "org-1", "acc-1", saas.RoleMember); err != nil {
			t.Fatalf("demote with another owner present: %v", err)
		}
		members, err := s.ListOrgMembers(ctx, "org-1")
		if err != nil {
			t.Fatalf("ListOrgMembers: %v", err)
		}
		roles := map[string]saas.Role{}
		for _, m := range members {
			roles[m.AccountID] = m.Role
		}
		if roles["acc-1"] != saas.RoleMember || roles["acc-2"] != saas.RoleOwner {
			t.Errorf("roles after promote/demote: %+v", roles)
		}
	})

	t.Run("SetMembershipRoleLastOwnerRefused", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		must(t, s.PutMembership(ctx, saas.Membership{AccountID: "owner-1", OrgID: "org-1", Role: saas.RoleOwner}))
		must(t, s.PutMembership(ctx, saas.Membership{AccountID: "member-1", OrgID: "org-1", Role: saas.RoleMember}))
		// Demoting the sole owner is refused.
		err := s.SetMembershipRole(ctx, "org-1", "owner-1", saas.RoleMember)
		if !errors.Is(err, saas.ErrLastOwner) {
			t.Fatalf("sole-owner demotion: got %v, want ErrLastOwner", err)
		}
		// The owner must still be an owner (the refusal did not mutate state).
		members, err := s.ListOrgMembers(ctx, "org-1")
		if err != nil {
			t.Fatalf("ListOrgMembers: %v", err)
		}
		for _, m := range members {
			if m.AccountID == "owner-1" && m.Role != saas.RoleOwner {
				t.Errorf("refused demotion still mutated role to %q", m.Role)
			}
		}
		// Setting the sole owner to owner again is a no-op, not a last-owner error.
		if err := s.SetMembershipRole(ctx, "org-1", "owner-1", saas.RoleOwner); err != nil {
			t.Errorf("re-setting sole owner to owner: %v", err)
		}
	})

	t.Run("ApiKeyPutGetByHashAndByID", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		created := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
		expires := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
		k := saas.ApiKey{
			ID: "key-1", OrgID: "org-1", Name: "ci", Prefix: "mitos_live_ab12",
			Hash: "hash-abc", Scopes: []string{saas.ScopeSandboxes, saas.ScopeReadOnly},
			CreatedAt: created, ExpiresAt: expires,
		}
		if err := s.PutApiKey(ctx, k); err != nil {
			t.Fatalf("PutApiKey: %v", err)
		}
		// Verify path: lookup by hash.
		byHash, err := s.GetApiKeyByHash(ctx, "hash-abc")
		if err != nil {
			t.Fatalf("GetApiKeyByHash: %v", err)
		}
		assertKeyEqual(t, byHash, k)
		// Lookup by id.
		byID, err := s.GetApiKey(ctx, "key-1")
		if err != nil {
			t.Fatalf("GetApiKey: %v", err)
		}
		assertKeyEqual(t, byID, k)
		// The expiry metadata is preserved (not zeroed).
		if !byID.ExpiresAt.Equal(expires) {
			t.Errorf("ExpiresAt not preserved: got %v, want %v", byID.ExpiresAt, expires)
		}
		if byID.IsExpired(created) {
			t.Error("key should not be expired at creation time")
		}
	})

	t.Run("ApiKeyNotFound", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		if _, err := s.GetApiKey(ctx, "nope"); !errors.Is(err, saas.ErrNotFound) {
			t.Errorf("GetApiKey unknown: got %v, want ErrNotFound", err)
		}
		if _, err := s.GetApiKeyByHash(ctx, "nope"); !errors.Is(err, saas.ErrNotFound) {
			t.Errorf("GetApiKeyByHash unknown: got %v, want ErrNotFound", err)
		}
	})

	t.Run("ApiKeyDuplicateIDConflict", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		must(t, s.PutApiKey(ctx, saas.ApiKey{ID: "key-1", OrgID: "org-1", Hash: "h1"}))
		err := s.PutApiKey(ctx, saas.ApiKey{ID: "key-1", OrgID: "org-1", Hash: "h2"})
		if !errors.Is(err, saas.ErrConflict) {
			t.Errorf("duplicate key id: got %v, want ErrConflict", err)
		}
	})

	t.Run("ApiKeyNeverExpiresZeroTime", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		// No ExpiresAt: the key never expires; the zero time must round trip.
		must(t, s.PutApiKey(ctx, saas.ApiKey{ID: "key-1", OrgID: "org-1", Hash: "h"}))
		got, err := s.GetApiKey(ctx, "key-1")
		if err != nil {
			t.Fatalf("GetApiKey: %v", err)
		}
		if !got.ExpiresAt.IsZero() {
			t.Errorf("ExpiresAt should be zero (never expires), got %v", got.ExpiresAt)
		}
		if got.IsExpired(time.Now()) {
			t.Error("never-expiring key reported as expired")
		}
		if got.IsRevoked() {
			t.Error("live key reported as revoked")
		}
	})

	t.Run("ListApiKeysOrgScopedIncludesRevokedAndExpired", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
		must(t, s.PutApiKey(ctx, saas.ApiKey{ID: "k-live", OrgID: "org-A", Hash: "ha"}))
		must(t, s.PutApiKey(ctx, saas.ApiKey{ID: "k-expired", OrgID: "org-A", Hash: "hb", ExpiresAt: past}))
		must(t, s.PutApiKey(ctx, saas.ApiKey{ID: "k-other", OrgID: "org-B", Hash: "hc"}))
		// Revoke one of org A's keys; ListApiKeys must still return it (audit).
		must(t, s.RevokeApiKey(ctx, "k-live", time.Now()))

		a, err := s.ListApiKeys(ctx, "org-A")
		if err != nil {
			t.Fatalf("ListApiKeys A: %v", err)
		}
		if len(a) != 2 {
			t.Fatalf("org A keys: got %d, want 2 (revoked + expired both listed): %+v", len(a), a)
		}
		for _, k := range a {
			if k.OrgID != "org-A" {
				t.Errorf("org A listing leaked org %q key %q", k.OrgID, k.ID)
			}
		}
		b, err := s.ListApiKeys(ctx, "org-B")
		if err != nil {
			t.Fatalf("ListApiKeys B: %v", err)
		}
		if len(b) != 1 || b[0].ID != "k-other" {
			t.Errorf("org B keys: got %+v, want [k-other]", b)
		}
	})

	t.Run("RevokeApiKeyIdempotent", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		must(t, s.PutApiKey(ctx, saas.ApiKey{ID: "key-1", OrgID: "org-1", Hash: "h"}))
		at := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
		if err := s.RevokeApiKey(ctx, "key-1", at); err != nil {
			t.Fatalf("first revoke: %v", err)
		}
		got, err := s.GetApiKey(ctx, "key-1")
		if err != nil {
			t.Fatalf("GetApiKey: %v", err)
		}
		if !got.IsRevoked() || !got.RevokedAt.Equal(at) {
			t.Fatalf("after revoke: IsRevoked=%v RevokedAt=%v want revoked at %v", got.IsRevoked(), got.RevokedAt, at)
		}
		// Second revoke is a no-op: the original RevokedAt is preserved.
		later := at.Add(24 * time.Hour)
		if err := s.RevokeApiKey(ctx, "key-1", later); err != nil {
			t.Fatalf("second revoke: %v", err)
		}
		got2, err := s.GetApiKey(ctx, "key-1")
		if err != nil {
			t.Fatalf("GetApiKey after second revoke: %v", err)
		}
		if !got2.RevokedAt.Equal(at) {
			t.Errorf("second revoke changed RevokedAt: got %v, want original %v", got2.RevokedAt, at)
		}
	})

	t.Run("RevokeApiKeyNotFound", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		err := s.RevokeApiKey(ctx, "ghost", time.Now())
		if !errors.Is(err, saas.ErrNotFound) {
			t.Errorf("revoke unknown id: got %v, want ErrNotFound", err)
		}
	})

	t.Run("InvitationCreateGetByHashAndList", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
		inv := saas.Invitation{
			ID: "inv-1", OrgID: "org-1", Email: "carol@example.com", Role: saas.RoleAdmin,
			TokenHash: "hash-inv-1", State: saas.InvitationPending, InviterID: "acc-owner",
			CreatedAt: created, ExpiresAt: created.Add(7 * 24 * time.Hour),
		}
		if err := s.CreateInvitation(ctx, inv); err != nil {
			t.Fatalf("CreateInvitation: %v", err)
		}
		got, err := s.GetInvitationByTokenHash(ctx, "hash-inv-1")
		if err != nil {
			t.Fatalf("GetInvitationByTokenHash: %v", err)
		}
		if got != inv {
			t.Errorf("GetInvitationByTokenHash mismatch:\n got %+v\nwant %+v", got, inv)
		}
		if _, err := s.GetInvitationByTokenHash(ctx, "nope"); !errors.Is(err, saas.ErrNotFound) {
			t.Errorf("GetInvitationByTokenHash unknown: got %v, want ErrNotFound", err)
		}

		list, err := s.ListInvitations(ctx, "org-1")
		if err != nil {
			t.Fatalf("ListInvitations: %v", err)
		}
		if len(list) != 1 || list[0] != inv {
			t.Fatalf("ListInvitations:\n got %+v\nwant [%+v]", list, inv)
		}
		other, err := s.ListInvitations(ctx, "org-other")
		if err != nil {
			t.Fatalf("ListInvitations other org: %v", err)
		}
		if len(other) != 0 {
			t.Errorf("ListInvitations for unrelated org leaked rows: %+v", other)
		}
	})

	t.Run("InvitationUpdateStateAndRemove", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		inv := saas.Invitation{ID: "inv-2", OrgID: "org-1", Email: "dave@example.com", TokenHash: "hash-inv-2", State: saas.InvitationPending}
		must(t, s.CreateInvitation(ctx, inv))

		if err := s.UpdateInvitationState(ctx, "inv-2", saas.InvitationAccepted); err != nil {
			t.Fatalf("UpdateInvitationState: %v", err)
		}
		got, err := s.GetInvitationByTokenHash(ctx, "hash-inv-2")
		if err != nil {
			t.Fatalf("GetInvitationByTokenHash after update: %v", err)
		}
		if got.State != saas.InvitationAccepted {
			t.Errorf("state after update = %q, want accepted", got.State)
		}
		if err := s.UpdateInvitationState(ctx, "ghost", saas.InvitationAccepted); !errors.Is(err, saas.ErrNotFound) {
			t.Errorf("UpdateInvitationState unknown id: got %v, want ErrNotFound", err)
		}

		if err := s.RemoveInvitation(ctx, "inv-2"); err != nil {
			t.Fatalf("RemoveInvitation: %v", err)
		}
		if _, err := s.GetInvitationByTokenHash(ctx, "hash-inv-2"); !errors.Is(err, saas.ErrNotFound) {
			t.Errorf("GetInvitationByTokenHash after remove: got %v, want ErrNotFound", err)
		}
		if err := s.RemoveInvitation(ctx, "inv-2"); !errors.Is(err, saas.ErrNotFound) {
			t.Errorf("RemoveInvitation already-removed: got %v, want ErrNotFound", err)
		}
	})

	t.Run("DeleteMembershipNotFound", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		if err := s.DeleteMembership(ctx, "org-1", "ghost"); !errors.Is(err, saas.ErrNotFound) {
			t.Errorf("DeleteMembership on missing membership: got %v, want ErrNotFound", err)
		}
	})

	t.Run("DeleteMembershipLastOwnerRefused", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		must(t, s.PutMembership(ctx, saas.Membership{AccountID: "owner-1", OrgID: "org-1", Role: saas.RoleOwner}))
		if err := s.DeleteMembership(ctx, "org-1", "owner-1"); !errors.Is(err, saas.ErrLastOwner) {
			t.Errorf("removing sole owner: got %v, want ErrLastOwner", err)
		}
		list, err := s.ListOrgMembers(ctx, "org-1")
		if err != nil {
			t.Fatalf("ListOrgMembers: %v", err)
		}
		if len(list) != 1 {
			t.Errorf("refused delete still mutated membership list: %+v", list)
		}
	})

	t.Run("DeleteMembershipRemovesNonSoleOwner", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		must(t, s.PutMembership(ctx, saas.Membership{AccountID: "owner-1", OrgID: "org-1", Role: saas.RoleOwner}))
		must(t, s.PutMembership(ctx, saas.Membership{AccountID: "member-1", OrgID: "org-1", Role: saas.RoleMember}))
		if err := s.DeleteMembership(ctx, "org-1", "member-1"); err != nil {
			t.Fatalf("DeleteMembership: %v", err)
		}
		list, err := s.ListOrgMembers(ctx, "org-1")
		if err != nil {
			t.Fatalf("ListOrgMembers: %v", err)
		}
		if len(list) != 1 || list[0].AccountID != "owner-1" {
			t.Errorf("after delete: got %+v, want only owner-1", list)
		}
	})
}

// must fails the test if err is non-nil.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
}

// assertKeyEqual compares two ApiKeys, tolerating the scope slice and using
// time.Equal for timestamps so a UTC normalization in the store does not cause a
// spurious failure.
func assertKeyEqual(t *testing.T, got, want saas.ApiKey) {
	t.Helper()
	if got.ID != want.ID || got.OrgID != want.OrgID || got.Name != want.Name || got.Prefix != want.Prefix || got.Hash != want.Hash {
		t.Errorf("key scalar fields mismatch:\n got %+v\nwant %+v", got, want)
	}
	if len(got.Scopes) != len(want.Scopes) {
		t.Errorf("scopes length mismatch: got %v, want %v", got.Scopes, want.Scopes)
	} else {
		for i := range want.Scopes {
			if got.Scopes[i] != want.Scopes[i] {
				t.Errorf("scope[%d] mismatch: got %q, want %q", i, got.Scopes[i], want.Scopes[i])
			}
		}
	}
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("CreatedAt mismatch: got %v, want %v", got.CreatedAt, want.CreatedAt)
	}
	if !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Errorf("ExpiresAt mismatch: got %v, want %v", got.ExpiresAt, want.ExpiresAt)
	}
	if !got.RevokedAt.Equal(want.RevokedAt) {
		t.Errorf("RevokedAt mismatch: got %v, want %v", got.RevokedAt, want.RevokedAt)
	}
}
