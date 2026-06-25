package saas

import "testing"

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
