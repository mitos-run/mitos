package saas

import (
	"context"
	"errors"
	"testing"
	"time"
)

// newAccountFixture wires an account service over a fresh in-memory store.
func newAccountFixture(t *testing.T) (*AccountService, *MemStore) {
	t.Helper()
	store := NewMemStore()
	keys := NewKeyService(store)
	return NewAccountService(store, keys), store
}

// TestSignUpCreatesPersonalOrgAndOwnerMembership asserts a new account gets a
// Personal org and is its owner, Daytona-style, so it can act immediately.
func TestSignUpCreatesPersonalOrgAndOwnerMembership(t *testing.T) {
	svc, store := newAccountFixture(t)
	acct, org, err := svc.SignUp(context.Background(), "dev@example.com")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	if !org.Personal {
		t.Error("auto-created org is not marked Personal")
	}
	if acct.PersonalOrgID != org.ID {
		t.Errorf("account PersonalOrgID = %q, want %q", acct.PersonalOrgID, org.ID)
	}
	members, err := store.ListMemberships(context.Background(), acct.ID)
	if err != nil {
		t.Fatalf("ListMemberships: %v", err)
	}
	if len(members) != 1 || members[0].OrgID != org.ID || members[0].Role != RoleOwner {
		t.Errorf("membership = %+v, want one owner membership of %q", members, org.ID)
	}
}

// TestSignUpStampsHomeRegion asserts a new Personal org is stamped with the
// deployment's placement registry default region (issue #712 phase 0), and
// that a service built with no WithHomeRegion option leaves it empty
// (meaning "the deployment's registry default", resolved elsewhere).
func TestSignUpStampsHomeRegion(t *testing.T) {
	store := NewMemStore()
	svc := NewAccountService(store, NewKeyService(store), WithHomeRegion("fra"))
	_, org, err := svc.SignUp(context.Background(), "region@example.com")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	if org.HomeRegion != "fra" {
		t.Errorf("HomeRegion = %q, want fra", org.HomeRegion)
	}
	stored, err := store.GetOrg(context.Background(), org.ID)
	if err != nil {
		t.Fatalf("GetOrg: %v", err)
	}
	if stored.HomeRegion != "fra" {
		t.Errorf("stored HomeRegion = %q, want fra", stored.HomeRegion)
	}

	svcNoRegion, _ := newAccountFixture(t)
	_, orgNoRegion, err := svcNoRegion.SignUp(context.Background(), "noregion@example.com")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	if orgNoRegion.HomeRegion != "" {
		t.Errorf("HomeRegion with no WithHomeRegion option = %q, want empty", orgNoRegion.HomeRegion)
	}
}

// TestOrganizationsForResolvesFromMembershipsAndDedups asserts OrganizationsFor
// resolves each unique org named by the given memberships (the zero-extra-list
// path used by the account view), deduplicates repeated org ids, and skips an
// org that no longer exists rather than failing the whole map.
func TestOrganizationsForResolvesFromMemberships(t *testing.T) {
	svc, store := newAccountFixture(t)
	ctx := context.Background()
	orgA := Organization{ID: "org-a", Name: "A", HomeRegion: "fra"}
	orgB := Organization{ID: "org-b", Name: "B", HomeRegion: "iad"}
	if err := store.PutOrg(ctx, orgA); err != nil {
		t.Fatalf("PutOrg A: %v", err)
	}
	if err := store.PutOrg(ctx, orgB); err != nil {
		t.Fatalf("PutOrg B: %v", err)
	}
	mems := []Membership{
		{AccountID: "acct-1", OrgID: "org-a", Role: RoleOwner},
		{AccountID: "acct-1", OrgID: "org-b", Role: RoleMember},
		{AccountID: "acct-1", OrgID: "org-a", Role: RoleOwner},     // duplicate id
		{AccountID: "acct-1", OrgID: "org-gone", Role: RoleMember}, // never stored
	}
	got := svc.OrganizationsFor(ctx, mems)
	if len(got) != 2 {
		t.Fatalf("OrganizationsFor returned %d orgs, want 2: %+v", len(got), got)
	}
	if got["org-a"].HomeRegion != "fra" || got["org-b"].HomeRegion != "iad" {
		t.Errorf("resolved orgs = %+v, want org-a fra and org-b iad", got)
	}
	if _, ok := got["org-gone"]; ok {
		t.Error("a membership to a missing org must be skipped, not surfaced")
	}
}

