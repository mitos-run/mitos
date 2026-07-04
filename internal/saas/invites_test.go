package saas_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"mitos.run/mitos/internal/saas"
)

// fixedClock returns a func() time.Time that always reports t, for
// deterministic invitation lifecycle tests.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// seedOrg builds a fresh MemStore with one owner account and org, returning
// the store (so callers can build one or more InvitationServices sharing it,
// e.g. to advance the clock between calls) plus the org and owner ids.
func seedOrg(t *testing.T) (saas.Store, string, string) {
	t.Helper()
	store := saas.NewMemStore()
	keys := saas.NewKeyService(store)
	accounts := saas.NewAccountService(store, keys)
	owner, org, err := accounts.SignUp(context.Background(), "owner@acme.com")
	if err != nil {
		t.Fatalf("SignUp owner: %v", err)
	}
	return store, org.ID, owner.ID
}

func TestInvitationCreateSendsEmailAndPersistsHashOnly(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store, orgID, ownerID := seedOrg(t)
	sender := saas.NewFakeInviteEmailSender()
	svc := saas.NewInvitationService(store, sender, saas.WithInvitationClock(fixedClock(now)))
	ctx := context.Background()

	inv, err := svc.CreateInvite(ctx, orgID, ownerID, "New.Person+tag@Example.com", saas.RoleMember)
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	if inv.Email != "new.person+tag@example.com" {
		t.Errorf("email not normalized: got %q", inv.Email)
	}
	if inv.State != saas.InvitationPending {
		t.Errorf("state = %q, want pending", inv.State)
	}
	if !inv.ExpiresAt.Equal(now.Add(7 * 24 * time.Hour)) {
		t.Errorf("ExpiresAt = %v, want now+7d", inv.ExpiresAt)
	}

	// The raw token was sent by email...
	token := sender.LastToken(inv.Email)
	if token == "" {
		t.Fatal("no invite email was sent")
	}
	// ...and the store holds only its hash, never the raw value.
	stored, err := store.GetInvitationByTokenHash(ctx, inv.TokenHash)
	if err != nil {
		t.Fatalf("GetInvitationByTokenHash: %v", err)
	}
	if stored.TokenHash == token {
		t.Error("the store must never hold the raw token as-is")
	}
	if stored.ID != inv.ID {
		t.Errorf("token hash resolves to a different invitation")
	}
}

func TestInvitationCreateDuplicatePendingConflict(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store, orgID, ownerID := seedOrg(t)
	svc := saas.NewInvitationService(store, saas.NewFakeInviteEmailSender(), saas.WithInvitationClock(fixedClock(now)))
	ctx := context.Background()

	if _, err := svc.CreateInvite(ctx, orgID, ownerID, "dup@example.com", saas.RoleMember); err != nil {
		t.Fatalf("first CreateInvite: %v", err)
	}
	_, err := svc.CreateInvite(ctx, orgID, ownerID, "dup@example.com", saas.RoleAdmin)
	if !errors.Is(err, saas.ErrInvitePending) {
		t.Errorf("second CreateInvite for same pending email: got %v, want ErrInvitePending", err)
	}
}

func TestInvitationCreateAllowedAfterPriorExpired(t *testing.T) {
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store, orgID, ownerID := seedOrg(t)
	sender := saas.NewFakeInviteEmailSender()
	svc := saas.NewInvitationService(store, sender, saas.WithInvitationClock(fixedClock(created)))
	ctx := context.Background()

	if _, err := svc.CreateInvite(ctx, orgID, ownerID, "stale@example.com", saas.RoleMember); err != nil {
		t.Fatalf("first CreateInvite: %v", err)
	}

	// Move the clock past the 7-day expiry: a fresh invite for the same email
	// must now be allowed (the old one is EFFECTIVELY expired even though its
	// stored state is still "pending").
	later := created.Add(8 * 24 * time.Hour)
	svcLater := saas.NewInvitationService(store, sender, saas.WithInvitationClock(fixedClock(later)))
	if _, err := svcLater.CreateInvite(ctx, orgID, ownerID, "stale@example.com", saas.RoleMember); err != nil {
		t.Errorf("CreateInvite after prior expired: got %v, want nil", err)
	}
}

