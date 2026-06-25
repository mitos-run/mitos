package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mitos.run/mitos/internal/saas"
)

// enforceFixture builds a console wired with a real AccountService so that
// c.authorize can resolve memberships. It exposes helpers to seed caller
// contexts for specific roles.
type enforceFixture struct {
	con        *Console
	accounts   *saas.AccountService
	store      *saas.MemStore
	ownerAcct  string
	ownerOrg   string
	adminAcct  string
	memberAcct string
	viewerAcct string
	sandboxes  *MemSandboxControl
}

func newEnforceFixture(t *testing.T) *enforceFixture {
	t.Helper()
	store := saas.NewMemStore()
	keys := saas.NewKeyService(store)
	accounts := saas.NewAccountService(store, keys)
	ctx := context.Background()

	// owner signs up: gets RoleOwner in their personal org.
	owner, ownerOrg, err := accounts.SignUp(ctx, "enforce-owner@example.com")
	if err != nil {
		t.Fatalf("SignUp owner: %v", err)
	}

	// admin: sign up (gets personal org), then add as admin to owner's org.
	admin, _, err := accounts.SignUp(ctx, "enforce-admin@example.com")
	if err != nil {
		t.Fatalf("SignUp admin: %v", err)
	}
	if err := store.PutMembership(ctx, saas.Membership{
		AccountID: admin.ID,
		OrgID:     ownerOrg.ID,
		Role:      saas.RoleAdmin,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed admin membership: %v", err)
	}

	// member: RoleMember - has PermUseResources and PermReadOnly only.
	member, _, err := accounts.SignUp(ctx, "enforce-member@example.com")
	if err != nil {
		t.Fatalf("SignUp member: %v", err)
	}
	if err := store.PutMembership(ctx, saas.Membership{
		AccountID: member.ID,
		OrgID:     ownerOrg.ID,
		Role:      saas.RoleMember,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed member membership: %v", err)
	}

	// viewer: RoleViewer - PermReadOnly only.
	viewer, _, err := accounts.SignUp(ctx, "enforce-viewer@example.com")
	if err != nil {
		t.Fatalf("SignUp viewer: %v", err)
	}
	if err := store.PutMembership(ctx, saas.Membership{
		AccountID: viewer.ID,
		OrgID:     ownerOrg.ID,
		Role:      saas.RoleViewer,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed viewer membership: %v", err)
	}

	sandboxes := NewMemSandboxControl()
	sandboxes.Add(SandboxView{ID: "sb-1", OrgID: ownerOrg.ID, Phase: "Running"})

	con := New(Deps{
		Accounts:  accounts,
		Sandboxes: sandboxes,
		Audit:     NewMemAuditLog(),
		Now:       time.Now,
	})

	return &enforceFixture{
		con:        con,
		accounts:   accounts,
		store:      store,
		ownerAcct:  owner.ID,
		ownerOrg:   ownerOrg.ID,
		adminAcct:  admin.ID,
		memberAcct: member.ID,
		viewerAcct: viewer.ID,
		sandboxes:  sandboxes,
	}
}

func (ef *enforceFixture) do(t *testing.T, method, target, body, acct, org string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	r = r.WithContext(WithCaller(r.Context(), acct, org))
	w := httptest.NewRecorder()
	ef.con.ServeHTTP(w, r)
	return w
}

// TestEnforceMemberCannotReachBillingPortal verifies that the billing-portal
// link is gated on billing.manage: a member (no billing.manage) gets 403 before
// any portal lookup, so the manage-subscription surface is not reachable.
func TestEnforceMemberCannotReachBillingPortal(t *testing.T) {
	ef := newEnforceFixture(t)
	w := ef.do(t, "GET", "/console/billing/portal", "", ef.memberAcct, ef.ownerOrg)
	if w.Code != http.StatusForbidden {
		t.Errorf("member GET /console/billing/portal = %d, want 403; body=%s", w.Code, w.Body.String())
	}
}

// TestEnforceMemberCannotManageSecrets verifies that a caller with RoleMember
// (which has PermUseResources and PermReadOnly, but NOT PermManageSecrets)
// receives 403 on POST /console/secrets.
func TestEnforceMemberCannotManageSecrets(t *testing.T) {
	ef := newEnforceFixture(t)
	w := ef.do(t, "POST", "/console/secrets",
		`{"name":"K","value":"v"}`,
		ef.memberAcct, ef.ownerOrg)
	if w.Code != http.StatusForbidden {
		t.Errorf("member POST /console/secrets = %d, want 403; body=%s", w.Code, w.Body.String())
	}
}

// TestEnforceMemberCannotManageProjects verifies that a caller with RoleMember
// receives 403 on POST /console/projects.
func TestEnforceMemberCannotManageProjects(t *testing.T) {
	ef := newEnforceFixture(t)
	w := ef.do(t, "POST", "/console/projects",
		`{"name":"proj"}`,
		ef.memberAcct, ef.ownerOrg)
	if w.Code != http.StatusForbidden {
		t.Errorf("member POST /console/projects = %d, want 403; body=%s", w.Code, w.Body.String())
	}
}

// TestEnforceMemberCannotSetDataRetention verifies that a caller with RoleMember
// receives 403 on PUT /console/retention.
func TestEnforceMemberCannotSetDataRetention(t *testing.T) {
	ef := newEnforceFixture(t)
	w := ef.do(t, "PUT", "/console/retention",
		`{"sandbox_metadata_days":30}`,
		ef.memberAcct, ef.ownerOrg)
	if w.Code != http.StatusForbidden {
		t.Errorf("member PUT /console/retention = %d, want 403; body=%s", w.Code, w.Body.String())
	}
}

// TestEnforceMemberCanCreateKey verifies that RoleMember (which HAS PermUseResources)
// is NOT rejected on POST /console/keys. The handler may return 200 or 201 but must
// not return 403.
func TestEnforceMemberCanCreateKey(t *testing.T) {
	ef := newEnforceFixture(t)
	w := ef.do(t, "POST", "/console/keys",
		`{"name":"mkey","scopes":["sandboxes"]}`,
		ef.memberAcct, ef.ownerOrg)
	if w.Code == http.StatusForbidden {
		t.Errorf("member POST /console/keys = 403, want NOT forbidden (member has resources.use); body=%s", w.Body.String())
	}
}

// TestEnforceViewerCannotTerminateSandbox verifies that a caller with RoleViewer
// (PermReadOnly only, no PermUseResources) receives 403 on
// DELETE /console/sandboxes/{id}.
func TestEnforceViewerCannotTerminateSandbox(t *testing.T) {
	ef := newEnforceFixture(t)
	w := ef.do(t, "DELETE", "/console/sandboxes/sb-1", "", ef.viewerAcct, ef.ownerOrg)
	if w.Code != http.StatusForbidden {
		t.Errorf("viewer DELETE /console/sandboxes/sb-1 = %d, want 403; body=%s", w.Code, w.Body.String())
	}
}

// TestEnforceAdminCanManageSecrets verifies that a caller with RoleAdmin
// (which has PermManageSecrets) is NOT rejected on POST /console/secrets.
func TestEnforceAdminCanManageSecrets(t *testing.T) {
	ef := newEnforceFixture(t)
	w := ef.do(t, "POST", "/console/secrets",
		`{"name":"K","value":"v"}`,
		ef.adminAcct, ef.ownerOrg)
	if w.Code == http.StatusForbidden {
		t.Errorf("admin POST /console/secrets = 403, want NOT forbidden; body=%s", w.Body.String())
	}
}

// TestEnforceAdminCanManageProjects verifies that a caller with RoleAdmin
// (which has PermManageProjects) is NOT rejected on POST /console/projects.
func TestEnforceAdminCanManageProjects(t *testing.T) {
	ef := newEnforceFixture(t)
	w := ef.do(t, "POST", "/console/projects",
		`{"name":"adminproj"}`,
		ef.adminAcct, ef.ownerOrg)
	if w.Code == http.StatusForbidden {
		t.Errorf("admin POST /console/projects = 403, want NOT forbidden; body=%s", w.Body.String())
	}
}

// TestEnforceOrgIsolationUnaffected verifies that the existing
// TestEveryEndpointRefusesMissingOrgContext behavior is unaffected by the new
// permission gates: a request with no org context must still return 401.
func TestEnforceOrgIsolationUnaffected(t *testing.T) {
	con := New(Deps{})
	endpoints := []struct{ method, target string }{
		{"POST", "/console/secrets"},
		{"POST", "/console/projects"},
		{"PUT", "/console/retention"},
		{"DELETE", "/console/sandboxes/x"},
		{"POST", "/console/keys"},
		{"POST", "/console/keys/x/revoke"},
		{"PUT", "/console/audit/retention"},
		{"POST", "/console/audit/sinks"},
		{"DELETE", "/console/audit/sinks/x"},
	}
	for _, ep := range endpoints {
		r := httptest.NewRequest(ep.method, ep.target, nil)
		w := httptest.NewRecorder()
		con.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without org context = %d, want 401", ep.method, ep.target, w.Code)
		}
	}
}
