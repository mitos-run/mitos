package onboarding

import (
	"context"
	"testing"
	"time"

	"mitos.run/mitos/internal/saas"
)

// seedInvite writes a pending invitation directly into store (bypassing
// saas.InvitationService, which lives in a different package and would
// create an import cycle from this test) so the onboarding auto-join hook
// can be exercised in isolation.
func seedInvite(t *testing.T, store saas.Store, orgID, email string, expiresAt time.Time) (inv saas.Invitation, rawToken string) {
	t.Helper()
	rawToken = "invite-raw-token-for-" + email
	sum := hashString(rawToken)
	inv = saas.Invitation{
		ID: "inv-" + email, OrgID: orgID, Email: email, Role: saas.RoleAdmin,
		TokenHash: sum, State: saas.InvitationPending, InviterID: "inviter-acct",
		CreatedAt: expiresAt.Add(-7 * 24 * time.Hour), ExpiresAt: expiresAt,
	}
	if err := store.CreateInvitation(context.Background(), inv); err != nil {
		t.Fatalf("seed invitation: %v", err)
	}
	return inv, rawToken
}

func TestVerifyAutoJoinsPendingInviteMatchingEmail(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ModeOpen)

	// A pending invite already exists for an org unrelated to the fresh
	// signup below, addressed to the SAME email the signup will use.
	inv, rawToken := seedInvite(t, h.store, "invited-org-1", "dev@example.com", h.now.Add(24*time.Hour))

	res, err := h.svc.SignUpWithInvite(ctx, "dev@example.com", "", rawToken)
	if err != nil {
		t.Fatalf("sign up: %v", err)
	}
	verifyToken := h.email.LastToken("dev@example.com")

	out, err := h.svc.Verify(ctx, verifyToken)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}

	// The account still gets its OWN Personal org...
	if out.Org.ID == inv.OrgID {
		t.Fatalf("Verify's own Org should be the fresh Personal org, not the invited org")
	}
	// ...AND auto-joins the invited org, in addition to (not instead of) it.
	mems, err := h.store.ListMemberships(ctx, out.Account.ID)
	if err != nil {
		t.Fatalf("ListMemberships: %v", err)
	}
	var joinedInvited bool
	for _, m := range mems {
		if m.OrgID == inv.OrgID && m.Role == saas.RoleAdmin {
			joinedInvited = true
		}
	}
	if !joinedInvited {
		t.Errorf("account did not auto-join the invited org: memberships=%+v", mems)
	}

	// The invitation is marked accepted.
	got, err := h.store.GetInvitationByTokenHash(ctx, inv.TokenHash)
	if err != nil {
		t.Fatalf("GetInvitationByTokenHash: %v", err)
	}
	if got.State != saas.InvitationAccepted {
		t.Errorf("invitation state = %q, want accepted", got.State)
	}

	_ = res
}

func TestVerifyWithNoInviteTokenBehavesLikePlainSignUp(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ModeOpen)

	if _, err := h.svc.SignUpWithInvite(ctx, "plain@example.com", "", ""); err != nil {
		t.Fatalf("sign up: %v", err)
	}
	verifyToken := h.email.LastToken("plain@example.com")
	out, err := h.svc.Verify(ctx, verifyToken)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	mems, err := h.store.ListMemberships(ctx, out.Account.ID)
	if err != nil {
		t.Fatalf("ListMemberships: %v", err)
	}
	if len(mems) != 1 {
		t.Fatalf("memberships = %+v, want exactly the Personal org", mems)
	}
}

// TestVerifyAutoJoinsGmailAliasInvite asserts the auto-join comparison uses
// the SAME canonicalization signup dedup does: an invite addressed to a
// Gmail dotted/plus-tagged alias of the address the signup actually
// verifies with must still auto-join, even though saas.InviteEmailMatches
// alone (gmail.com is a consumer domain there, requiring an exact address
// match) would refuse it.
func TestVerifyAutoJoinsGmailAliasInvite(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, ModeOpen)

	// Invite addressed to a dotted alias; the signup verifies with the
	// canonical (undotted) address.
	inv, rawToken := seedInvite(t, h.store, "invited-org-gmail", "j.ohn@gmail.com", h.now.Add(24*time.Hour))

	if _, err := h.svc.SignUpWithInvite(ctx, "john@gmail.com", "", rawToken); err != nil {
		t.Fatalf("sign up: %v", err)
	}
	verifyToken := h.email.LastToken("john@gmail.com")
	out, err := h.svc.Verify(ctx, verifyToken)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}

	mems, err := h.store.ListMemberships(ctx, out.Account.ID)
	if err != nil {
		t.Fatalf("ListMemberships: %v", err)
	}
	var joinedInvited bool
	for _, m := range mems {
		if m.OrgID == inv.OrgID {
			joinedInvited = true
		}
	}
	if !joinedInvited {
		t.Errorf("account did not auto-join the invited org for a Gmail dotted-alias invite: memberships=%+v", mems)
	}

	got, err := h.store.GetInvitationByTokenHash(ctx, inv.TokenHash)
	if err != nil {
		t.Fatalf("GetInvitationByTokenHash: %v", err)
	}
	if got.State != saas.InvitationAccepted {
		t.Errorf("invitation state = %q, want accepted", got.State)
	}
}

func TestVerifyIgnoresExpiredOrMismatchedInvite(t *testing.T) {
	ctx := context.Background()

	t.Run("expired", func(t *testing.T) {
		h := newHarness(t, ModeOpen)
		inv, rawToken := seedInvite(t, h.store, "invited-org-2", "late@example.com", h.now.Add(-time.Hour))
		if _, err := h.svc.SignUpWithInvite(ctx, "late@example.com", "", rawToken); err != nil {
			t.Fatalf("sign up: %v", err)
		}
		verifyToken := h.email.LastToken("late@example.com")
		out, err := h.svc.Verify(ctx, verifyToken)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		mems, err := h.store.ListMemberships(ctx, out.Account.ID)
		if err != nil {
			t.Fatalf("ListMemberships: %v", err)
		}
		for _, m := range mems {
			if m.OrgID == inv.OrgID {
				t.Errorf("must not join an EXPIRED invitation's org")
			}
		}
	})

	t.Run("email mismatch", func(t *testing.T) {
		h := newHarness(t, ModeOpen)
		inv, rawToken := seedInvite(t, h.store, "invited-org-3", "someone@gmail.com", h.now.Add(24*time.Hour))
		if _, err := h.svc.SignUpWithInvite(ctx, "different@gmail.com", "", rawToken); err != nil {
			t.Fatalf("sign up: %v", err)
		}
		verifyToken := h.email.LastToken("different@gmail.com")
		out, err := h.svc.Verify(ctx, verifyToken)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		mems, err := h.store.ListMemberships(ctx, out.Account.ID)
		if err != nil {
			t.Fatalf("ListMemberships: %v", err)
		}
		for _, m := range mems {
			if m.OrgID == inv.OrgID {
				t.Errorf("must not join when the verified email does not match the invite (gmail.com requires exact match)")
			}
		}
	})
}
