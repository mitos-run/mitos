package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"mitos.run/mitos/internal/saas"
)

// projectAccessFixture sets up the full matrix described in Task 1:
//
//   - Org O with five accounts: OWNER, ADMIN, MEMBER, VIEWER, PVIEWER.
//   - Project P inside org O.
//   - Two sandboxes: SU (unassigned) and SP (assigned to project P).
//   - PVIEWER is a Viewer at the org level but an Admin of project P.
//
// A second org O2 (with its own owner) is wired for cross-org isolation tests.
type projectAccessFixture struct {
	con *Console

	store  *saas.MemStore
	sboxes *MemSandboxControl

	orgID  string
	org2ID string

	ownerAcct   string
	adminAcct   string
	memberAcct  string
	viewerAcct  string
	pviewerAcct string

	projectP  string
	sandboxSU string
	sandboxSP string

	org2Acct string
}

func newProjectAccessFixture(t *testing.T) *projectAccessFixture {
	t.Helper()
	ctx := context.Background()

	store := saas.NewMemStore()
	keys := saas.NewKeyService(store)
	accounts := saas.NewAccountService(store, keys)

	// OWNER: RoleOwner in org O (via SignUp).
	owner, ownerOrg, err := accounts.SignUp(ctx, "pa-owner@example.com")
	if err != nil {
		t.Fatalf("SignUp owner: %v", err)
	}

	// ADMIN: RoleAdmin in org O.
	admin, _, err := accounts.SignUp(ctx, "pa-admin@example.com")
	if err != nil {
		t.Fatalf("SignUp admin: %v", err)
	}
	if err := store.PutMembership(ctx, saas.Membership{
		AccountID: admin.ID, OrgID: ownerOrg.ID, Role: saas.RoleAdmin, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	// MEMBER: RoleMember in org O (PermUseResources + PermReadOnly).
	member, _, err := accounts.SignUp(ctx, "pa-member@example.com")
	if err != nil {
		t.Fatalf("SignUp member: %v", err)
	}
	if err := store.PutMembership(ctx, saas.Membership{
		AccountID: member.ID, OrgID: ownerOrg.ID, Role: saas.RoleMember, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	// VIEWER: RoleViewer in org O (PermReadOnly only).
	viewer, _, err := accounts.SignUp(ctx, "pa-viewer@example.com")
	if err != nil {
		t.Fatalf("SignUp viewer: %v", err)
	}
	if err := store.PutMembership(ctx, saas.Membership{
		AccountID: viewer.ID, OrgID: ownerOrg.ID, Role: saas.RoleViewer, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed viewer: %v", err)
	}

	// PVIEWER: RoleViewer in org O (org-wide), PLUS project Admin of project P.
	pviewer, _, err := accounts.SignUp(ctx, "pa-pviewer@example.com")
	if err != nil {
		t.Fatalf("SignUp pviewer: %v", err)
	}
	if err := store.PutMembership(ctx, saas.Membership{
		AccountID: pviewer.ID, OrgID: ownerOrg.ID, Role: saas.RoleViewer, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed pviewer org membership: %v", err)
	}

	// Org O2 owner for cross-org isolation tests.
	org2Owner, org2, err := accounts.SignUp(ctx, "pa-org2@example.com")
	if err != nil {
		t.Fatalf("SignUp org2 owner: %v", err)
	}

	// Project P in org O.
	projects := NewMemProjectStore()
	projectP, err := projects.Create(ctx, ownerOrg.ID, "project-P", "")
	if err != nil {
		t.Fatalf("Create project P: %v", err)
	}

	// Seed PVIEWER as Admin of project P.
	pm := NewMemProjectMembershipStore()
	if err := pm.Assign(ctx, ownerOrg.ID, projectP.ID, pviewer.ID, saas.RoleAdmin); err != nil {
		t.Fatalf("Assign pviewer to project P: %v", err)
	}

	// Sandboxes: SU (unassigned) and SP (assigned to P).
	sboxes := NewMemSandboxControl()
	sboxes.Add(SandboxView{ID: "sb-su", OrgID: ownerOrg.ID, Phase: "Running"})
	sboxes.Add(SandboxView{ID: "sb-sp", OrgID: ownerOrg.ID, Phase: "Running"})
	sboxes.Add(SandboxView{ID: "sb-org2", OrgID: org2.ID, Phase: "Running"})

	// Tag SP -> project P.
	rp := NewMemResourceProjectStore()
	if err := rp.SetProject(ctx, ownerOrg.ID, "sandbox", "sb-sp", projectP.ID); err != nil {
		t.Fatalf("SetProject sb-sp: %v", err)
	}

	con := New(Deps{
		Accounts:         accounts,
		Sandboxes:        sboxes,
		Projects:         projects,
		ProjectMembers:   pm,
		ResourceProjects: rp,
		Audit:            NewMemAuditLog(),
		Now:              time.Now,
	})

	return &projectAccessFixture{
		con:         con,
		store:       store,
		sboxes:      sboxes,
		orgID:       ownerOrg.ID,
		org2ID:      org2.ID,
		ownerAcct:   owner.ID,
		adminAcct:   admin.ID,
		memberAcct:  member.ID,
		viewerAcct:  viewer.ID,
		pviewerAcct: pviewer.ID,
		projectP:    projectP.ID,
		sandboxSU:   "sb-su",
		sandboxSP:   "sb-sp",
		org2Acct:    org2Owner.ID,
	}
}

func (f *projectAccessFixture) do(t *testing.T, method, target, acct, org string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	r = r.WithContext(WithCaller(r.Context(), acct, org))
	w := httptest.NewRecorder()
	f.con.ServeHTTP(w, r)
	return w
}

// listSandboxIDs decodes a GET /console/sandboxes response and returns the set of IDs.
func listSandboxIDs(t *testing.T, w *httptest.ResponseRecorder) map[string]bool {
	t.Helper()
	var resp struct {
		Sandboxes []SandboxView `json:"sandboxes"`
	}
	decode(t, w, &resp)
	ids := make(map[string]bool, len(resp.Sandboxes))
	for _, s := range resp.Sandboxes {
		ids[s.ID] = true
	}
	return ids
}

// --- List sandbox filtering ---

// TestProjectAccessAdminListSeesAll verifies that an org-wide Admin (who has
// PermManageProjects) sees both SU (unassigned) and SP (assigned to project P).
func TestProjectAccessAdminListSeesAll(t *testing.T) {
	f := newProjectAccessFixture(t)
	w := f.do(t, "GET", "/console/sandboxes", f.adminAcct, f.orgID)
	if w.Code != http.StatusOK {
		t.Fatalf("ADMIN list = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	ids := listSandboxIDs(t, w)
	if !ids[f.sandboxSU] {
		t.Errorf("ADMIN list must include SU; got %v", ids)
	}
	if !ids[f.sandboxSP] {
		t.Errorf("ADMIN list must include SP; got %v", ids)
	}
}

// TestProjectAccessMemberListSeesSUNotSP verifies that an org-wide Member (not
// in project P) sees SU but NOT SP.
func TestProjectAccessMemberListSeesSUNotSP(t *testing.T) {
	f := newProjectAccessFixture(t)
	w := f.do(t, "GET", "/console/sandboxes", f.memberAcct, f.orgID)
	if w.Code != http.StatusOK {
		t.Fatalf("MEMBER list = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	ids := listSandboxIDs(t, w)
	if !ids[f.sandboxSU] {
		t.Errorf("MEMBER list must include SU (org-wide use); got %v", ids)
	}
	if ids[f.sandboxSP] {
		t.Errorf("MEMBER list must NOT include SP (not in project P); got %v", ids)
	}
}

// TestProjectAccessPViewerListSeesSpAndSu verifies that PVIEWER (org-wide
// Viewer + project Admin of P) sees SP (project access) and SU (org-wide read
// on unassigned sandboxes).
func TestProjectAccessPViewerListSeesSpAndSu(t *testing.T) {
	f := newProjectAccessFixture(t)
	w := f.do(t, "GET", "/console/sandboxes", f.pviewerAcct, f.orgID)
	if w.Code != http.StatusOK {
		t.Fatalf("PVIEWER list = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	ids := listSandboxIDs(t, w)
	if !ids[f.sandboxSP] {
		t.Errorf("PVIEWER list must include SP (project Admin of P); got %v", ids)
	}
	if !ids[f.sandboxSU] {
		t.Errorf("PVIEWER list must include SU (org-wide Viewer reads unassigned); got %v", ids)
	}
}

// --- Inspect (GET /console/sandboxes/{id}) ---

// TestProjectAccessMemberInspectSPReturns404 verifies that MEMBER gets 404 on
// inspecting SP (not 403; must not leak existence).
func TestProjectAccessMemberInspectSPReturns404(t *testing.T) {
	f := newProjectAccessFixture(t)
	w := f.do(t, "GET", "/console/sandboxes/"+f.sandboxSP, f.memberAcct, f.orgID)
	if w.Code != http.StatusNotFound {
		t.Errorf("MEMBER inspect SP = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

// TestProjectAccessAdminInspectSPReturns200 verifies that ADMIN can inspect SP.
func TestProjectAccessAdminInspectSPReturns200(t *testing.T) {
	f := newProjectAccessFixture(t)
	w := f.do(t, "GET", "/console/sandboxes/"+f.sandboxSP, f.adminAcct, f.orgID)
	if w.Code != http.StatusOK {
		t.Errorf("ADMIN inspect SP = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestProjectAccessPViewerInspectSPReturns200 verifies that PVIEWER (project
// Admin of P) can inspect SP.
func TestProjectAccessPViewerInspectSPReturns200(t *testing.T) {
	f := newProjectAccessFixture(t)
	w := f.do(t, "GET", "/console/sandboxes/"+f.sandboxSP, f.pviewerAcct, f.orgID)
	if w.Code != http.StatusOK {
		t.Errorf("PVIEWER inspect SP = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// --- Terminate (DELETE /console/sandboxes/{id}) ---

// TestProjectAccessMemberTerminateSPReturns403 verifies that MEMBER (not in
// project P) gets 403 when terminating SP, and SP is not terminated.
func TestProjectAccessMemberTerminateSPReturns403(t *testing.T) {
	f := newProjectAccessFixture(t)
	w := f.do(t, "DELETE", "/console/sandboxes/"+f.sandboxSP, f.memberAcct, f.orgID)
	if w.Code != http.StatusForbidden {
		t.Errorf("MEMBER terminate SP = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	if _, err := f.sboxes.Get(context.Background(), f.orgID, f.sandboxSP); err != nil {
		t.Errorf("SP must still exist after forbidden terminate; err=%v", err)
	}
}

// TestProjectAccessPViewerTerminateSPReturns200 verifies that PVIEWER (project
// Admin of P, granting PermUseResources within P) can terminate SP.
func TestProjectAccessPViewerTerminateSPReturns200(t *testing.T) {
	f := newProjectAccessFixture(t)
	w := f.do(t, "DELETE", "/console/sandboxes/"+f.sandboxSP, f.pviewerAcct, f.orgID)
	if w.Code != http.StatusOK {
		t.Errorf("PVIEWER terminate SP = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestProjectAccessAdminTerminateSPReturns200 verifies that ADMIN (org-wide
// PermManageProjects -> full access) can terminate SP.
func TestProjectAccessAdminTerminateSPReturns200(t *testing.T) {
	f := newProjectAccessFixture(t)
	w := f.do(t, "DELETE", "/console/sandboxes/"+f.sandboxSP, f.adminAcct, f.orgID)
	if w.Code != http.StatusOK {
		t.Errorf("ADMIN terminate SP = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestProjectAccessMemberTerminateSUReturns200 verifies that MEMBER (org-wide
// PermUseResources) can terminate the unassigned sandbox SU.
func TestProjectAccessMemberTerminateSUReturns200(t *testing.T) {
	f := newProjectAccessFixture(t)
	w := f.do(t, "DELETE", "/console/sandboxes/"+f.sandboxSU, f.memberAcct, f.orgID)
	if w.Code != http.StatusOK {
		t.Errorf("MEMBER terminate SU = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// --- Org isolation ---

// TestProjectAccessOrg2CannotInspectOrg1SP verifies that the org2 owner
// (authenticated for org2) cannot inspect org O's SP sandbox (404).
func TestProjectAccessOrg2CannotInspectOrg1SP(t *testing.T) {
	f := newProjectAccessFixture(t)
	w := f.do(t, "GET", "/console/sandboxes/"+f.sandboxSP, f.org2Acct, f.org2ID)
	if w.Code != http.StatusNotFound {
		t.Errorf("org2 inspect org1 SP = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

// TestProjectAccessOrg2CannotTerminateOrg1SP verifies that the org2 owner
// cannot terminate org O's SP (the sandbox is not found in org2, so 404).
func TestProjectAccessOrg2CannotTerminateOrg1SP(t *testing.T) {
	f := newProjectAccessFixture(t)
	w := f.do(t, "DELETE", "/console/sandboxes/"+f.sandboxSP, f.org2Acct, f.org2ID)
	if w.Code != http.StatusNotFound {
		t.Errorf("org2 terminate org1 SP = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	if _, err := f.sboxes.Get(context.Background(), f.orgID, f.sandboxSP); err != nil {
		t.Errorf("org1 SP must still exist after cross-org terminate attempt; err=%v", err)
	}
}

// TestProjectAccessOrg2ListSeesOnlyOwnSandboxes verifies that the org2 owner's
// list does not contain org O's sandboxes.
func TestProjectAccessOrg2ListSeesOnlyOwnSandboxes(t *testing.T) {
	f := newProjectAccessFixture(t)
	w := f.do(t, "GET", "/console/sandboxes", f.org2Acct, f.org2ID)
	if w.Code != http.StatusOK {
		t.Fatalf("org2 list = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Sandboxes []SandboxView `json:"sandboxes"`
	}
	decode(t, w, &resp)
	for _, s := range resp.Sandboxes {
		if s.OrgID == f.orgID {
			t.Errorf("org2 list leaked org1 sandbox %s", s.ID)
		}
	}
}
