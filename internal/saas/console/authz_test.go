package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"mitos.run/mitos/internal/saas"
)

// authzFixture builds a minimal console with a real AccountService so MemberRole
// resolves from actual memberships. It returns the console, the account service,
// and the in-memory store so tests can seed memberships and custom roles.
type authzFixture struct {
	con         *Console
	accounts    *saas.AccountService
	store       *saas.MemStore
	customRoles *MemCustomRoleStore
}

func newAuthzFixture(t *testing.T) *authzFixture {
	t.Helper()
	store := saas.NewMemStore()
	keys := saas.NewKeyService(store)
	accounts := saas.NewAccountService(store, keys)
	cr := NewMemCustomRoleStore()
	con := New(Deps{
		Accounts:    accounts,
		CustomRoles: cr,
		Now:         time.Now,
	})
	return &authzFixture{
		con:         con,
		accounts:    accounts,
		store:       store,
		customRoles: cr,
	}
}

// TestAuthzAdminPermissions verifies that a built-in Admin resolves to a set
// containing secrets.manage and settings.manage but NOT billing.manage.
func TestAuthzAdminPermissions(t *testing.T) {
	f := newAuthzFixture(t)
	ctx := context.Background()

	// Sign up an account and seed it as an admin of its own org.
	admin, adminOrg, err := f.accounts.SignUp(ctx, "admin-authz@example.com")
	if err != nil {
		t.Fatalf("SignUp admin: %v", err)
	}
	if err := f.store.PutMembership(ctx, saas.Membership{
		AccountID: admin.ID,
		OrgID:     adminOrg.ID,
		Role:      saas.RoleAdmin,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed admin membership: %v", err)
	}

	perms, err := f.con.permissionsFor(ctx, admin.ID, adminOrg.ID)
	if err != nil {
		t.Fatalf("permissionsFor: %v", err)
	}
	if !perms[saas.PermManageSecrets] {
		t.Error("admin must have secrets.manage")
	}
	if !perms[saas.PermManageSettings] {
		t.Error("admin must have settings.manage")
	}
	if perms[saas.PermManageBilling] {
		t.Error("admin must NOT have billing.manage")
	}
}

// TestAuthzCustomRoleAuditor verifies that a custom role "auditor" with only
// {read} resolves to read-only, and authorize for secrets.manage is denied
// with a 403.
func TestAuthzCustomRoleAuditor(t *testing.T) {
	f := newAuthzFixture(t)
	ctx := context.Background()

	// Sign up a user; we will assign them the custom role "auditor".
	user, org, err := f.accounts.SignUp(ctx, "auditor-authz@example.com")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}

	// Upsert the custom role into the store.
	if err := f.customRoles.Upsert(ctx, org.ID, CustomRole{
		Name:        "auditor",
		Permissions: []saas.Permission{saas.PermReadOnly},
	}); err != nil {
		t.Fatalf("Upsert auditor role: %v", err)
	}

	// Assign the user the custom role name via a membership with role = "auditor".
	if err := f.store.PutMembership(ctx, saas.Membership{
		AccountID: user.ID,
		OrgID:     org.ID,
		Role:      saas.Role("auditor"),
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed auditor membership: %v", err)
	}

	// Permissions should be read-only.
	perms, err := f.con.permissionsFor(ctx, user.ID, org.ID)
	if err != nil {
		t.Fatalf("permissionsFor: %v", err)
	}
	if !perms[saas.PermReadOnly] {
		t.Error("auditor must have read permission")
	}
	if perms[saas.PermManageSecrets] {
		t.Error("auditor must NOT have secrets.manage")
	}

	// authorize for secrets.manage must return 403.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(WithCaller(r.Context(), user.ID, org.ID))
	_, _, apiErr, ok := f.con.authorize(r, saas.PermManageSecrets)
	if ok {
		t.Error("authorize(secrets.manage) must return ok=false for auditor")
	}
	if apiErr.Status != http.StatusForbidden {
		t.Errorf("authorize status = %d, want 403", apiErr.Status)
	}
}

// TestAuthzMemberRoleErrorIsInternalNotForbidden verifies the error-vs-deny
// distinction: a caller whose role cannot be resolved (no membership in the
// context org) must yield a 500, NOT a 403. A lookup failure must never be
// silently masked as a clean deny.
func TestAuthzMemberRoleErrorIsInternalNotForbidden(t *testing.T) {
	f := newAuthzFixture(t)
	ctx := context.Background()

	// A real account, but with NO membership in the org we attach to the request.
	user, _, err := f.accounts.SignUp(ctx, "stranger-authz@example.com")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(WithCaller(r.Context(), user.ID, "org-the-user-does-not-belong-to"))
	_, _, apiErr, ok := f.con.authorize(r, saas.PermReadOnly)
	if ok {
		t.Fatal("authorize must not succeed when the role cannot be resolved")
	}
	if apiErr.Status != http.StatusInternalServerError {
		t.Errorf("authorize status = %d, want 500 (an unresolved role is an error, not a clean 403)", apiErr.Status)
	}
}

// TestAuthzStoreIsolation verifies that orgB never sees orgA's custom roles.
func TestAuthzStoreIsolation(t *testing.T) {
	f := newAuthzFixture(t)
	ctx := context.Background()

	// Upsert a role for orgA only.
	if err := f.customRoles.Upsert(ctx, "orgA", CustomRole{
		Name:        "super",
		Permissions: []saas.Permission{saas.PermManageSecrets},
	}); err != nil {
		t.Fatalf("Upsert orgA role: %v", err)
	}

	// orgB must not see orgA's role.
	roles, err := f.customRoles.List(ctx, "orgB")
	if err != nil {
		t.Fatalf("List orgB: %v", err)
	}
	if len(roles) != 0 {
		t.Errorf("orgB sees %d roles, want 0 (cross-org leak)", len(roles))
	}

	// Get for orgB returns not-found.
	_, found, err := f.customRoles.Get(ctx, "orgB", "super")
	if err != nil {
		t.Fatalf("Get orgB/super: %v", err)
	}
	if found {
		t.Error("orgB must not find orgA's 'super' role")
	}
}

// TestAuthzDenyByDefault verifies that an unknown role name resolves to an
// empty permission set and authorize denies.
func TestAuthzDenyByDefault(t *testing.T) {
	f := newAuthzFixture(t)
	ctx := context.Background()

	user, org, err := f.accounts.SignUp(ctx, "unknown-role@example.com")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}

	// Assign the user an unknown role name with no corresponding custom role.
	if err := f.store.PutMembership(ctx, saas.Membership{
		AccountID: user.ID,
		OrgID:     org.ID,
		Role:      saas.Role("nonexistent-role"),
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed unknown role membership: %v", err)
	}

	perms, err := f.con.permissionsFor(ctx, user.ID, org.ID)
	if err != nil {
		t.Fatalf("permissionsFor must not error for unknown role: %v", err)
	}
	if len(perms) != 0 {
		t.Errorf("unknown role must resolve to empty permission set, got %v", perms)
	}

	// authorize for ANY permission must deny.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(WithCaller(r.Context(), user.ID, org.ID))
	_, _, _, ok := f.con.authorize(r, saas.PermReadOnly)
	if ok {
		t.Error("authorize must deny for unknown role (deny by default)")
	}
}

// TestCustomRoleStoreUpsertAndGet verifies basic store CRUD operations.
func TestCustomRoleStoreUpsertAndGet(t *testing.T) {
	store := NewMemCustomRoleStore()
	ctx := context.Background()

	role := CustomRole{Name: "editor", Permissions: []saas.Permission{saas.PermManageProjects, saas.PermReadOnly}}
	if err := store.Upsert(ctx, "org1", role); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, found, err := store.Get(ctx, "org1", "editor")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Fatal("Get returned not-found after Upsert")
	}
	if got.Name != "editor" || len(got.Permissions) != 2 {
		t.Errorf("Get = %+v, want editor with 2 permissions", got)
	}
}

