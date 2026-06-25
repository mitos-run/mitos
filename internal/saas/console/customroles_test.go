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

// rolesFixture wires a console with two orgs so isolation tests can verify
// that orgB's GET /console/roles does not return orgA's custom roles.
type rolesFixture struct {
	con        *Console
	store      *saas.MemStore
	roles      *MemCustomRoleStore
	aliceAcct  string
	aliceOrg   string
	bobAcct    string
	bobOrg     string
	memberAcct string // member role: PermReadOnly only (viewer)
}

func newRolesFixture(t *testing.T) *rolesFixture {
	t.Helper()
	store := saas.NewMemStore()
	keys := saas.NewKeyService(store)
	accounts := saas.NewAccountService(store, keys)
	ctx := context.Background()

	alice, aliceOrg, err := accounts.SignUp(ctx, "roles-alice@example.com")
	if err != nil {
		t.Fatalf("SignUp alice: %v", err)
	}
	bob, bobOrg, err := accounts.SignUp(ctx, "roles-bob@example.com")
	if err != nil {
		t.Fatalf("SignUp bob: %v", err)
	}

	// carol is a viewer in alice's org: she has PermReadOnly but not PermManageSettings.
	carol, _, err := accounts.SignUp(ctx, "roles-carol@example.com")
	if err != nil {
		t.Fatalf("SignUp carol: %v", err)
	}
	if err := store.PutMembership(ctx, saas.Membership{
		AccountID: carol.ID,
		OrgID:     aliceOrg.ID,
		Role:      saas.RoleViewer,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed carol membership: %v", err)
	}

	roles := NewMemCustomRoleStore()
	con := New(Deps{
		Accounts:    accounts,
		CustomRoles: roles,
		Now:         time.Now,
	})
	return &rolesFixture{
		con:        con,
		store:      store,
		roles:      roles,
		aliceAcct:  alice.ID,
		aliceOrg:   aliceOrg.ID,
		bobAcct:    bob.ID,
		bobOrg:     bobOrg.ID,
		memberAcct: carol.ID,
	}
}

// req dispatches a request to the console and returns the recorder.
func (f *rolesFixture) req(t *testing.T, method, target, body, acct, org string) *httptest.ResponseRecorder {
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

// TestCustomRolesAdminUpsertAndGet verifies that an admin (alice, owner) can
// POST a custom role and GET /console/roles returns it under "custom" alongside
// the 5 built-in role descriptions.
func TestCustomRolesAdminUpsertAndGet(t *testing.T) {
	f := newRolesFixture(t)

	// POST the custom role as alice (owner, has PermManageSettings).
	pw := f.req(t, "POST", "/console/roles",
		`{"name":"auditor","permissions":["read"]}`,
		f.aliceAcct, f.aliceOrg)
	if pw.Code != http.StatusOK {
		t.Fatalf("POST /console/roles status = %d, want 200; body=%s", pw.Code, pw.Body.String())
	}
	var upserted CustomRole
	if err := json.Unmarshal(pw.Body.Bytes(), &upserted); err != nil {
		t.Fatalf("decode upsert response: %v; body=%s", err, pw.Body.String())
	}
	if upserted.Name != "auditor" {
		t.Errorf("upserted.Name = %q, want auditor", upserted.Name)
	}

	// GET /console/roles as alice.
	gw := f.req(t, "GET", "/console/roles", "", f.aliceAcct, f.aliceOrg)
	if gw.Code != http.StatusOK {
		t.Fatalf("GET /console/roles status = %d, want 200; body=%s", gw.Code, gw.Body.String())
	}
	var resp struct {
		OrgID    string       `json:"org_id"`
		Builtins []rolesEntry `json:"builtins"`
		Custom   []CustomRole `json:"custom"`
	}
	if err := json.Unmarshal(gw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode GET response: %v; body=%s", err, gw.Body.String())
	}

	if resp.OrgID != f.aliceOrg {
		t.Errorf("org_id = %q, want aliceOrg", resp.OrgID)
	}

	// The 5 built-in roles must be present.
	if len(resp.Builtins) != 5 {
		t.Errorf("builtins count = %d, want 5; got %+v", len(resp.Builtins), resp.Builtins)
	}
	builtinNames := map[string]bool{}
	for _, b := range resp.Builtins {
		builtinNames[b.Name] = true
	}
	for _, name := range []string{"owner", "admin", "billing", "member", "viewer"} {
		if !builtinNames[name] {
			t.Errorf("builtin %q missing from builtins list", name)
		}
	}

	// The custom role must appear under "custom".
	if len(resp.Custom) != 1 || resp.Custom[0].Name != "auditor" {
		t.Errorf("custom roles = %+v, want [{auditor [read]}]", resp.Custom)
	}
}

// rolesEntry is the shape of one builtin entry in the GET /console/roles response.
type rolesEntry struct {
	Name        string            `json:"name"`
	Permissions []saas.Permission `json:"permissions"`
}

// TestCustomRolesMemberCanGetButNotWriteOrDelete verifies that a viewer (carol)
// gets 200 on GET, 403 on POST, and 403 on DELETE.
func TestCustomRolesMemberCanGetButNotWriteOrDelete(t *testing.T) {
	f := newRolesFixture(t)

	// Viewer can GET (PermReadOnly).
	gw := f.req(t, "GET", "/console/roles", "", f.memberAcct, f.aliceOrg)
	if gw.Code != http.StatusOK {
		t.Errorf("viewer GET /console/roles = %d, want 200; body=%s", gw.Code, gw.Body.String())
	}

	// Viewer cannot POST (requires PermManageSettings).
	pw := f.req(t, "POST", "/console/roles",
		`{"name":"myrole","permissions":["read"]}`,
		f.memberAcct, f.aliceOrg)
	if pw.Code != http.StatusForbidden {
		t.Errorf("viewer POST /console/roles = %d, want 403; body=%s", pw.Code, pw.Body.String())
	}

	// Viewer cannot DELETE (requires PermManageSettings).
	dw := f.req(t, "DELETE", "/console/roles/myrole", "", f.memberAcct, f.aliceOrg)
	if dw.Code != http.StatusForbidden {
		t.Errorf("viewer DELETE /console/roles/myrole = %d, want 403; body=%s", dw.Code, dw.Body.String())
	}
}

// TestCustomRolesValidationRejectsBuiltinName verifies that POST with a name
// that matches a built-in role (e.g. "admin") returns 400.
func TestCustomRolesValidationRejectsBuiltinName(t *testing.T) {
	f := newRolesFixture(t)
	w := f.req(t, "POST", "/console/roles",
		`{"name":"admin","permissions":["read"]}`,
		f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusBadRequest {
		t.Errorf("POST with builtin name = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestCustomRolesValidationRejectsUnknownPermission verifies that POST with a
// permission not in knownPermissions returns 400.
func TestCustomRolesValidationRejectsUnknownPermission(t *testing.T) {
	f := newRolesFixture(t)
	w := f.req(t, "POST", "/console/roles",
		`{"name":"myrole","permissions":["god.mode"]}`,
		f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusBadRequest {
		t.Errorf("POST with unknown permission = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestCustomRolesValidationRejectsEmptyName verifies that POST with an empty
// name returns 400.
func TestCustomRolesValidationRejectsEmptyName(t *testing.T) {
	f := newRolesFixture(t)
	w := f.req(t, "POST", "/console/roles",
		`{"name":"","permissions":["read"]}`,
		f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusBadRequest {
		t.Errorf("POST with empty name = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestCustomRolesOrgIsolation verifies that orgB's GET /console/roles does not
// return orgA's custom roles.
func TestCustomRolesOrgIsolation(t *testing.T) {
	f := newRolesFixture(t)

	// Seed a custom role for alice's org.
	f.req(t, "POST", "/console/roles",
		`{"name":"auditor","permissions":["read"]}`,
		f.aliceAcct, f.aliceOrg)

	// Bob reads his own org's roles.
	gw := f.req(t, "GET", "/console/roles", "", f.bobAcct, f.bobOrg)
	if gw.Code != http.StatusOK {
		t.Fatalf("bob GET /console/roles = %d, want 200; body=%s", gw.Code, gw.Body.String())
	}
	var resp struct {
		Custom []CustomRole `json:"custom"`
	}
	if err := json.Unmarshal(gw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode bob GET response: %v", err)
	}
	if len(resp.Custom) != 0 {
		t.Errorf("bob sees orgA custom roles: %+v", resp.Custom)
	}
}

// TestCustomRolesDeleteRemovesRole verifies that DELETE removes the role and
// subsequent GET does not return it.
func TestCustomRolesDeleteRemovesRole(t *testing.T) {
	f := newRolesFixture(t)

	// Upsert a role first.
	f.req(t, "POST", "/console/roles",
		`{"name":"auditor","permissions":["read"]}`,
		f.aliceAcct, f.aliceOrg)

	// Delete it.
	dw := f.req(t, "DELETE", "/console/roles/auditor", "", f.aliceAcct, f.aliceOrg)
	if dw.Code != http.StatusOK {
		t.Fatalf("DELETE /console/roles/auditor = %d, want 200; body=%s", dw.Code, dw.Body.String())
	}

	// GET should show no custom roles.
	gw := f.req(t, "GET", "/console/roles", "", f.aliceAcct, f.aliceOrg)
	var resp struct {
		Custom []CustomRole `json:"custom"`
	}
	if err := json.Unmarshal(gw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	if len(resp.Custom) != 0 {
		t.Errorf("after delete, custom roles = %+v, want empty", resp.Custom)
	}
}
