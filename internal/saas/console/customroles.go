package console

import (
	"context"
	"sync"

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