// TestCustomRoleStoreDelete verifies that Delete removes the role and that
// Get returns not-found afterward.
func TestCustomRoleStoreDelete(t *testing.T) {
	store := NewMemCustomRoleStore()
	ctx := context.Background()

	if err := store.Upsert(ctx, "org1", CustomRole{Name: "temp", Permissions: []saas.Permission{saas.PermReadOnly}}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := store.Delete(ctx, "org1", "temp"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, found, err := store.Get(ctx, "org1", "temp")
	if err != nil {
		t.Fatalf("Get after Delete: %v", err)
	}
	if found {
		t.Error("Get must return not-found after Delete")
	}
}

// TestPermissionsForOwnerHasBilling verifies that the built-in Owner role
// includes billing.manage (sanity check that built-in resolution is correct).
func TestPermissionsForOwnerHasBilling(t *testing.T) {
	f := newAuthzFixture(t)
	ctx := context.Background()

	owner, org, err := f.accounts.SignUp(ctx, "owner-billing@example.com")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}

	perms, err := f.con.permissionsFor(ctx, owner.ID, org.ID)
	if err != nil {
		t.Fatalf("permissionsFor: %v", err)
	}
	if !perms[saas.PermManageBilling] {
		t.Error("owner must have billing.manage")
	}
}
