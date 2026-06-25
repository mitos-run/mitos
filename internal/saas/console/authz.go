package console

import (
	"context"
	"net/http"

	"mitos.run/mitos/internal/apierr"
	"mitos.run/mitos/internal/saas"
)

// knownPermissions is the exhaustive set of permissions the console enforces.
// Built-in role resolution iterates this slice to build a permission map via
// role.Can, so adding a new permission here automatically extends the resolver
// without touching rolePerms (which is unexported in the saas package).
var knownPermissions = []saas.Permission{
	saas.PermManageMembers,
	saas.PermManageProjects,
	saas.PermManageSecrets,
	saas.PermManageSettings,
	saas.PermManageBilling,
	saas.PermUseResources,
	saas.PermReadOnly,
}

// builtinRoles is the set of role strings that are built-in to the saas
// package. If MemberRole returns one of these, we resolve permissions via
// role.Can rather than looking up a custom role.
var builtinRoles = map[saas.Role]bool{
	saas.RoleOwner:   true,
	saas.RoleAdmin:   true,
	saas.RoleBilling: true,
	saas.RoleMember:  true,
	saas.RoleViewer:  true,
}

// permissionsFor resolves the caller's permission set. It calls MemberRole to
// get the caller's role string; if it is a built-in role, it returns the set
// produced by role.Can over knownPermissions. Otherwise it looks up the role
// string as a custom role name in the CustomRoles store. An unknown or missing
// custom role name returns an EMPTY map (deny by default), not an error. A
// MemberRole lookup error propagates unchanged.
func (c *Console) permissionsFor(ctx context.Context, accountID, orgID string) (map[saas.Permission]bool, error) {
	role, err := c.deps.Accounts.MemberRole(ctx, accountID, orgID)
	if err != nil {
		return nil, err
	}

	if builtinRoles[role] {
		perms := make(map[saas.Permission]bool, len(knownPermissions))
		for _, p := range knownPermissions {
			if role.Can(p) {
				perms[p] = true
			}
		}
		return perms, nil
	}

	// The role string is not a built-in; look it up as a custom role.
	cr, found, err := c.deps.CustomRoles.Get(ctx, orgID, string(role))
	if err != nil {
		return nil, err
	}
	if !found {
		// Unknown role: deny by default.
		return map[saas.Permission]bool{}, nil
	}

	perms := make(map[saas.Permission]bool, len(cr.Permissions))
	for _, p := range cr.Permissions {
		perms[p] = true
	}
	return perms, nil
}

// authorize is the single authorization gate used by console endpoints. It
// calls caller to extract the accountID and orgID from the request context,
// then resolves the caller's permission set via permissionsFor. If the
// required permission is absent, it returns a 403 apierr with ok=false. On
// success it returns accountID, orgID, and ok=true.
func (c *Console) authorize(r *http.Request, perm saas.Permission) (accountID, orgID string, e apierr.Error, ok bool) {
	accountID, orgID, e, ok = c.caller(r)
	if !ok {
		return accountID, orgID, e, false
	}

	perms, err := c.permissionsFor(r.Context(), accountID, orgID)
	if err != nil {
		return accountID, orgID, apierr.Get(apierr.CodeInternal).
			WithCause("the permission check could not be completed"), false
	}

	if !perms[perm] {
		return accountID, orgID, apierr.Get(apierr.CodeForbidden).
			WithCause("the caller's role does not grant " + string(perm)), false
	}

	var noErr apierr.Error
	return accountID, orgID, noErr, true
}
