package console

import (
	"context"
	"net/http"
	"sync"

	"mitos.run/mitos/internal/apierr"
	"mitos.run/mitos/internal/saas"
)

// CustomRole is a named permission set that an org administrator can define.
// It is referenced by a membership's Role string when the string does not
// match any built-in role. Name and Permissions are both required.
type CustomRole struct {
	Name        string            `json:"name"`
	Permissions []saas.Permission `json:"permissions"`
}

// CustomRoleStore is the org-scoped seam for custom-role definitions. All
// methods are scoped to the supplied orgID: a write for orgA must never affect
// orgB, and a read for orgA must never return orgB's roles.
type CustomRoleStore interface {
	// List returns all custom roles defined for the org.
	List(ctx context.Context, orgID string) ([]CustomRole, error)
	// Get returns the named custom role for the org and a found flag. If the
	// role does not exist, found is false and err is nil.
	Get(ctx context.Context, orgID, name string) (CustomRole, bool, error)
	// Upsert creates or replaces the custom role for the org.
	Upsert(ctx context.Context, orgID string, role CustomRole) error
	// Delete removes the named custom role for the org. It is not an error to
	// delete a role that does not exist.
	Delete(ctx context.Context, orgID, name string) error
}

// MemCustomRoleStore is the in-memory tested default for CustomRoleStore. It
// is safe for concurrent use and never allows one org's roles to appear under
// another org's namespace.
type MemCustomRoleStore struct {
	mu    sync.RWMutex
	byOrg map[string]map[string]CustomRole
}

// NewMemCustomRoleStore returns an empty in-memory custom-role store.
func NewMemCustomRoleStore() *MemCustomRoleStore {
	return &MemCustomRoleStore{byOrg: map[string]map[string]CustomRole{}}
}

// List returns all custom roles for the org (copies; cross-org never leaks).
func (m *MemCustomRoleStore) List(_ context.Context, orgID string) ([]CustomRole, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	src := m.byOrg[orgID]
	out := make([]CustomRole, 0, len(src))
	for _, r := range src {
		out = append(out, r)
	}
	return out, nil
}

// Get returns the named custom role for the org and a found flag.
func (m *MemCustomRoleStore) Get(_ context.Context, orgID, name string) (CustomRole, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.byOrg[orgID][name]
	return r, ok, nil
}

// Upsert creates or replaces the named custom role for the org.
func (m *MemCustomRoleStore) Upsert(_ context.Context, orgID string, role CustomRole) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byOrg[orgID] == nil {
		m.byOrg[orgID] = map[string]CustomRole{}
	}
	m.byOrg[orgID][role.Name] = role
	return nil
}

// Delete removes the named custom role from the org. It is not an error to
// delete a role that does not exist.
func (m *MemCustomRoleStore) Delete(_ context.Context, orgID, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.byOrg[orgID], name)
	return nil
}

// --- Handlers ---

// builtinRoleEntry is the wire shape of one built-in role in the GET
// /console/roles response. The permissions list is derived by testing
// role.Can over knownPermissions so the response is always in step with the
// saas package's built-in permission matrix.
type builtinRoleEntry struct {
	Name        string            `json:"name"`
	Permissions []saas.Permission `json:"permissions"`
}

// handleListRoles returns the org's custom roles alongside the five built-in
// role permission sets so the UI can render a complete permission matrix.
// Gated by PermReadOnly: any org member can read the matrix.
func (c *Console) handleListRoles(w http.ResponseWriter, r *http.Request) {
	_, orgID, e, ok := c.authorize(r, saas.PermReadOnly)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	// Build the built-in role list by probing role.Can over knownPermissions.
	builtins := make([]builtinRoleEntry, 0, len(builtinRoles))
	for role := range builtinRoles {
		perms := make([]saas.Permission, 0, len(knownPermissions))
		for _, p := range knownPermissions {
			if role.Can(p) {
				perms = append(perms, p)
			}
		}
		builtins = append(builtins, builtinRoleEntry{Name: string(role), Permissions: perms})
	}
	// List custom roles for the org.
	custom, err := c.deps.CustomRoles.List(r.Context(), orgID)
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).
			WithCause("the custom roles could not be listed"))
		return
	}
	if custom == nil {
		custom = []CustomRole{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"org_id":   orgID,
		"builtins": builtins,
		"custom":   custom,
	})
}

// handleUpsertRole creates or replaces a custom role for the org. Gated by
// PermManageSettings. Validates: non-empty name; name not a built-in role;
// all permissions in knownPermissions.
func (c *Console) handleUpsertRole(w http.ResponseWriter, r *http.Request) {
	_, orgID, e, ok := c.authorize(r, saas.PermManageSettings)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	var role CustomRole
	if err := decodeBody(r, &role); err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidJSON).
			WithCause("the role body is not valid JSON"))
		return
	}
	// Validate: name must not be empty.
	if role.Name == "" {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidJSON).
			WithCause("the role name must not be empty"))
		return
	}
	// Validate: name must not collide with a built-in role.
	if builtinRoles[saas.Role(role.Name)] {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidJSON).
			WithCause("the role name collides with a built-in role: "+role.Name))
		return
	}
	// Validate: all permissions must be in knownPermissions.
	known := make(map[saas.Permission]bool, len(knownPermissions))
	for _, p := range knownPermissions {
		known[p] = true
	}
	for _, p := range role.Permissions {
		if !known[p] {
			apierr.Encode(w, apierr.Get(apierr.CodeInvalidJSON).
				WithCause("unknown permission: "+string(p)+"; must be one of the known permissions"))
			return
		}
	}
	if err := c.deps.CustomRoles.Upsert(r.Context(), orgID, role); err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).
			WithCause("the custom role could not be stored"))
		return
	}
	writeJSON(w, http.StatusOK, role)
}

// handleDeleteRole removes a custom role for the org. Gated by
// PermManageSettings. Deleting a non-existent role is not an error.
func (c *Console) handleDeleteRole(w http.ResponseWriter, r *http.Request) {
	_, orgID, e, ok := c.authorize(r, saas.PermManageSettings)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	name := r.PathValue("name")
	if err := c.deps.CustomRoles.Delete(r.Context(), orgID, name); err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).
			WithCause("the custom role could not be deleted"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"org_id": orgID, "deleted": name})
}
