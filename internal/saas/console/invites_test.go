package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mitos.run/mitos/internal/saas"
)

// doReq builds an authenticated request against con, mirroring fixture.req in
// console_test.go, but usable from fixtures other than the shared `fixture`
// type (invites need their own fixture wiring Deps.Invitations).
func doReq(t *testing.T, con *Console, method, target, body, acct, org string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	r = r.WithContext(WithCaller(r.Context(), acct, org))
	w := httptest.NewRecorder()
	con.ServeHTTP(w, r)
	return w
}

// inviteFixture wires a Console with an InvitationService sharing the SAME
// store as the AccountService, so lookups of the inviter's own name resolve.
// alice owns aliceOrg; carol is a plain member of aliceOrg (no
// PermManageMembers) used to assert the RBAC gate.
type inviteFixture struct {
	con     *Console
	store   *saas.MemStore
	sender  *saas.FakeInviteEmailSender
	aliceID string
	carolID string
	orgID   string
	// bobID owns a SEPARATE org (bobOrgID), used to prove that an actor with
	// PermManageMembers in their OWN org still cannot reach another org's
	// invitation by id (cross-org isolation, as opposed to the RBAC gate).
	bobID    string
	bobOrgID string
	now      time.Time
}

func newInviteFixture(t *testing.T) *inviteFixture {
	t.Helper()
	store := saas.NewMemStore()
	keys := saas.NewKeyService(store)
	accounts := saas.NewAccountService(store, keys)
	ctx := t.Context()

	alice, org, err := accounts.SignUp(ctx, "alice-invites@example.com")
	if err != nil {
		t.Fatalf("SignUp alice: %v", err)
	}
	carol, _, err := accounts.SignUp(ctx, "carol-invites@example.com")
	if err != nil {
		t.Fatalf("SignUp carol: %v", err)
	}
	if err := store.PutMembership(ctx, saas.Membership{AccountID: carol.ID, OrgID: org.ID, Role: saas.RoleMember}); err != nil {
		t.Fatalf("seed carol membership: %v", err)
	}
	bob, bobOrg, err := accounts.SignUp(ctx, "bob-invites@example.com")
	if err != nil {
		t.Fatalf("SignUp bob: %v", err)
	}

	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	sender := saas.NewFakeInviteEmailSender()
	invites := saas.NewInvitationService(store, sender, saas.WithInvitationClock(func() time.Time { return now }))

	con := New(Deps{
		Accounts:    accounts,
		Invitations: invites,
		Audit:       NewMemAuditLog(),
		Now:         func() time.Time { return now },
	})
	return &inviteFixture{
		con: con, store: store, sender: sender,
		aliceID: alice.ID, carolID: carol.ID, orgID: org.ID,
		bobID: bob.ID, bobOrgID: bobOrg.ID, now: now,
	}
}

func (f *inviteFixture) req(t *testing.T, method, target, body, acct, org string) *httptest.ResponseRecorder {
	t.Helper()
	return doReq(t, f.con, method, target, body, acct, org)
}

func TestCreateInviteRequiresManageMembers(t *testing.T) {
	f := newInviteFixture(t)
	w := f.req(t, "POST", "/console/invites", `{"email":"new@example.com","role":"member"}`, f.carolID, f.orgID)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s, want 403", w.Code, w.Body.String())
	}
}

func TestCreateInviteSendsEmailListsAndAudits(t *testing.T) {
	f := newInviteFixture(t)
	w := f.req(t, "POST", "/console/invites", `{"email":"New@Example.com","role":"admin"}`, f.aliceID, f.orgID)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var view InvitationView
	decode(t, w, &view)
	if view.Email != "new@example.com" {
		t.Errorf("Email = %q", view.Email)
	}
	if view.Role != saas.RoleAdmin {
		t.Errorf("Role = %q", view.Role)
	}
	if view.State != saas.InvitationPending {
		t.Errorf("State = %q", view.State)
	}
	if view.InviterName == "" {
		t.Error("InviterName should resolve to alice's account")
	}

	if token := f.sender.LastToken("new@example.com"); token == "" {
		t.Error("no invite email was sent")
	}

	wl := f.req(t, "GET", "/console/invites", "", f.aliceID, f.orgID)
	if wl.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", wl.Code, wl.Body.String())
	}
	var listResp struct {
		Invitations []InvitationView `json:"invitations"`
	}
	decode(t, wl, &listResp)
	if len(listResp.Invitations) != 1 {
		t.Fatalf("invitations listed: got %d, want 1", len(listResp.Invitations))
	}

	events, err := f.con.deps.Audit.List(t.Context(), f.orgID, 10)
	if err != nil {
		t.Fatalf("audit list: %v", err)
	}
	found := false
	for _, e := range events {
		if e.Action == "invite.create" && e.TargetType == "invite" && e.TargetName == "new@example.com" {
			found = true
		}
	}
	if !found {
		t.Errorf("no invite.create audit event found: %+v", events)
	}
}

