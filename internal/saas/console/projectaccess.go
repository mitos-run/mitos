package console

import (
	"context"
	"fmt"

	"mitos.run/mitos/internal/saas"
)

// projectPermissionsFor resolves the permission set for the caller's
// per-project role assignment. It lists the ProjectMemberships for (orgID,
// projectID) and finds the entry whose AccountID matches the caller. If no
// membership is found, it returns an empty (deny-all) map. Otherwise it
// resolves the role's permission set using the same logic as permissionsFor:
// built-in roles are resolved via role.Can over knownPermissions; custom role
// names are looked up in the CustomRoles store (unknown -> empty map).
func (c *Console) projectPermissionsFor(ctx context.Context, accountID, orgID, projectID string) (map[saas.Permission]bool, error) {
	members, err := c.deps.ProjectMembers.List(ctx, orgID, projectID)
	if err != nil {
		return nil, fmt.Errorf("projectPermissionsFor: list project members: %w", err)
	}

	// Find the caller's membership in this project.
	var membership *ProjectMembership
	for i := range members {
		if members[i].AccountID == accountID {
			membership = &members[i]
			break
		}
	}
	if membership == nil {
		// No membership: deny by default.
		return map[saas.Permission]bool{}, nil
	}

	role := membership.Role

	if builtinRoles[role] {
		perms := make(map[saas.Permission]bool, len(knownPermissions))
		for _, p := range knownPermissions {
			if role.Can(p) {
				perms[p] = true
			}
		}
		return perms, nil
	}

	// Not a built-in role: look it up as a custom role.
	cr, found, err := c.deps.CustomRoles.Get(ctx, orgID, string(role))
	if err != nil {
		return nil, fmt.Errorf("projectPermissionsFor: get custom role: %w", err)
	}
	if !found {
		// Unknown custom role: deny by default.
		return map[saas.Permission]bool{}, nil
	}

	perms := make(map[saas.Permission]bool, len(cr.Permissions))
	for _, p := range cr.Permissions {
		perms[p] = true
	}
	return perms, nil
}

// canAccessSandbox returns whether the caller (accountID in orgID) may perform
// perm on the sandbox in projectID. The four-step decision is:
//
//  1. Resolve org-wide permissions via permissionsFor. On error, deny.
//  2. If the org-wide set includes PermManageProjects (Owner or Admin), grant
//     unconditionally: they manage ALL projects.
//  3. If projectID is empty, the sandbox is unassigned: apply org-wide perms.
//  4. Otherwise resolve per-project permissions via projectPermissionsFor and
//     apply those.
func (c *Console) canAccessSandbox(ctx context.Context, accountID, orgID, projectID string, perm saas.Permission) (bool, error) {
	orgPerms, err := c.permissionsFor(ctx, accountID, orgID)
	if err != nil {
		return false, fmt.Errorf("canAccessSandbox: org perms: %w", err)
	}

	// Owners and Admins (PermManageProjects) have full access to every project.
	if orgPerms[saas.PermManageProjects] {
		return true, nil
	}

	// Unassigned sandbox: use org-wide permissions.
	if projectID == "" {
		return orgPerms[perm], nil
	}

	// Assigned to a project: resolve per-project permissions.
	projPerms, err := c.projectPermissionsFor(ctx, accountID, orgID, projectID)
	if err != nil {
		return false, fmt.Errorf("canAccessSandbox: project perms: %w", err)
	}
	return projPerms[perm], nil
}
