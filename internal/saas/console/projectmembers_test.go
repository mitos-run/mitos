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

// projectMembersFixture wires a console with two orgs (alice and bob), a
// seeded project in alice's org, and an admin + member caller in alice's org
// so the permission tests have the full matrix.
type projectMembersFixture struct {
	con        *Console
	store      *saas.MemStore
	members    *MemProjectMembershipStore
	projects   *MemProjectStore
	aliceAcct  string
	aliceOrg   string
	bobAcct    string
	bobOrg     string
	adminAcct  string // RoleAdmin in alice's org: has projects.manage
	memberAcct string // RoleMember in alice's org: no projects.manage, has read
	projectID  string // seeded project in alice's org
}

func newProjectMembersFixture(t *testing.T) *projectMembersFixture {
	t.Helper()
	store := saas.NewMemStore()
	keys := saas.NewKeyService(store)
	accounts := saas.NewAccountService(store, keys)
	ctx := context.Background()

	alice, aliceOrg, err := accounts.SignUp(ctx, "pmem-alice@example.com")
	if err != nil {
		t.Fatalf("SignUp alice: %v", err)
	}
	bob, bobOrg, err := accounts.SignUp(ctx, "pmem-bob@example.com")
	if err != nil {
		t.Fatalf("SignUp bob: %v", err)
	}

	// admin is a second user with RoleAdmin in alice's org (has projects.manage).
	admin, _, err := accounts.SignUp(ctx, "pmem-admin@example.com")
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

	// member is a regular member in alice's org (no projects.manage).
	member, _, err := accounts.SignUp(ctx, "pmem-member@example.com")
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

	members := NewMemProjectMembershipStore()
	con := New(Deps{
		Accounts:       accounts,
		Projects:       projects,
		ProjectMembers: members,
		Now:            time.Now,
	})
	return &projectMembersFixture{
		con:        con,
		store:      store,
		members:    members,
		projects:   projects,
		aliceAcct:  alice.ID,
		aliceOrg:   aliceOrg.ID,
		bobAcct:    bob.ID,
		bobOrg:     bobOrg.ID,
		adminAcct:  admin.ID,
		memberAcct: member.ID,
		projectID:  p.ID,
	}
}

