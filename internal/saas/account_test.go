package saas

import (
	"context"
	"errors"
	"testing"
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