// TestSignUpRejectsDuplicateEmail asserts a second sign-up on the same email
// fails rather than creating a second account.
func TestSignUpRejectsDuplicateEmail(t *testing.T) {
	svc, _ := newAccountFixture(t)
	if _, _, err := svc.SignUp(context.Background(), "dup@example.com"); err != nil {
		t.Fatalf("first SignUp: %v", err)
	}
	if _, _, err := svc.SignUp(context.Background(), "dup@example.com"); !errors.Is(err, ErrConflict) {
		t.Errorf("second SignUp err = %v, want ErrConflict", err)
	}
}

// TestCreateKeyForOwnOrgSucceeds asserts a member can mint a key for its org.
func TestCreateKeyForOwnOrgSucceeds(t *testing.T) {
	svc, _ := newAccountFixture(t)
	acct, org, err := svc.SignUp(context.Background(), "owner@example.com")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	created, err := svc.CreateKey(context.Background(), acct.ID, CreateKeyRequest{OrgID: org.ID, Name: "ci", Scopes: []string{ScopeSandboxes}})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if created.RawKey == "" {
		t.Error("CreateKey returned an empty raw key")
	}
}

// TestCreateKeyForOtherOrgIsRefused is the management-layer cross-org guard: an
// account cannot mint a key for an org it does not belong to.
func TestCreateKeyForOtherOrgIsRefused(t *testing.T) {
	svc, _ := newAccountFixture(t)
	alice, _, err := svc.SignUp(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("SignUp alice: %v", err)
	}
	_, bobOrg, err := svc.SignUp(context.Background(), "bob@example.com")
	if err != nil {
		t.Fatalf("SignUp bob: %v", err)
	}
	// Alice attempts to mint a key for Bob's personal org.
	_, err = svc.CreateKey(context.Background(), alice.ID, CreateKeyRequest{OrgID: bobOrg.ID, Scopes: []string{ScopeSandboxes}})
	if !errors.Is(err, ErrKeyWrongOrg) {
		t.Errorf("CreateKey for other org err = %v, want ErrKeyWrongOrg", err)
	}
}

// TestListKeysForOtherOrgIsRefused asserts an account cannot list another org's
// keys even if it learns the org id.
func TestListKeysForOtherOrgIsRefused(t *testing.T) {
	svc, _ := newAccountFixture(t)
	alice, _, _ := svc.SignUp(context.Background(), "alice2@example.com")
	_, bobOrg, _ := svc.SignUp(context.Background(), "bob2@example.com")
	if _, err := svc.ListKeys(context.Background(), alice.ID, bobOrg.ID); !errors.Is(err, ErrKeyWrongOrg) {
		t.Errorf("ListKeys for other org err = %v, want ErrKeyWrongOrg", err)
	}
}

// TestRevokeOtherOrgKeyIsRefused asserts an account cannot revoke another org's
// key even with the key id, and the key stays live.
func TestRevokeOtherOrgKeyIsRefused(t *testing.T) {
	svc, store := newAccountFixture(t)
	alice, _, _ := svc.SignUp(context.Background(), "alice3@example.com")
	bob, bobOrg, _ := svc.SignUp(context.Background(), "bob3@example.com")
	created, err := svc.CreateKey(context.Background(), bob.ID, CreateKeyRequest{OrgID: bobOrg.ID, Scopes: []string{ScopeSandboxes}})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if err := svc.RevokeKey(context.Background(), alice.ID, created.Record.ID); !errors.Is(err, ErrKeyWrongOrg) {
		t.Errorf("RevokeKey across orgs err = %v, want ErrKeyWrongOrg", err)
	}
	// The key must still be live.
	rec, err := store.GetApiKey(context.Background(), created.Record.ID)
	if err != nil {
		t.Fatalf("GetApiKey: %v", err)
	}
	if rec.IsRevoked() {
		t.Error("Bob's key was revoked by a non-member")
	}
}

// TestRevokeOwnKeySucceeds asserts a member can revoke its own org's key and the
// key stops verifying.
func TestRevokeOwnKeySucceeds(t *testing.T) {
	svc, _ := newAccountFixture(t)
	bob, bobOrg, _ := svc.SignUp(context.Background(), "bob4@example.com")
	keys := NewKeyService(svcStore(svc))
	created, err := svc.CreateKey(context.Background(), bob.ID, CreateKeyRequest{OrgID: bobOrg.ID, Scopes: []string{ScopeSandboxes}})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if err := svc.RevokeKey(context.Background(), bob.ID, created.Record.ID); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	if _, err := keys.Verify(context.Background(), created.RawKey, ScopeSandboxes); !errors.Is(err, ErrKeyRevoked) {
		t.Errorf("Verify after revoke err = %v, want ErrKeyRevoked", err)
	}
}