func TestInvitationListReturnsMostRecentFirst(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store, orgID, ownerID := seedOrg(t)
	sender := saas.NewFakeInviteEmailSender()
	ctx := context.Background()

	svc1 := saas.NewInvitationService(store, sender, saas.WithInvitationClock(fixedClock(now)))
	first, err := svc1.CreateInvite(ctx, orgID, ownerID, "a@example.com", saas.RoleMember)
	if err != nil {
		t.Fatalf("CreateInvite a: %v", err)
	}
	svc2 := saas.NewInvitationService(store, sender, saas.WithInvitationClock(fixedClock(now.Add(time.Minute))))
	second, err := svc2.CreateInvite(ctx, orgID, ownerID, "b@example.com", saas.RoleMember)
	if err != nil {
		t.Fatalf("CreateInvite b: %v", err)
	}

	list, err := svc2.ListInvites(ctx, orgID)
	if err != nil {
		t.Fatalf("ListInvites: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListInvites: got %d, want 2", len(list))
	}
	if list[0].ID != second.ID || list[1].ID != first.ID {
		t.Errorf("ListInvites order: got [%s,%s], want [%s,%s]", list[0].ID, list[1].ID, second.ID, first.ID)
	}
}

func TestInvitationRevokeDeletesRow(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store, orgID, ownerID := seedOrg(t)
	svc := saas.NewInvitationService(store, saas.NewFakeInviteEmailSender(), saas.WithInvitationClock(fixedClock(now)))
	ctx := context.Background()

	inv, err := svc.CreateInvite(ctx, orgID, ownerID, "gone@example.com", saas.RoleMember)
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	revoked, err := svc.RevokeInvite(ctx, orgID, inv.ID)
	if err != nil {
		t.Fatalf("RevokeInvite: %v", err)
	}
	if revoked.Email != "gone@example.com" {
		t.Errorf("revoked invitation email = %q", revoked.Email)
	}
	if _, err := store.GetInvitationByTokenHash(ctx, inv.TokenHash); !errors.Is(err, saas.ErrNotFound) {
		t.Errorf("revoked invitation still resolvable by token hash: %v", err)
	}
	// Revoking cross-org (wrong org id) must not find it.
	if _, err := svc.RevokeInvite(ctx, "other-org", inv.ID); !errors.Is(err, saas.ErrNotFound) {
		t.Errorf("cross-org revoke: got %v, want ErrNotFound", err)
	}
}

func TestInvitationResendMintsFreshTokenAndKeepsPending(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store, orgID, ownerID := seedOrg(t)
	sender := saas.NewFakeInviteEmailSender()
	svc := saas.NewInvitationService(store, sender, saas.WithInvitationClock(fixedClock(now)))
	ctx := context.Background()

	inv, err := svc.CreateInvite(ctx, orgID, ownerID, "resend@example.com", saas.RoleMember)
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	oldToken := sender.LastToken("resend@example.com")

	resent, err := svc.ResendInvite(ctx, orgID, inv.ID)
	if err != nil {
		t.Fatalf("ResendInvite: %v", err)
	}
	if resent.ID == inv.ID {
		t.Error("resend should mint a fresh invitation row (new id)")
	}
	if resent.State != saas.InvitationPending {
		t.Errorf("resent state = %q, want pending", resent.State)
	}
	newToken := sender.LastToken("resend@example.com")
	if newToken == "" || newToken == oldToken {
		t.Error("resend must mint and send a fresh token, not reuse the old one")
	}
	// The old invitation row is gone.
	if _, err := store.GetInvitationByTokenHash(ctx, inv.TokenHash); !errors.Is(err, saas.ErrNotFound) {
		t.Errorf("old invitation row should be removed after resend, got %v", err)
	}

	list, err := svc.ListInvites(ctx, orgID)
	if err != nil {
		t.Fatalf("ListInvites: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListInvites after resend: got %d, want 1", len(list))
	}
}

func TestInvitationResendRefusesAlreadyAccepted(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store, orgID, ownerID := seedOrg(t)
	sender := saas.NewFakeInviteEmailSender()
	svc := saas.NewInvitationService(store, sender, saas.WithInvitationClock(fixedClock(now)))
	ctx := context.Background()

	inv, err := svc.CreateInvite(ctx, orgID, ownerID, "acc@example.com", saas.RoleMember)
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	token := sender.LastToken("acc@example.com")
	if _, err := svc.AcceptInvite(ctx, "newacct", "acc@example.com", token); err != nil {
		t.Fatalf("AcceptInvite: %v", err)
	}
	if _, err := svc.ResendInvite(ctx, orgID, inv.ID); !errors.Is(err, saas.ErrInviteNotPending) {
		t.Errorf("resend of accepted invite: got %v, want ErrInviteNotPending", err)
	}
}