func TestCreateInviteDuplicatePendingReturnsInvalidInput(t *testing.T) {
	f := newInviteFixture(t)
	if w := f.req(t, "POST", "/console/invites", `{"email":"dup@example.com"}`, f.aliceID, f.orgID); w.Code != http.StatusCreated {
		t.Fatalf("first create: %d %s", w.Code, w.Body.String())
	}
	w := f.req(t, "POST", "/console/invites", `{"email":"dup@example.com"}`, f.aliceID, f.orgID)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("duplicate create status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

func TestCreateInviteRateLimited(t *testing.T) {
	f := newInviteFixture(t)
	f.con.inviteRateLimit = newInviteRateLimiter(1, 24*time.Hour)
	if w := f.req(t, "POST", "/console/invites", `{"email":"one@example.com"}`, f.aliceID, f.orgID); w.Code != http.StatusCreated {
		t.Fatalf("first create: %d %s", w.Code, w.Body.String())
	}
	w := f.req(t, "POST", "/console/invites", `{"email":"two@example.com"}`, f.aliceID, f.orgID)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second create status = %d, want 429: %s", w.Code, w.Body.String())
	}
}

func TestCreateInviteDuplicateRejectionDoesNotConsumeQuota(t *testing.T) {
	f := newInviteFixture(t)
	f.con.inviteRateLimit = newInviteRateLimiter(2, 24*time.Hour)

	if w := f.req(t, "POST", "/console/invites", `{"email":"quota-dup@example.com"}`, f.aliceID, f.orgID); w.Code != http.StatusCreated {
		t.Fatalf("first create: %d %s", w.Code, w.Body.String())
	}
	// A duplicate of the still-pending email is rejected...
	if w := f.req(t, "POST", "/console/invites", `{"email":"quota-dup@example.com"}`, f.aliceID, f.orgID); w.Code != http.StatusBadRequest {
		t.Fatalf("duplicate create: %d %s, want 400", w.Code, w.Body.String())
	}
	// ...and must NOT have consumed the second slot of the limit=2 budget: a
	// genuinely new invite still fits.
	if w := f.req(t, "POST", "/console/invites", `{"email":"quota-new@example.com"}`, f.aliceID, f.orgID); w.Code != http.StatusCreated {
		t.Fatalf("third create (new email): %d %s, want 201 (duplicate rejection must not consume quota)", w.Code, w.Body.String())
	}
}

func TestResendInviteNotFoundDoesNotConsumeQuota(t *testing.T) {
	f := newInviteFixture(t)
	f.con.inviteRateLimit = newInviteRateLimiter(1, 24*time.Hour)

	// A resend of a nonexistent invitation id is rejected...
	if w := f.req(t, "POST", "/console/invites/does-not-exist/resend", "", f.aliceID, f.orgID); w.Code != http.StatusNotFound {
		t.Fatalf("resend of unknown id: %d %s, want 404", w.Code, w.Body.String())
	}
	// ...and must not have consumed the single slot of the limit=1 budget: a
	// genuine create still fits.
	if w := f.req(t, "POST", "/console/invites", `{"email":"after-notfound@example.com"}`, f.aliceID, f.orgID); w.Code != http.StatusCreated {
		t.Fatalf("create after not-found resend: %d %s, want 201 (not-found resend must not consume quota)", w.Code, w.Body.String())
	}
}

func TestRevokeInviteAndCrossOrgIsolation(t *testing.T) {
	f := newInviteFixture(t)
	w := f.req(t, "POST", "/console/invites", `{"email":"revoke-me@example.com"}`, f.aliceID, f.orgID)
	var view InvitationView
	decode(t, w, &view)

	// bob is a real owner authorized to manage members, but in HIS OWN org:
	// he must not be able to reach alice's invitation by id.
	wOther := f.req(t, "DELETE", "/console/invites/"+view.ID, "", f.bobID, f.bobOrgID)
	if wOther.Code != http.StatusNotFound {
		t.Fatalf("cross-org revoke status = %d, want 404: %s", wOther.Code, wOther.Body.String())
	}

	wRevoke := f.req(t, "DELETE", "/console/invites/"+view.ID, "", f.aliceID, f.orgID)
	if wRevoke.Code != http.StatusOK {
		t.Fatalf("revoke status = %d body=%s", wRevoke.Code, wRevoke.Body.String())
	}

	wl := f.req(t, "GET", "/console/invites", "", f.aliceID, f.orgID)
	var listResp struct {
		Invitations []InvitationView `json:"invitations"`
	}
	decode(t, wl, &listResp)
	if len(listResp.Invitations) != 0 {
		t.Errorf("invitations after revoke: got %d, want 0", len(listResp.Invitations))
	}
}

func TestResendInviteMintsFreshInvitation(t *testing.T) {
	f := newInviteFixture(t)
	w := f.req(t, "POST", "/console/invites", `{"email":"resend-me@example.com"}`, f.aliceID, f.orgID)
	var view InvitationView
	decode(t, w, &view)
	oldToken := f.sender.LastToken("resend-me@example.com")

	wResend := f.req(t, "POST", "/console/invites/"+view.ID+"/resend", "", f.aliceID, f.orgID)
	if wResend.Code != http.StatusOK {
		t.Fatalf("resend status = %d body=%s", wResend.Code, wResend.Body.String())
	}
	var resent InvitationView
	decode(t, wResend, &resent)
	if resent.ID == view.ID {
		t.Error("resend should mint a new invitation id")
	}
	if newToken := f.sender.LastToken("resend-me@example.com"); newToken == "" || newToken == oldToken {
		t.Error("resend should mint and send a fresh token")
	}
}

func TestRemoveMemberForbiddenWithoutPermission(t *testing.T) {
	f := newInviteFixture(t)
	w := f.req(t, "DELETE", "/console/members/"+f.aliceID, "", f.carolID, f.orgID)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s, want 403", w.Code, w.Body.String())
	}
}

