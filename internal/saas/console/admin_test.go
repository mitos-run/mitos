package console

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/billing"
	"mitos.run/mitos/internal/usage"
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

// --- GET /console/admin/orgs ---

// TestAdminOrgsForbiddenForNonAdmin asserts the orgs table is gated exactly
// like every other /console/admin/... endpoint.
func TestAdminOrgsForbiddenForNonAdmin(t *testing.T) {
	f := newAdminFixture(t, Deps{Capabilities: Capabilities{Edition: "hosted"}})
	regular, org, err := f.accounts.SignUp(context.Background(), "regular@example.com")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	w := f.req(t, "GET", "/console/admin/orgs", regular.ID, org.ID)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

// TestAdminOrgsShape asserts each row's tier, member count, running count,
// and month-to-date usage cents, and that Total is the true org count.
func TestAdminOrgsShape(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	usageStore := usage.NewMemUsageStore()
	f := newAdminFixture(t, Deps{
		Capabilities:        Capabilities{Edition: "hosted"},
		InstanceAdminEmails: []string{"ops@example.com"},
		Usage:               usageStore,
		Now:                 func() time.Time { return now },
	})
	ctx := context.Background()
	ops, opsOrg, err := f.accounts.SignUp(ctx, "ops@example.com")
	if err != nil {
		t.Fatalf("SignUp ops: %v", err)
	}
	_, teamOrg, err := f.accounts.SignUp(ctx, "team-customer@example.com")
	if err != nil {
		t.Fatalf("SignUp team customer: %v", err)
	}
	member, _, err := f.accounts.SignUp(ctx, "second-member@example.com")
	if err != nil {
		t.Fatalf("SignUp second member: %v", err)
	}
	if err := f.store.PutMembership(ctx, saas.Membership{
		AccountID: member.ID, OrgID: teamOrg.ID, Role: saas.RoleMember, CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	f.con.deps.Plans = billing.NewStaticPlanSource([]string{teamOrg.ID})
	f.sandboxes.Add(SandboxView{ID: "sbx-a", OrgID: teamOrg.ID, Phase: "Running"})

	// A within-month record (counted) and a prior-month record (excluded).
	if err := usageStore.UpsertRecord(ctx, usage.UsageRecord{OrgID: teamOrg.ID, SandboxID: "sbx-a", Window: now.Add(-time.Hour), VCPUSeconds: 3600}); err != nil {
		t.Fatalf("seed usage: %v", err)
	}
	if err := usageStore.UpsertRecord(ctx, usage.UsageRecord{OrgID: teamOrg.ID, SandboxID: "sbx-a", Window: now.AddDate(0, -1, 0), VCPUSeconds: 999_999}); err != nil {
		t.Fatalf("seed prior-month usage: %v", err)
	}

	w := f.req(t, "GET", "/console/admin/orgs", ops.ID, opsOrg.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Orgs  []AdminOrgView `json:"orgs"`
		Total int            `json:"total"`
	}
	decode(t, w, &body)
	// Three SignUps (ops, team-customer, second-member) each create their own
	// personal org; second-member is ADDITIONALLY seeded as a member of
	// teamOrg below, so the total org count is 3, not 2.
	if body.Total != 3 {
		t.Fatalf("total = %d, want 3", body.Total)
	}
	var teamRow *AdminOrgView
	for i := range body.Orgs {
		if body.Orgs[i].ID == teamOrg.ID {
			teamRow = &body.Orgs[i]
		}
	}
	if teamRow == nil {
		t.Fatalf("team org row not found in %+v", body.Orgs)
	}
	if teamRow.Tier != string(billing.PlanTeam) {
		t.Errorf("tier = %q, want %q", teamRow.Tier, billing.PlanTeam)
	}
	if teamRow.Members != 2 {
		t.Errorf("members = %d, want 2", teamRow.Members)
	}
	if teamRow.Running != 1 {
		t.Errorf("running = %d, want 1", teamRow.Running)
	}
	wantCents := int64(billing.DefaultRates().CostCents(usage.UsageRecord{VCPUSeconds: 3600}))
	if teamRow.MonthUsageCents != wantCents {
		t.Errorf("month_usage_cents = %d, want %d (prior-month record must be excluded)", teamRow.MonthUsageCents, wantCents)
	}
}

// --- Resilience: a per-org read failure must not abort the whole response ---

// failingOrgDirectory wraps a real OrgDirectory but makes ListOrgMembers fail
// for one specific org id (failOrgID), succeeding for every other org exactly
// as the wrapped directory would. This lets a test simulate a single bad org
// read (e.g. a transient store hiccup) without every org's read failing.
type failingOrgDirectory struct {
	OrgDirectory
	failOrgID string
	err       error
}

func (f *failingOrgDirectory) ListOrgMembers(ctx context.Context, orgID string) ([]saas.Membership, error) {
	if orgID == f.failOrgID {
		return nil, f.err
	}
	return f.OrgDirectory.ListOrgMembers(ctx, orgID)
}

// failingSandboxControl wraps a real SandboxControl but makes List fail for
// one specific org id, succeeding for every other org.
type failingSandboxControl struct {
	SandboxControl
	failOrgID string
	err       error
}

func (f *failingSandboxControl) List(ctx context.Context, orgID string) ([]SandboxView, error) {
	if orgID == f.failOrgID {
		return nil, f.err
	}
	return f.SandboxControl.List(ctx, orgID)
}

// TestAdminOverviewPerOrgSandboxErrorIsNotFatal asserts that when
// runningSandboxCount fails for exactly one org, GET /console/admin/overview
// still returns 200 with the other orgs' sandboxes rolled up, and reports
// that one org in failed_orgs rather than 500ing the whole request.
func TestAdminOverviewPerOrgSandboxErrorIsNotFatal(t *testing.T) {
	f := newAdminFixture(t, Deps{
		Capabilities:        Capabilities{Edition: "hosted"},
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
	f.con.deps.Sandboxes = &failingSandboxControl{
		SandboxControl: f.sandboxes,
		failOrgID:      org2.ID,
		err:            errors.New("simulated sandbox store outage"),
	}

	w := f.req(t, "GET", "/console/admin/overview", ops.ID, opsOrg.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var view AdminOverview
	decode(t, w, &view)
	if view.Orgs != 2 {
		t.Errorf("orgs = %d, want 2 (the true, uncapped total)", view.Orgs)
	}
	if view.RunningSandboxes != 1 {
		t.Errorf("running_sandboxes = %d, want 1 (only ops's org, org2 skipped)", view.RunningSandboxes)
	}
	if view.FailedOrgs != 1 {
		t.Errorf("failed_orgs = %d, want 1", view.FailedOrgs)
	}
}

// TestAdminOverviewNoFailuresOmitsFailedOrgs asserts the all-succeed case
// (every existing overview test's shape) reports a zero FailedOrgs, matching
// the omitempty wire convention: no orgs failed, so the field carries no
// signal.
func TestAdminOverviewNoFailuresOmitsFailedOrgs(t *testing.T) {
	f := newAdminFixture(t, Deps{
		Capabilities:        Capabilities{Edition: "hosted"},
		InstanceAdminEmails: []string{"ops@example.com"},
	})
	ops, org, err := f.accounts.SignUp(context.Background(), "ops@example.com")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	w := f.req(t, "GET", "/console/admin/overview", ops.ID, org.ID)
	var view AdminOverview
	decode(t, w, &view)
	if view.FailedOrgs != 0 {
		t.Errorf("failed_orgs = %d, want 0", view.FailedOrgs)
	}
	if strings.Contains(w.Body.String(), "failed_orgs") {
		t.Errorf("expected failed_orgs to be omitted (omitempty) from the response body, got %s", w.Body.String())
	}
}

// TestAdminOrgsPerOrgErrorIsNotFatal asserts that when ListOrgMembers fails
// for exactly one org, GET /console/admin/orgs still returns 200 with the
// other orgs' rows intact, reports the true uncapped total, and reports the
// one failure in failed_orgs.
func TestAdminOrgsPerOrgErrorIsNotFatal(t *testing.T) {
	f := newAdminFixture(t, Deps{
		Capabilities:        Capabilities{Edition: "hosted"},
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
	f.con.deps.Orgs = &failingOrgDirectory{
		OrgDirectory: f.store,
		failOrgID:    org2.ID,
		err:          errors.New("simulated membership store outage"),
	}

	w := f.req(t, "GET", "/console/admin/orgs", ops.ID, opsOrg.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Orgs       []AdminOrgView `json:"orgs"`
		Total      int            `json:"total"`
		FailedOrgs int            `json:"failed_orgs"`
	}
	decode(t, w, &body)
	if body.Total != 2 {
		t.Errorf("total = %d, want 2 (the true, uncapped total)", body.Total)
	}
	if len(body.Orgs) != 1 || body.Orgs[0].ID != opsOrg.ID {
		t.Fatalf("orgs = %+v, want exactly ops's org row", body.Orgs)
	}
	if body.FailedOrgs != 1 {
		t.Errorf("failed_orgs = %d, want 1", body.FailedOrgs)
	}
}

// TestAdminOrgsNoFailuresOmitsFailedOrgs asserts the all-succeed case never
// regresses: no failed_orgs key at all when every org's read succeeds.
func TestAdminOrgsNoFailuresOmitsFailedOrgs(t *testing.T) {
	f := newAdminFixture(t, Deps{
		Capabilities:        Capabilities{Edition: "hosted"},
		InstanceAdminEmails: []string{"ops@example.com"},
	})
	ops, org, err := f.accounts.SignUp(context.Background(), "ops@example.com")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	w := f.req(t, "GET", "/console/admin/orgs", ops.ID, org.ID)
	if strings.Contains(w.Body.String(), "failed_orgs") {
		t.Errorf("expected failed_orgs to be omitted from the response body when no org failed, got %s", w.Body.String())
	}
}

// --- Waitlist ---

// fakeWaitlistSource is an in-memory WaitlistSource test double that records
// Approve calls.
type fakeWaitlistSource struct {
	entries    []WaitlistEntry
	approved   []string
	approveErr error
}

func (f *fakeWaitlistSource) List(context.Context) ([]WaitlistEntry, error) {
	return f.entries, nil
}

func (f *fakeWaitlistSource) Approve(_ context.Context, email string) error {
	if f.approveErr != nil {
		return f.approveErr
	}
	f.approved = append(f.approved, email)
	return nil
}

// TestAdminWaitlistDefaultNotConfigured asserts the safe-to-instantiate
// default (no WaitlistSource wired) lists as empty and refuses approval with
// an honest 501, never a fabricated success.
func TestAdminWaitlistDefaultNotConfigured(t *testing.T) {
	f := newAdminFixture(t, Deps{
		Capabilities:        Capabilities{Edition: "hosted"},
		InstanceAdminEmails: []string{"ops@example.com"},
	})
	ops, org, err := f.accounts.SignUp(context.Background(), "ops@example.com")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	w := f.req(t, "GET", "/console/admin/waitlist", ops.ID, org.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d", w.Code)
	}
	var body struct {
		Entries []AdminWaitlistEntryView `json:"entries"`
	}
	decode(t, w, &body)
	if len(body.Entries) != 0 {
		t.Fatalf("expected no entries by default, got %+v", body.Entries)
	}

	w = f.req(t, "POST", "/console/admin/waitlist/"+encodeWaitlistID("someone@example.com")+"/approve", ops.ID, org.ID)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("approve status = %d, want 501; body=%s", w.Code, w.Body.String())
	}
}

// TestAdminWaitlistListAndApprove asserts the list round-trips id->email and
// that POST .../{id}/approve decodes the id and calls WaitlistSource.Approve
// with the exact original email.
func TestAdminWaitlistListAndApprove(t *testing.T) {
	fw := &fakeWaitlistSource{entries: []WaitlistEntry{
		{Email: "waiting@example.com", CreatedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)},
	}}
	f := newAdminFixture(t, Deps{
		Capabilities:        Capabilities{Edition: "hosted"},
		InstanceAdminEmails: []string{"ops@example.com"},
		Waitlist:            fw,
	})
	ops, org, err := f.accounts.SignUp(context.Background(), "ops@example.com")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}

	w := f.req(t, "GET", "/console/admin/waitlist", ops.ID, org.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d", w.Code)
	}
	var body struct {
		Entries []AdminWaitlistEntryView `json:"entries"`
	}
	decode(t, w, &body)
	if len(body.Entries) != 1 || body.Entries[0].Email != "waiting@example.com" {
		t.Fatalf("entries = %+v", body.Entries)
	}
	id := body.Entries[0].ID

	w = f.req(t, "POST", "/console/admin/waitlist/"+id+"/approve", ops.ID, org.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("approve status = %d, body=%s", w.Code, w.Body.String())
	}
	if len(fw.approved) != 1 || fw.approved[0] != "waiting@example.com" {
		t.Fatalf("approved = %v, want [waiting@example.com]", fw.approved)
	}
}

// TestAdminWaitlistApproveInvalidID asserts a garbage path segment is
// rejected with 404 rather than reaching the seam.
func TestAdminWaitlistApproveInvalidID(t *testing.T) {
	fw := &fakeWaitlistSource{}
	f := newAdminFixture(t, Deps{
		Capabilities:        Capabilities{Edition: "hosted"},
		InstanceAdminEmails: []string{"ops@example.com"},
		Waitlist:            fw,
	})
	ops, org, err := f.accounts.SignUp(context.Background(), "ops@example.com")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	w := f.req(t, "POST", "/console/admin/waitlist/not-valid-base64!!/approve", ops.ID, org.ID)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if len(fw.approved) != 0 {
		t.Fatal("Approve must not be called for an invalid id")
	}
}

// TestAdminWaitlistForbiddenForNonAdmin asserts both waitlist endpoints are
// gated like every other /console/admin/... endpoint.
func TestAdminWaitlistForbiddenForNonAdmin(t *testing.T) {
	f := newAdminFixture(t, Deps{Capabilities: Capabilities{Edition: "hosted"}})
	regular, org, err := f.accounts.SignUp(context.Background(), "regular@example.com")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	if w := f.req(t, "GET", "/console/admin/waitlist", regular.ID, org.ID); w.Code != http.StatusForbidden {
		t.Fatalf("list status = %d, want 403", w.Code)
	}
	if w := f.req(t, "POST", "/console/admin/waitlist/"+encodeWaitlistID("x@example.com")+"/approve", regular.ID, org.ID); w.Code != http.StatusForbidden {
		t.Fatalf("approve status = %d, want 403", w.Code)
	}
}

// --- GET /console/admin/nodes ---

// fakeNodeSource is a test NodeSource double.
type fakeNodeSource struct {
	nodes []NodeView
	err   error
}

func (f *fakeNodeSource) Nodes(context.Context) ([]NodeView, error) { return f.nodes, f.err }

// TestAdminNodesUnconfiguredReportsUnavailable asserts a Console with no
// NodeSource wired reports {"available": false, "nodes": []}, never a
// fabricated node list.
func TestAdminNodesUnconfiguredReportsUnavailable(t *testing.T) {
	f := newAdminFixture(t, Deps{
		Capabilities:        Capabilities{Edition: "hosted"},
		InstanceAdminEmails: []string{"ops@example.com"},
	})
	ops, org, err := f.accounts.SignUp(context.Background(), "ops@example.com")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	w := f.req(t, "GET", "/console/admin/nodes", ops.ID, org.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var body struct {
		Available bool       `json:"available"`
		Nodes     []NodeView `json:"nodes"`
	}
	decode(t, w, &body)
	if body.Available {
		t.Error("expected available=false with no NodeSource configured")
	}
	if len(body.Nodes) != 0 {
		t.Errorf("expected no nodes, got %+v", body.Nodes)
	}
}

// TestAdminNodesConfiguredReportsNodes asserts a configured NodeSource is
// reported available with its node list passed through.
func TestAdminNodesConfiguredReportsNodes(t *testing.T) {
	fn := &fakeNodeSource{nodes: []NodeView{
		{Name: "node-1", Ready: true, KVM: true, Dedicated: true, AllocatableCPU: "16", AllocatableMem: "62Gi"},
	}}
	f := newAdminFixture(t, Deps{
		Capabilities:        Capabilities{Edition: "hosted"},
		InstanceAdminEmails: []string{"ops@example.com"},
		Nodes:               fn,
	})
	ops, org, err := f.accounts.SignUp(context.Background(), "ops@example.com")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	w := f.req(t, "GET", "/console/admin/nodes", ops.ID, org.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var body struct {
		Available bool       `json:"available"`
		Nodes     []NodeView `json:"nodes"`
	}
	decode(t, w, &body)
	if !body.Available {
		t.Error("expected available=true with a configured NodeSource")
	}
	if len(body.Nodes) != 1 || body.Nodes[0].Name != "node-1" {
		t.Fatalf("nodes = %+v", body.Nodes)
	}
}

// TestAdminNodesSourceErrorReportsUnavailable asserts a NodeSource error
// degrades to available=false rather than a 500, since a cluster hiccup
// should not break the whole admin nodes page.
func TestAdminNodesSourceErrorReportsUnavailable(t *testing.T) {
	fn := &fakeNodeSource{err: context.DeadlineExceeded}
	f := newAdminFixture(t, Deps{
		Capabilities:        Capabilities{Edition: "hosted"},
		InstanceAdminEmails: []string{"ops@example.com"},
		Nodes:               fn,
	})
	ops, org, err := f.accounts.SignUp(context.Background(), "ops@example.com")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	w := f.req(t, "GET", "/console/admin/nodes", ops.ID, org.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var body struct {
		Available bool `json:"available"`
	}
	decode(t, w, &body)
	if body.Available {
		t.Error("expected available=false when the NodeSource errors")
	}
}

// TestAdminNodesForbiddenForNonAdmin asserts the same 403 gating as every
// other /console/admin/... endpoint.
func TestAdminNodesForbiddenForNonAdmin(t *testing.T) {
	f := newAdminFixture(t, Deps{Capabilities: Capabilities{Edition: "hosted"}})
	regular, org, err := f.accounts.SignUp(context.Background(), "regular@example.com")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	w := f.req(t, "GET", "/console/admin/nodes", regular.ID, org.ID)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}