func (f *projectMembersFixture) req(t *testing.T, method, target, body, acct, org string) *httptest.ResponseRecorder {
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

// TestProjectMembersAdminAssignAndList verifies that an admin can POST a
// membership and GET it back in the list.
func TestProjectMembersAdminAssignAndList(t *testing.T) {
	f := newProjectMembersFixture(t)

	// Admin assigns account "x" as viewer in the project.
	body := `{"account_id":"x","role":"viewer"}`
	pw := f.req(t, "POST", "/console/projects/"+f.projectID+"/members", body, f.adminAcct, f.aliceOrg)
	if pw.Code != http.StatusOK {
		t.Fatalf("POST /console/projects/{id}/members status = %d, want 200; body=%s", pw.Code, pw.Body.String())
	}

	// GET lists the membership.
	gw := f.req(t, "GET", "/console/projects/"+f.projectID+"/members", "", f.aliceAcct, f.aliceOrg)
	if gw.Code != http.StatusOK {
		t.Fatalf("GET /console/projects/{id}/members status = %d, want 200; body=%s", gw.Code, gw.Body.String())
	}
	var resp struct {
		ProjectID string              `json:"project_id"`
		Members   []ProjectMembership `json:"members"`
	}
	if err := json.Unmarshal(gw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode GET response: %v; body=%s", err, gw.Body.String())
	}
	if resp.ProjectID != f.projectID {
		t.Errorf("project_id = %q, want %q", resp.ProjectID, f.projectID)
	}
	if len(resp.Members) != 1 {
		t.Fatalf("members = %+v, want 1 member", resp.Members)
	}
	if resp.Members[0].AccountID != "x" || resp.Members[0].Role != saas.RoleViewer {
		t.Errorf("member = %+v, want {account_id:x role:viewer}", resp.Members[0])
	}
}

// TestProjectMembersMemberCanGetButNotAssignOrRevoke verifies that a caller
// with RoleMember (no projects.manage) gets 200 on GET but 403 on POST and
// DELETE.
func TestProjectMembersMemberCanGetButNotAssignOrRevoke(t *testing.T) {
	f := newProjectMembersFixture(t)

	// member GET: allowed (PermReadOnly).
	gw := f.req(t, "GET", "/console/projects/"+f.projectID+"/members", "", f.memberAcct, f.aliceOrg)
	if gw.Code != http.StatusOK {
		t.Errorf("member GET = %d, want 200; body=%s", gw.Code, gw.Body.String())
	}

	// member POST: forbidden (no projects.manage).
	pw := f.req(t, "POST", "/console/projects/"+f.projectID+"/members",
		`{"account_id":"y","role":"viewer"}`, f.memberAcct, f.aliceOrg)
	if pw.Code != http.StatusForbidden {
		t.Errorf("member POST = %d, want 403; body=%s", pw.Code, pw.Body.String())
	}

	// member DELETE: forbidden (no projects.manage).
	dw := f.req(t, "DELETE", "/console/projects/"+f.projectID+"/members/y", "", f.memberAcct, f.aliceOrg)
	if dw.Code != http.StatusForbidden {
		t.Errorf("member DELETE = %d, want 403; body=%s", dw.Code, dw.Body.String())
	}
}

// TestProjectMembersUnknownProjectReturns404 verifies that GET/POST/DELETE on
// a project id not in the caller's org return 404.
func TestProjectMembersUnknownProjectReturns404(t *testing.T) {
	f := newProjectMembersFixture(t)

	gw := f.req(t, "GET", "/console/projects/no-such-project/members", "", f.aliceAcct, f.aliceOrg)
	if gw.Code != http.StatusNotFound {
		t.Errorf("GET unknown project = %d, want 404; body=%s", gw.Code, gw.Body.String())
	}

	pw := f.req(t, "POST", "/console/projects/no-such-project/members",
		`{"account_id":"x","role":"viewer"}`, f.adminAcct, f.aliceOrg)
	if pw.Code != http.StatusNotFound {
		t.Errorf("POST unknown project = %d, want 404; body=%s", pw.Code, pw.Body.String())
	}

	dw := f.req(t, "DELETE", "/console/projects/no-such-project/members/x", "", f.adminAcct, f.aliceOrg)
	if dw.Code != http.StatusNotFound {
		t.Errorf("DELETE unknown project = %d, want 404; body=%s", dw.Code, dw.Body.String())
	}
}

// TestProjectMembersOrgIsolation verifies that orgB cannot list orgA's project
// members. Alice's project is not visible to bob's org context; the endpoint
// returns 404 (project not found in bob's org).
func TestProjectMembersOrgIsolation(t *testing.T) {
	f := newProjectMembersFixture(t)

	// Seed a membership in alice's project.
	ctx := context.Background()
	if err := f.members.Assign(ctx, f.aliceOrg, f.projectID, "x", saas.RoleViewer); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	// Bob requests alice's project id but is authenticated as bob's org.
	gw := f.req(t, "GET", "/console/projects/"+f.projectID+"/members", "", f.bobAcct, f.bobOrg)
	// The project does not exist in bob's org so we expect 404, not a data leak.
	if gw.Code != http.StatusNotFound {
		t.Errorf("orgB GET orgA project = %d, want 404; body=%s", gw.Code, gw.Body.String())
	}
}

// TestProjectMembersRevokeRemovesMembership verifies that DELETE removes the
// membership and a subsequent GET no longer lists it.
func TestProjectMembersRevokeRemovesMembership(t *testing.T) {
	f := newProjectMembersFixture(t)

	// Assign first.
	f.req(t, "POST", "/console/projects/"+f.projectID+"/members",
		`{"account_id":"z","role":"viewer"}`, f.adminAcct, f.aliceOrg)

	// Revoke.
	dw := f.req(t, "DELETE", "/console/projects/"+f.projectID+"/members/z", "", f.adminAcct, f.aliceOrg)
	if dw.Code != http.StatusOK {
		t.Fatalf("DELETE = %d, want 200; body=%s", dw.Code, dw.Body.String())
	}

	// GET should return empty list.
	gw := f.req(t, "GET", "/console/projects/"+f.projectID+"/members", "", f.aliceAcct, f.aliceOrg)
	var resp struct {
		Members []ProjectMembership `json:"members"`
	}
	if err := json.Unmarshal(gw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	if len(resp.Members) != 0 {
		t.Errorf("after revoke, members = %+v, want empty", resp.Members)
	}
}
