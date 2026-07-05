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

// TestCanGrantRole pins the full role-grant ceiling matrix enforced by both
// InvitationService.CreateInvite and AccountService.SetMemberRole: only an
// owner may grant the owner role; every other built-in role is grantable by
// anyone (the permission gate that they hold PermManageMembers at all is a
// separate, already-enforced check).
func TestCanGrantRole(t *testing.T) {
	roles := []Role{RoleOwner, RoleAdmin, RoleBilling, RoleMember, RoleViewer}
	for _, actor := range roles {
		for _, target := range roles {
			want := target != RoleOwner || actor == RoleOwner
			if got := canGrantRole(actor, target); got != want {
				t.Errorf("canGrantRole(%s, %s) = %v, want %v", actor, target, got, want)
			}
		}
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
