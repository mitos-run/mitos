package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mitos.run/mitos/internal/saas"
)

// resourceProjectsFixture wires a console with two orgs (alice and bob), a
// seeded project and sandbox in alice's org, and an admin + member caller in
// alice's org so the permission tests have the full matrix.
type resourceProjectsFixture struct {
	con        *Console
	store      *saas.MemStore
	projects   *MemProjectStore
	sandboxes  *MemSandboxControl
	resProj    *MemResourceProjectStore
	aliceAcct  string
	aliceOrg   string
	bobAcct    string
	bobOrg     string
	adminAcct  string // RoleAdmin in alice's org: has projects.manage
	memberAcct string // RoleMember in alice's org: no projects.manage
	projectID  string // seeded project in alice's org
	sandboxID  string // seeded sandbox in alice's org
}

func newResourceProjectsFixture(t *testing.T) *resourceProjectsFixture {
	t.Helper()
	store := saas.NewMemStore()
	keys := saas.NewKeyService(store)
	accounts := saas.NewAccountService(store, keys)
	ctx := context.Background()

	alice, aliceOrg, err := accounts.SignUp(ctx, "rp-alice@example.com")
	if err != nil {
		t.Fatalf("SignUp alice: %v", err)
	}
	bob, bobOrg, err := accounts.SignUp(ctx, "rp-bob@example.com")
	if err != nil {
		t.Fatalf("SignUp bob: %v", err)
	}

	// admin has RoleAdmin in alice's org (has projects.manage).
	admin, _, err := accounts.SignUp(ctx, "rp-admin@example.com")
	if err != nil {
		t.Fatalf("SignUp admin: %v", err)
	}
	if err := store.PutMembership(ctx, saas.Membership{
		AccountID: admin.ID,
		OrgID:     aliceOrg.ID,
		Role:      saas.RoleAdmin,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed admin membership: %v", err)
	}

	// member has RoleMember in alice's org (no projects.manage).
	member, _, err := accounts.SignUp(ctx, "rp-member@example.com")
	if err != nil {
		t.Fatalf("SignUp member: %v", err)
	}
	if err := store.PutMembership(ctx, saas.Membership{
		AccountID: member.ID,
		OrgID:     aliceOrg.ID,
		Role:      saas.RoleMember,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed member membership: %v", err)
	}

	projects := NewMemProjectStore()
	p, err := projects.Create(ctx, aliceOrg.ID, "ProjectA", "desc")
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}

	sandboxes := NewMemSandboxControl()
	sandboxID := "rp-sb-1"
	sandboxes.Add(SandboxView{
		ID:    sandboxID,
		OrgID: aliceOrg.ID,
		Phase: "Running",
	})

	resProj := NewMemResourceProjectStore()
	con := New(Deps{
		Accounts:         accounts,
		Projects:         projects,
		Sandboxes:        sandboxes,
		ResourceProjects: resProj,
		Now:              time.Now,
	})
	return &resourceProjectsFixture{
		con:        con,
		store:      store,
		projects:   projects,
		sandboxes:  sandboxes,
		resProj:    resProj,
		aliceAcct:  alice.ID,
		aliceOrg:   aliceOrg.ID,
		bobAcct:    bob.ID,
		bobOrg:     bobOrg.ID,
		adminAcct:  admin.ID,
		memberAcct: member.ID,
		projectID:  p.ID,
		sandboxID:  sandboxID,
	}
}

func (f *resourceProjectsFixture) req(t *testing.T, method, target, body, acct, org string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	r = r.WithContext(WithCaller(r.Context(), acct, org))
	w := httptest.NewRecorder()
	f.con.ServeHTTP(w, r)
	return w
}

// TestResourceProjectAdminSetsAndListReflects verifies that an admin PUT
// assigns a project to a sandbox and a subsequent GET /console/sandboxes
// returns that sandbox with project_id set.
func TestResourceProjectAdminSetsAndListReflects(t *testing.T) {
	f := newResourceProjectsFixture(t)

	// Admin PUTs project assignment.
	body := `{"project_id":"` + f.projectID + `"}`
	pw := f.req(t, "PUT", "/console/sandboxes/"+f.sandboxID+"/project", body, f.adminAcct, f.aliceOrg)
	if pw.Code != http.StatusOK {
		t.Fatalf("PUT /console/sandboxes/{id}/project status = %d, want 200; body=%s", pw.Code, pw.Body.String())
	}

	// GET list should reflect the project_id.
	gw := f.req(t, "GET", "/console/sandboxes", "", f.aliceAcct, f.aliceOrg)
	if gw.Code != http.StatusOK {
		t.Fatalf("GET /console/sandboxes status = %d, want 200; body=%s", gw.Code, gw.Body.String())
	}
	var resp struct {
		Sandboxes []SandboxView `json:"sandboxes"`
	}
	if err := json.Unmarshal(gw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, gw.Body.String())
	}
	if len(resp.Sandboxes) != 1 {
		t.Fatalf("sandboxes = %+v, want 1", resp.Sandboxes)
	}
	if resp.Sandboxes[0].ProjectID != f.projectID {
		t.Errorf("sandbox project_id = %q, want %q", resp.Sandboxes[0].ProjectID, f.projectID)
	}
}

