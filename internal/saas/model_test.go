package saas

import "testing"

// TestSettingsPermissionOnAdminNotMember asserts that Admin (and Owner) can
// manage settings, while Member and Viewer cannot.
func TestSettingsPermissionOnAdminNotMember(t *testing.T) {
	if !RoleOwner.Can(PermManageSettings) {
		t.Fatal("owner must have settings.manage")
	}
	if !RoleAdmin.Can(PermManageSettings) {
		t.Fatal("admin must have settings.manage")
	}
	if RoleBilling.Can(PermManageSettings) {
		t.Fatal("billing must NOT have settings.manage")
	}
	if RoleMember.Can(PermManageSettings) {
		t.Fatal("member must NOT have settings.manage")
	}
	if RoleViewer.Can(PermManageSettings) {
		t.Fatal("viewer must NOT have settings.manage")
	}
}

func TestRolePermissions(t *testing.T) {
	cases := []struct {
		role Role
		perm Permission
		want bool
	}{
		{RoleOwner, PermManageMembers, true},
		{RoleOwner, PermManageBilling, true},
		{RoleAdmin, PermManageMembers, true},
		{RoleAdmin, PermManageBilling, false},
		{RoleBilling, PermManageBilling, true},
		{RoleBilling, PermManageMembers, false},
		{RoleMember, PermUseResources, true},
		{RoleMember, PermManageMembers, false},
		{RoleViewer, PermReadOnly, true},
		{RoleViewer, PermUseResources, false},
	}
	for _, c := range cases {
		if got := c.role.Can(c.perm); got != c.want {
			t.Errorf("%s.Can(%s) = %v, want %v", c.role, c.perm, got, c.want)
		}
	}
}