func TestInvitationAcceptExactEmailMatch(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store, orgID, ownerID := seedOrg(t)
	sender := saas.NewFakeInviteEmailSender()
	svc := saas.NewInvitationService(store, sender, saas.WithInvitationClock(fixedClock(now)))
	ctx := context.Background()

	_, err := svc.CreateInvite(ctx, orgID, ownerID, "carol@example.com", saas.RoleAdmin)
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	token := sender.LastToken("carol@example.com")

	accepted, err := svc.AcceptInvite(ctx, "acct-carol", "carol@example.com", token)
	if err != nil {
		t.Fatalf("AcceptInvite: %v", err)
	}
	if accepted.State != saas.InvitationAccepted {
		t.Errorf("accepted.State = %q, want accepted", accepted.State)
	}
	mems, err := store.ListOrgMembers(ctx, orgID)
	if err != nil {
		t.Fatalf("ListOrgMembers: %v", err)
	}
	found := false
	for _, m := range mems {
		if m.AccountID == "acct-carol" && m.Role == saas.RoleAdmin {
			found = true
		}
	}
	if !found {
		t.Error("accepted invite did not create a membership at the invited role")
	}
}

func TestInvitationAcceptCorporateDomainMatch(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store, orgID, ownerID := seedOrg(t)
	sender := saas.NewFakeInviteEmailSender()
	svc := saas.NewInvitationService(store, sender, saas.WithInvitationClock(fixedClock(now)))
	ctx := context.Background()

	_, err := svc.CreateInvite(ctx, orgID, ownerID, "team@corp-example.com", saas.RoleMember)
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	token := sender.LastToken("team@corp-example.com")

	// A different mailbox on the SAME non-consumer domain may accept.
	if _, err := svc.AcceptInvite(ctx, "acct-dave", "dave@corp-example.com", token); err != nil {
		t.Errorf("same-domain accept: got %v, want nil", err)
	}
}

func TestInvitationAcceptConsumerDomainRequiresExactMatch(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store, orgID, ownerID := seedOrg(t)
	sender := saas.NewFakeInviteEmailSender()
	svc := saas.NewInvitationService(store, sender, saas.WithInvitationClock(fixedClock(now)))
	ctx := context.Background()

	_, err := svc.CreateInvite(ctx, orgID, ownerID, "someone@gmail.com", saas.RoleMember)
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	token := sender.LastToken("someone@gmail.com")

	if _, err := svc.AcceptInvite(ctx, "acct-x", "other@gmail.com", token); !errors.Is(err, saas.ErrInviteEmailMismatch) {
		t.Errorf("different gmail.com mailbox: got %v, want ErrInviteEmailMismatch", err)
	}
	if _, err := svc.AcceptInvite(ctx, "acct-y", "someone@gmail.com", token); err != nil {
		t.Errorf("exact gmail.com match: got %v, want nil", err)
	}
}

func TestInvitationAcceptExpired(t *testing.T) {
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store, orgID, ownerID := seedOrg(t)
	sender := saas.NewFakeInviteEmailSender()
	svc := saas.NewInvitationService(store, sender, saas.WithInvitationClock(fixedClock(created)))
	ctx := context.Background()

	_, err := svc.CreateInvite(ctx, orgID, ownerID, "late@example.com", saas.RoleMember)
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	token := sender.LastToken("late@example.com")

	later := created.Add(8 * 24 * time.Hour)
	svcLater := saas.NewInvitationService(store, sender, saas.WithInvitationClock(fixedClock(later)))
	if _, err := svcLater.AcceptInvite(ctx, "acct-late", "late@example.com", token); !errors.Is(err, saas.ErrInviteExpired) {
		t.Errorf("accept past expiry: got %v, want ErrInviteExpired", err)
	}
}