// svcStore reaches the store behind an account service for assertions. It exists
// so the test can build a KeyService sharing the same store.
func svcStore(s *AccountService) Store { return s.store }

// seedOrgWithOwnerAndMember creates a service with one org, an owner account,
// and a plain member account. It returns the service, org id, owner id, and
// member id.
func seedOrgWithOwnerAndMember(t *testing.T) (*AccountService, string, string, string) {
	t.Helper()
	svc, store := newAccountFixture(t)
	ctx := context.Background()

	owner, org, err := svc.SignUp(ctx, "owner.seed@example.com")
	if err != nil {
		t.Fatalf("seedOrgWithOwnerAndMember SignUp owner: %v", err)
	}
	member, _, err := svc.SignUp(ctx, "member.seed@example.com")
	if err != nil {
		t.Fatalf("seedOrgWithOwnerAndMember SignUp member: %v", err)
	}
	mem := Membership{AccountID: member.ID, OrgID: org.ID, Role: RoleMember, CreatedAt: org.CreatedAt}
	if err := store.PutMembership(ctx, mem); err != nil {
		t.Fatalf("seedOrgWithOwnerAndMember PutMembership: %v", err)
	}
	return svc, org.ID, owner.ID, member.ID
}

// roleOf returns the role of targetID from a membership slice.
func roleOf(members []Membership, targetID string) Role {
	for _, m := range members {
		if m.AccountID == targetID {
			return m.Role
		}
	}
	return ""
}

// TestSetMemberRoleAuthorization verifies the permission model for SetMemberRole:
// owners can change roles, viewers cannot, and the last owner cannot be demoted.
func TestSetMemberRoleAuthorization(t *testing.T) {
	svc, orgID, ownerID, memberID := seedOrgWithOwnerAndMember(t)
	ctx := context.Background()

	// Owner can promote the member to admin.
	if err := svc.SetMemberRole(ctx, ownerID, orgID, memberID, RoleAdmin); err != nil {
		t.Fatalf("owner SetMemberRole: %v", err)
	}
	members, err := svc.ListMembers(ctx, ownerID, orgID)
	if err != nil {
		t.Fatalf("ListMembers after promote: %v", err)
	}
	if roleOf(members, memberID) != RoleAdmin {
		t.Fatalf("member role = %s, want admin", roleOf(members, memberID))
	}

	// Demote back to viewer so memberID has no PermManageMembers.
	if err := svc.SetMemberRole(ctx, ownerID, orgID, memberID, RoleViewer); err != nil {
		t.Fatalf("demote to viewer: %v", err)
	}

	// A viewer must not be able to change roles.
	if err := svc.SetMemberRole(ctx, memberID, orgID, ownerID, RoleMember); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer SetMemberRole err = %v, want ErrForbidden", err)
	}

	// A non-member actor gets a not-a-member error (ErrKeyWrongOrg).
	outsider, _, _ := svc.SignUp(ctx, "outsider.seed@example.com")
	if err := svc.SetMemberRole(ctx, outsider.ID, orgID, ownerID, RoleMember); !errors.Is(err, ErrKeyWrongOrg) {
		t.Fatalf("non-member SetMemberRole err = %v, want ErrKeyWrongOrg", err)
	}

	// The last owner cannot be demoted.
	if err := svc.SetMemberRole(ctx, ownerID, orgID, ownerID, RoleMember); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("demote last owner err = %v, want ErrLastOwner", err)
	}

	// SetMemberRole returns ErrNotFound for a target not in the org.
	if err := svc.SetMemberRole(ctx, ownerID, orgID, "no-such-account", RoleMember); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown target SetMemberRole err = %v, want ErrNotFound", err)
	}
}