// TestResourceProjectCrossOrgIsolation locks the per-org keying invariant
// directly at the store: after alice's org tags a sandbox, bob's org never sees
// that tag for the same resource id, and a tag bob's org sets for the same id is
// independent. This is defense in depth in case the handler-level project
// validation is ever bypassed or refactored.
func TestResourceProjectCrossOrgIsolation(t *testing.T) {
	f := newResourceProjectsFixture(t)
	ctx := context.Background()

	// Alice's org tags the sandbox with its project.
	pw := f.req(t, "PUT", "/console/sandboxes/"+f.sandboxID+"/project",
		`{"project_id":"`+f.projectID+`"}`, f.adminAcct, f.aliceOrg)
	if pw.Code != http.StatusOK {
		t.Fatalf("alice PUT status = %d, want 200; body=%s", pw.Code, pw.Body.String())
	}

	// Bob's org must NOT see alice's tag for the same resource id.
	got, err := f.resProj.Project(ctx, f.bobOrg, "sandbox", f.sandboxID)
	if err != nil {
		t.Fatalf("Project(bobOrg): %v", err)
	}
	if got != "" {
		t.Fatalf("bob's org sees alice's tag %q for the same sandbox id; per-org keying leaked", got)
	}

	// Alice's tag is intact.
	aliceTag, err := f.resProj.Project(ctx, f.aliceOrg, "sandbox", f.sandboxID)
	if err != nil {
		t.Fatalf("Project(aliceOrg): %v", err)
	}
	if aliceTag != f.projectID {
		t.Fatalf("alice tag = %q, want %q", aliceTag, f.projectID)
	}
}

// TestResourceProjectInspectReflects verifies that GET /console/sandboxes/{id}
// returns the sandbox with project_id set after a PUT assignment.
func TestResourceProjectInspectReflects(t *testing.T) {
	f := newResourceProjectsFixture(t)

	// Admin assigns the project.
	body := `{"project_id":"` + f.projectID + `"}`
	f.req(t, "PUT", "/console/sandboxes/"+f.sandboxID+"/project", body, f.adminAcct, f.aliceOrg)

	// Inspect the sandbox directly.
	gw := f.req(t, "GET", "/console/sandboxes/"+f.sandboxID, "", f.aliceAcct, f.aliceOrg)
	if gw.Code != http.StatusOK {
		t.Fatalf("GET /console/sandboxes/{id} status = %d, want 200; body=%s", gw.Code, gw.Body.String())
	}
	var sb SandboxView
	if err := json.Unmarshal(gw.Body.Bytes(), &sb); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, gw.Body.String())
	}
	if sb.ProjectID != f.projectID {
		t.Errorf("sandbox project_id = %q, want %q", sb.ProjectID, f.projectID)
	}
}

// TestResourceProjectMemberGetsForbidden verifies that a caller with RoleMember
// (no projects.manage) gets 403 on PUT.
func TestResourceProjectMemberGetsForbidden(t *testing.T) {
	f := newResourceProjectsFixture(t)

	body := `{"project_id":"` + f.projectID + `"}`
	pw := f.req(t, "PUT", "/console/sandboxes/"+f.sandboxID+"/project", body, f.memberAcct, f.aliceOrg)
	if pw.Code != http.StatusForbidden {
		t.Errorf("member PUT = %d, want 403; body=%s", pw.Code, pw.Body.String())
	}
}

// TestResourceProjectUnknownProjectReturns404 verifies that a PUT with a
// project_id that does not belong to the org returns 404.
func TestResourceProjectUnknownProjectReturns404(t *testing.T) {
	f := newResourceProjectsFixture(t)

	body := `{"project_id":"not-in-org"}`
	pw := f.req(t, "PUT", "/console/sandboxes/"+f.sandboxID+"/project", body, f.adminAcct, f.aliceOrg)
	if pw.Code != http.StatusNotFound {
		t.Errorf("PUT unknown project = %d, want 404; body=%s", pw.Code, pw.Body.String())
	}
}

// TestResourceProjectEmptyUnassigns verifies that a PUT with an empty
// project_id unassigns the sandbox, and a subsequent GET shows "".
func TestResourceProjectEmptyUnassigns(t *testing.T) {
	f := newResourceProjectsFixture(t)

	// First, assign a project.
	body := `{"project_id":"` + f.projectID + `"}`
	f.req(t, "PUT", "/console/sandboxes/"+f.sandboxID+"/project", body, f.adminAcct, f.aliceOrg)

	// Now unassign with empty project_id.
	uw := f.req(t, "PUT", "/console/sandboxes/"+f.sandboxID+"/project", `{"project_id":""}`, f.adminAcct, f.aliceOrg)
	if uw.Code != http.StatusOK {
		t.Fatalf("PUT empty project_id status = %d, want 200; body=%s", uw.Code, uw.Body.String())
	}

	// GET list should now show empty project_id.
	gw := f.req(t, "GET", "/console/sandboxes", "", f.aliceAcct, f.aliceOrg)
	var resp struct {
		Sandboxes []SandboxView `json:"sandboxes"`
	}
	if err := json.Unmarshal(gw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Sandboxes) != 1 {
		t.Fatalf("sandboxes = %+v, want 1", resp.Sandboxes)
	}
	if resp.Sandboxes[0].ProjectID != "" {
		t.Errorf("sandbox project_id = %q, want empty after unassign", resp.Sandboxes[0].ProjectID)
	}
}