func TestInvitationAcceptUnknownToken(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store, _, _ := seedOrg(t)
	svc := saas.NewInvitationService(store, saas.NewFakeInviteEmailSender(), saas.WithInvitationClock(fixedClock(now)))
	ctx := context.Background()

	if _, err := svc.AcceptInvite(ctx, "acct-x", "x@example.com", "not-a-real-token"); !errors.Is(err, saas.ErrNotFound) {
		t.Errorf("unknown token: got %v, want ErrNotFound", err)
	}
}

func TestInvitationLookupMasksEmailAndReportsEffectiveState(t *testing.T) {
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store, orgID, ownerID := seedOrg(t)
	sender := saas.NewFakeInviteEmailSender()
	svc := saas.NewInvitationService(store, sender, saas.WithInvitationClock(fixedClock(created)))
	ctx := context.Background()

	_, err := svc.CreateInvite(ctx, orgID, ownerID, "hidden@example.com", saas.RoleMember)
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	token := sender.LastToken("hidden@example.com")

	look, err := svc.LookupInvite(ctx, token)
	if err != nil {
		t.Fatalf("LookupInvite: %v", err)
	}
	if look.EmailHint == "hidden@example.com" {
		t.Error("LookupInvite must not return the full email")
	}
	if look.OrgName == "" {
		t.Error("LookupInvite should resolve the org name")
	}
	if look.State != saas.InvitationPending {
		t.Errorf("State = %q, want pending", look.State)
	}

	// Past expiry, the SAME stored row reads as expired.
	later := created.Add(8 * 24 * time.Hour)
	svcLater := saas.NewInvitationService(store, sender, saas.WithInvitationClock(fixedClock(later)))
	look2, err := svcLater.LookupInvite(ctx, token)
	if err != nil {
		t.Fatalf("LookupInvite (expired): %v", err)
	}
	if look2.State != saas.InvitationExpired {
		t.Errorf("State after expiry = %q, want expired", look2.State)
	}
}

func TestInvitationEffectiveStateNeverWritesBack(t *testing.T) {
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	inv := saas.Invitation{State: saas.InvitationPending, ExpiresAt: created.Add(time.Hour)}
	later := created.Add(2 * time.Hour)
	if got := inv.EffectiveState(later); got != saas.InvitationExpired {
		t.Errorf("EffectiveState = %q, want expired", got)
	}
	// The stored State field itself is untouched: EffectiveState is a pure read.
	if inv.State != saas.InvitationPending {
		t.Errorf("EffectiveState mutated the stored state: %q", inv.State)
	}
}

func TestRemoveMemberLastOwnerRefused(t *testing.T) {
	ctx := context.Background()
	store := saas.NewMemStore()
	keys := saas.NewKeyService(store)
	accounts := saas.NewAccountService(store, keys)
	owner, org, err := accounts.SignUp(ctx, "sole-owner@example.com")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	if err := accounts.RemoveMember(ctx, owner.ID, org.ID, owner.ID); !errors.Is(err, saas.ErrLastOwner) {
		t.Errorf("removing sole owner: got %v, want ErrLastOwner", err)
	}
}

func TestRemoveMemberSelfAllowedWithoutManagePermission(t *testing.T) {
	ctx := context.Background()
	store := saas.NewMemStore()
	keys := saas.NewKeyService(store)
	accounts := saas.NewAccountService(store, keys)
	owner, org, err := accounts.SignUp(ctx, "owner-x@example.com")
	if err != nil {
		t.Fatalf("SignUp owner: %v", err)
	}
	viewer, _, err := accounts.SignUp(ctx, "viewer-x@example.com")
	if err != nil {
		t.Fatalf("SignUp viewer: %v", err)
	}
	if err := store.PutMembership(ctx, saas.Membership{AccountID: viewer.ID, OrgID: org.ID, Role: saas.RoleViewer}); err != nil {
		t.Fatalf("seed viewer membership: %v", err)
	}
	// The viewer removing someone ELSE (the owner) is forbidden.
	if err := accounts.RemoveMember(ctx, viewer.ID, org.ID, owner.ID); !errors.Is(err, saas.ErrForbidden) {
		t.Errorf("viewer removing owner: got %v, want ErrForbidden", err)
	}
	// But the viewer removing THEMSELVES is allowed even without PermManageMembers.
	if err := accounts.RemoveMember(ctx, viewer.ID, org.ID, viewer.ID); err != nil {
		t.Errorf("self-removal: got %v, want nil", err)
	}
}
