package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"mitos.run/mitos/internal/saas"
)

// adminFixture builds a Console wired with a real AccountService/Store (so
// isInstanceAdmin can resolve accounts, orgs, and membership roles) plus an
// org-scoped MemSandboxControl for the overview/orgs rollups.
type adminFixture struct {
	con       *Console
	accounts  *saas.AccountService
	store     *saas.MemStore
	sandboxes *MemSandboxControl
}

func newAdminFixture(t *testing.T, deps Deps) *adminFixture {
	t.Helper()
	store := saas.NewMemStore()
	keys := saas.NewKeyService(store)
	accounts := saas.NewAccountService(store, keys)
	sandboxes := NewMemSandboxControl()
	deps.Accounts = accounts
	deps.Orgs = store
	deps.Sandboxes = sandboxes
	if deps.Audit == nil {
		deps.Audit = NewMemAuditLog()
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	con := New(deps)
	return &adminFixture{con: con, accounts: accounts, store: store, sandboxes: sandboxes}
}

func (f *adminFixture) req(t *testing.T, method, path, acctID, orgID string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	if acctID != "" {
		r = r.WithContext(WithCaller(context.Background(), acctID, orgID))
	}
	w := httptest.NewRecorder()
	f.con.ServeHTTP(w, r)
	return w
}

// --- isInstanceAdmin ---

// TestInstanceAdminEmailAllowlist asserts an account whose email is in
// InstanceAdminEmails (case-insensitively) is an instance admin regardless of
// edition or org membership, and one that is not, is refused.
func TestInstanceAdminEmailAllowlist(t *testing.T) {
	f := newAdminFixture(t, Deps{
		Capabilities:        Capabilities{Edition: "hosted"},
		InstanceAdminEmails: []string{"Ops@Example.com"},
	})
	ctx := context.Background()
	ops, opsOrg, err := f.accounts.SignUp(ctx, "ops@example.com")
	if err != nil {
		t.Fatalf("SignUp ops: %v", err)
	}
	other, _, err := f.accounts.SignUp(ctx, "someone-else@example.com")
	if err != nil {
		t.Fatalf("SignUp other: %v", err)
	}
	if !f.con.isInstanceAdmin(ctx, ops.ID) {
		t.Error("expected ops@example.com to be an instance admin (case-insensitive allowlist match)")
	}
	if f.con.isInstanceAdmin(ctx, other.ID) {
		t.Error("expected someone-else@example.com to NOT be an instance admin")
	}
	_ = opsOrg
}

// TestInstanceAdminCommunityFallback asserts the community-edition,
// single-org-owner fallback: with exactly one org on the deployment, that
// org's owner is an instance admin with NO email configured, but the SAME
// account demoted to a non-owner role is not (a second SignUp would create
// its own personal org too, so the "exactly one org" precondition is tested
// by forcing the role on the SAME sole org rather than adding a second
// account).
func TestInstanceAdminCommunityFallback(t *testing.T) {
	f := newAdminFixture(t, Deps{Capabilities: Capabilities{Edition: "community"}})
	ctx := context.Background()
	owner, org, err := f.accounts.SignUp(ctx, "owner@example.com")
	if err != nil {
		t.Fatalf("SignUp owner: %v", err)
	}
	if !f.con.isInstanceAdmin(ctx, owner.ID) {
		t.Error("expected the sole org's owner to be an instance admin under community edition")
	}
	// Force the role to non-owner directly on the store (bypassing
	// AccountService.SetMemberRole's last-owner protection, which is
	// intentionally not testable via the normal API): still exactly one org,
	// same account, but no longer its owner.
	if err := f.store.PutMembership(ctx, saas.Membership{
		AccountID: owner.ID, OrgID: org.ID, Role: saas.RoleMember, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("force non-owner role: %v", err)
	}
	if f.con.isInstanceAdmin(ctx, owner.ID) {
		t.Error("expected a non-owner member to NOT be an instance admin, even as the sole org's only account")
	}
}

// TestInstanceAdminCommunityFallbackNeverAppliesToHosted asserts the
// single-org-owner fallback is gated on Edition == "community": a hosted
// deployment's first customer (also happening to be the only org) must NOT
// be silently promoted to instance admin.
func TestInstanceAdminCommunityFallbackNeverAppliesToHosted(t *testing.T) {
	f := newAdminFixture(t, Deps{Capabilities: Capabilities{Edition: "hosted"}})
	ctx := context.Background()
	owner, _, err := f.accounts.SignUp(ctx, "first-customer@example.com")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	if f.con.isInstanceAdmin(ctx, owner.ID) {
		t.Error("hosted edition must never grant instance-admin via the single-org fallback")
	}
}

// TestInstanceAdminCommunityFallbackNotAppliedWithMultipleOrgs asserts the
// fallback requires EXACTLY one org: with two orgs present, neither owner is
// auto-granted instance admin.
func TestInstanceAdminCommunityFallbackNotAppliedWithMultipleOrgs(t *testing.T) {
	f := newAdminFixture(t, Deps{Capabilities: Capabilities{Edition: "community"}})
	ctx := context.Background()
	ownerA, _, err := f.accounts.SignUp(ctx, "a@example.com")
	if err != nil {
		t.Fatalf("SignUp a: %v", err)
	}
	if _, _, err := f.accounts.SignUp(ctx, "b@example.com"); err != nil {
		t.Fatalf("SignUp b: %v", err)
	}
	if f.con.isInstanceAdmin(ctx, ownerA.ID) {
		t.Error("expected no instance admin when more than one org exists")
	}
}

// --- Capabilities.Admin ---

// TestCapabilitiesAdvertisesAdminForInstanceAdmin asserts GET
// /console/capabilities reports admin:true for an authenticated instance
// admin and admin:false for a regular caller and for an unauthenticated
// request.
func TestCapabilitiesAdvertisesAdminForInstanceAdmin(t *testing.T) {
	f := newAdminFixture(t, Deps{
		Capabilities:        Capabilities{Edition: "hosted"},
		InstanceAdminEmails: []string{"ops@example.com"},
	})
	ctx := context.Background()
	ops, opsOrg, err := f.accounts.SignUp(ctx, "ops@example.com")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	other, otherOrg, err := f.accounts.SignUp(ctx, "regular@example.com")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}

	w := f.req(t, "GET", "/console/capabilities", ops.ID, opsOrg.ID)
	var caps Capabilities
	decode(t, w, &caps)
	if !caps.Admin {
		t.Error("expected admin:true for the instance admin")
	}

	w = f.req(t, "GET", "/console/capabilities", other.ID, otherOrg.ID)
	decode(t, w, &caps)
	if caps.Admin {
		t.Error("expected admin:false for a regular caller")
	}

	w = f.req(t, "GET", "/console/capabilities", "", "")
	decode(t, w, &caps)
	if caps.Admin {
		t.Error("expected admin:false for an unauthenticated request")
	}
}

// --- GET /console/admin/overview ---

// TestAdminOverviewForbiddenForNonAdmin asserts a regular authenticated
// caller gets 403 from every /console/admin/... endpoint.
func TestAdminOverviewForbiddenForNonAdmin(t *testing.T) {
	f := newAdminFixture(t, Deps{Capabilities: Capabilities{Edition: "hosted"}})
	ctx := context.Background()
	regular, org, err := f.accounts.SignUp(ctx, "regular@example.com")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	w := f.req(t, "GET", "/console/admin/overview", regular.ID, org.ID)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
}

// TestAdminOverviewUnauthenticated asserts an unauthenticated request is
// refused (401), never leaking whether admin is even configured.
func TestAdminOverviewUnauthenticated(t *testing.T) {
	f := newAdminFixture(t, Deps{Capabilities: Capabilities{Edition: "hosted"}})
	w := f.req(t, "GET", "/console/admin/overview", "", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

// TestAdminOverviewShape asserts the overview reflects the true org count,
// running-sandbox rollup across orgs, and the signup mode, with nil node
// counts when no NodeSource is configured.
func TestAdminOverviewShape(t *testing.T) {
	f := newAdminFixture(t, Deps{
		Capabilities:        Capabilities{Edition: "hosted", Signup: false},
		InstanceAdminEmails: []string{"ops@example.com"},
	})
	ctx := context.Background()
	ops, opsOrg, err := f.accounts.SignUp(ctx, "ops@example.com")
	if err != nil {
		t.Fatalf("SignUp ops: %v", err)
	}
	_, org2, err := f.accounts.SignUp(ctx, "customer@example.com")
	if err != nil {
		t.Fatalf("SignUp customer: %v", err)
	}
	f.sandboxes.Add(SandboxView{ID: "sbx-1", OrgID: opsOrg.ID, Phase: "Running"})
	f.sandboxes.Add(SandboxView{ID: "sbx-2", OrgID: org2.ID, Phase: "Running"})
	f.sandboxes.Add(SandboxView{ID: "sbx-3", OrgID: org2.ID, Phase: "Terminated"})

	w := f.req(t, "GET", "/console/admin/overview", ops.ID, opsOrg.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var view AdminOverview
	decode(t, w, &view)
	if view.Orgs != 2 {
		t.Errorf("orgs = %d, want 2", view.Orgs)
	}
	if view.RunningSandboxes != 2 {
		t.Errorf("running_sandboxes = %d, want 2", view.RunningSandboxes)
	}
	if view.SignupMode != "waitlist" {
		t.Errorf("signup_mode = %q, want waitlist", view.SignupMode)
	}
	if view.NodesReady != nil || view.NodesTotal != nil {
		t.Errorf("expected nil node counts with no NodeSource configured, got ready=%v total=%v", view.NodesReady, view.NodesTotal)
	}
}

// TestAdminOverviewSignupModeOpen asserts signup_mode reflects caps.Signup.
func TestAdminOverviewSignupModeOpen(t *testing.T) {
	f := newAdminFixture(t, Deps{
		Capabilities:        Capabilities{Edition: "hosted", Signup: true},
		InstanceAdminEmails: []string{"ops@example.com"},
	})
	ops, org, err := f.accounts.SignUp(context.Background(), "ops@example.com")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	w := f.req(t, "GET", "/console/admin/overview", ops.ID, org.ID)
	var view AdminOverview
	decode(t, w, &view)
	if view.SignupMode != "open" {
		t.Errorf("signup_mode = %q, want open", view.SignupMode)
	}
}