func TestRemoveMemberSelfAllowed(t *testing.T) {
	f := newInviteFixture(t)
	w := f.req(t, "DELETE", "/console/members/"+f.carolID, "", f.carolID, f.orgID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	events, err := f.con.deps.Audit.List(t.Context(), f.orgID, 10)
	if err != nil {
		t.Fatalf("audit list: %v", err)
	}
	found := false
	for _, e := range events {
		if e.Action == "member.remove" && e.Target == f.carolID {
			found = true
		}
	}
	if !found {
		t.Error("no member.remove audit event found")
	}
}

func TestRemoveMemberLastOwnerRefused(t *testing.T) {
	f := newInviteFixture(t)
	w := f.req(t, "DELETE", "/console/members/"+f.aliceID, "", f.aliceID, f.orgID)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("removing sole owner status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

func TestLookupInviteIsPublicAndMasksEmail(t *testing.T) {
	f := newInviteFixture(t)
	w := f.req(t, "POST", "/console/invites", `{"email":"lookup-me@example.com"}`, f.aliceID, f.orgID)
	var view InvitationView
	decode(t, w, &view)
	token := f.sender.LastToken("lookup-me@example.com")

	// LookupInvite is called directly (as main.go mounts it), with NO caller
	// context attached at all: it must not require a session.
	r := httptest.NewRequest("GET", "/console/invites/lookup?token="+token, nil)
	rr := httptest.NewRecorder()
	f.con.LookupInvite(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var look InviteLookupView
	decode(t, rr, &look)
	if look.EmailHint == "lookup-me@example.com" {
		t.Error("lookup must not return the full email")
	}
	if look.OrgName == "" {
		t.Error("lookup should resolve the org name")
	}
	if look.State != saas.InvitationPending {
		t.Errorf("State = %q, want pending", look.State)
	}
}

func TestLookupInviteUnknownTokenIs404(t *testing.T) {
	f := newInviteFixture(t)
	r := httptest.NewRequest("GET", "/console/invites/lookup?token=bogus", nil)
	rr := httptest.NewRecorder()
	f.con.LookupInvite(rr, r)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rr.Code, rr.Body.String())
	}
}

func TestAcceptInviteRequiresSession(t *testing.T) {
	f := newInviteFixture(t)
	w := f.req(t, "POST", "/console/invites", `{"email":"accept-me@example.com"}`, f.aliceID, f.orgID)
	var view InvitationView
	decode(t, w, &view)
	token := f.sender.LastToken("accept-me@example.com")

	// No caller context attached: unauthenticated.
	r := httptest.NewRequest("POST", "/console/invites/accept", strings.NewReader(`{"token":"`+token+`"}`))
	rr := httptest.NewRecorder()
	f.con.ServeHTTP(rr, r)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s, want 401", rr.Code, rr.Body.String())
	}
}

func TestAcceptInviteJoinsOrgAndAudits(t *testing.T) {
	f := newInviteFixture(t)
	w := f.req(t, "POST", "/console/invites", `{"email":"dave-invites@example.com","role":"admin"}`, f.aliceID, f.orgID)
	var view InvitationView
	decode(t, w, &view)
	token := f.sender.LastToken("dave-invites@example.com")

	// dave is an EXISTING account (a member of bob's org) whose email exactly
	// matches the invite; his session is for bob's org (accept must still
	// join HIM to alice's org, the invite's org, not his session's org).
	ctx := t.Context()
	keys := saas.NewKeyService(f.store)
	accounts := saas.NewAccountService(f.store, keys)
	dave, _, err := accounts.SignUp(ctx, "dave-invites@example.com")
	if err != nil {
		t.Fatalf("SignUp dave: %v", err)
	}

	wa := f.req(t, "POST", "/console/invites/accept", `{"token":"`+token+`"}`, dave.ID, f.bobOrgID)
	if wa.Code != http.StatusOK {
		t.Fatalf("accept status = %d body=%s", wa.Code, wa.Body.String())
	}
	var resp struct {
		OrgID string    `json:"org_id"`
		Role  saas.Role `json:"role"`
	}
	decode(t, wa, &resp)
	if resp.OrgID != f.orgID {
		t.Errorf("accept joined org %q, want %q (the invite's org)", resp.OrgID, f.orgID)
	}
	if resp.Role != saas.RoleAdmin {
		t.Errorf("accept role = %q, want admin", resp.Role)
	}

	mems, err := f.store.ListOrgMembers(ctx, f.orgID)
	if err != nil {
		t.Fatalf("ListOrgMembers: %v", err)
	}
	found := false
	for _, m := range mems {
		if m.AccountID == dave.ID && m.Role == saas.RoleAdmin {
			found = true
		}
	}
	if !found {
		t.Error("dave was not added as an admin member of alice's org")
	}

	events, err := f.con.deps.Audit.List(ctx, f.orgID, 10)
	if err != nil {
		t.Fatalf("audit list: %v", err)
	}
	foundAudit := false
	for _, e := range events {
		if e.Action == "invite.accept" && e.ActorID == dave.ID {
			foundAudit = true
		}
	}
	if !foundAudit {
		t.Errorf("no invite.accept audit event found: %+v", events)
	}
}

func TestAcceptInviteEmailMismatchForbidden(t *testing.T) {
	f := newInviteFixture(t)
	// Invite a gmail.com address: the consumer-domain rule requires an EXACT
	// match, so bob (whose account email is on a different domain entirely)
	// cannot accept it.
	w := f.req(t, "POST", "/console/invites", `{"email":"only-for-carol@gmail.com"}`, f.aliceID, f.orgID)
	var view InvitationView
	decode(t, w, &view)
	token := f.sender.LastToken("only-for-carol@gmail.com")

	wa := f.req(t, "POST", "/console/invites/accept", `{"token":"`+token+`"}`, f.bobID, f.bobOrgID)
	if wa.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s, want 403", wa.Code, wa.Body.String())
	}
}

func TestInvitesUnavailableWhenNotConfigured(t *testing.T) {
	con := New(Deps{Accounts: saas.NewAccountService(saas.NewMemStore(), saas.NewKeyService(saas.NewMemStore()))})
	w := doReq(t, con, "GET", "/console/invites", "", "acct", "org")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (not enabled): %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "not enabled") {
		t.Errorf("body should explain invites are not enabled: %s", w.Body.String())
	}
}