// TestSetMemberRoleAdminCannotGrantOwner verifies the role-grant ceiling: an
// admin holds PermManageMembers (so is not blocked by ErrForbidden) but must
// not be able to promote anyone to owner, only an owner can. This closes the
// privilege-escalation path an admin would otherwise have via direct role
// changes, mirroring the same ceiling CreateInvite enforces.
func TestSetMemberRoleAdminCannotGrantOwner(t *testing.T) {
	svc, orgID, ownerID, memberID := seedOrgWithOwnerAndMember(t)
	ctx := context.Background()

	// Promote memberID to admin so it holds PermManageMembers.
	if err := svc.SetMemberRole(ctx, ownerID, orgID, memberID, RoleAdmin); err != nil {
		t.Fatalf("owner promote to admin: %v", err)
	}

	// A second account to be the target of the owner-grant attempt.
	target, _, err := svc.SignUp(ctx, "target.seed@example.com")
	if err != nil {
		t.Fatalf("SignUp target: %v", err)
	}
	if err := svc.store.PutMembership(ctx, Membership{AccountID: target.ID, OrgID: orgID, Role: RoleViewer, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("seed target membership: %v", err)
	}

	// The admin cannot mint a new owner.
	if err := svc.SetMemberRole(ctx, memberID, orgID, target.ID, RoleOwner); !errors.Is(err, ErrRoleNotGrantable) {
		t.Fatalf("admin SetMemberRole to owner err = %v, want ErrRoleNotGrantable", err)
	}

	// The owner can.
	if err := svc.SetMemberRole(ctx, ownerID, orgID, target.ID, RoleOwner); err != nil {
		t.Fatalf("owner SetMemberRole to owner: %v", err)
	}
	members, err := svc.ListMembers(ctx, ownerID, orgID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	if roleOf(members, target.ID) != RoleOwner {
		t.Fatalf("target role = %s, want owner", roleOf(members, target.ID))
	}
}

// TestUpdateProfile asserts UpdateProfile persists display name, timezone, and
// locale, and that Profile returns the updated values alongside memberships.
func TestUpdateProfile(t *testing.T) {
	svc, _, ownerID, _ := seedOrgWithOwnerAndMember(t)
	if _, err := svc.UpdateProfile(context.Background(), ownerID, ProfileUpdate{DisplayName: "Alice A", Timezone: "Europe/Berlin", Locale: "en-GB"}); err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	acct, _, err := svc.Profile(context.Background(), ownerID)
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if acct.DisplayName != "Alice A" || acct.Timezone != "Europe/Berlin" || acct.Locale != "en-GB" {
		t.Fatalf("profile not updated: %+v", acct)
	}
}

// TestMemberRoleReturnsSeededRole asserts that MemberRole returns the correct
// role for a known member and a non-nil error for a non-member.
func TestMemberRoleReturnsSeededRole(t *testing.T) {
	svc, orgID, ownerID, _ := seedOrgWithOwnerAndMember(t)
	ctx := context.Background()

	// A known owner membership must return RoleOwner.
	role, err := svc.MemberRole(ctx, ownerID, orgID)
	if err != nil {
		t.Fatalf("MemberRole for owner: %v", err)
	}
	if role != RoleOwner {
		t.Errorf("MemberRole for owner = %q, want %q", role, RoleOwner)
	}

	// A non-member must return a non-nil error.
	outsider, _, _ := svc.SignUp(ctx, "outsider.memberrole@example.com")
	if _, err := svc.MemberRole(ctx, outsider.ID, orgID); err == nil {
		t.Error("MemberRole for non-member: expected non-nil error, got nil")
	}
}

// TestListMembersReturnsOrgMembers asserts a member can list its org's members
// and that another org's members are never included.
func TestListMembersReturnsOrgMembers(t *testing.T) {
	svc, _ := newAccountFixture(t)
	ctx := context.Background()
	alice, aliceOrg, _ := svc.SignUp(ctx, "alice.mem@example.com")
	bob, bobOrg, _ := svc.SignUp(ctx, "bob.mem@example.com")

	got, err := svc.ListMembers(ctx, alice.ID, aliceOrg.ID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	if len(got) != 1 || got[0].AccountID != alice.ID {
		t.Fatalf("ListMembers = %+v, want only alice", got)
	}
	// Bob's org must never leak alice, and alice cannot list bob's org.
	if _, err := svc.ListMembers(ctx, alice.ID, bobOrg.ID); !errors.Is(err, ErrKeyWrongOrg) {
		t.Errorf("alice ListMembers(bobOrg) err = %v, want ErrKeyWrongOrg", err)
	}
	bobMembers, err := svc.ListMembers(ctx, bob.ID, bobOrg.ID)
	if err != nil {
		t.Fatalf("bob ListMembers: %v", err)
	}
	if len(bobMembers) != 1 || bobMembers[0].AccountID != bob.ID {
		t.Errorf("bob ListMembers = %+v, want only bob", bobMembers)
	}
}
