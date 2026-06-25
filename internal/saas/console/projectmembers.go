package console

import (
	"context"
	"net/http"
	"sync"

	"mitos.run/mitos/internal/apierr"
	"mitos.run/mitos/internal/saas"
)

// ProjectMembership is a per-project role assignment: one account holds a role
// within one project in one org. The org is never carried over the wire; it is
// resolved from the caller's authenticated context.
type ProjectMembership struct {
	AccountID string    `json:"account_id"`
	ProjectID string    `json:"project_id"`
	Role      saas.Role `json:"role"`
}

// ProjectMembershipStore is the per-org, per-project membership seam. All
// methods are scoped to both orgID and projectID: a write for orgA must never
// affect orgB, and a read for orgA must never return orgB's memberships.
type ProjectMembershipStore interface {
	// List returns all memberships for the project in the given org.
	List(ctx context.Context, orgID, projectID string) ([]ProjectMembership, error)
	// Assign creates or replaces the account's role in the project.
	Assign(ctx context.Context, orgID, projectID, accountID string, role saas.Role) error
	// Revoke removes the account's membership from the project. It is not an
	// error to revoke a membership that does not exist.
	Revoke(ctx context.Context, orgID, projectID, accountID string) error
}

// MemProjectMembershipStore is the in-memory tested default. It is keyed first
// by org then by project: cross-org reads always return an empty slice.
type MemProjectMembershipStore struct {
	mu    sync.RWMutex
	byOrg map[string]map[string]map[string]ProjectMembership
}

// NewMemProjectMembershipStore returns an empty in-memory membership store.
func NewMemProjectMembershipStore() *MemProjectMembershipStore {
	return &MemProjectMembershipStore{
		byOrg: map[string]map[string]map[string]ProjectMembership{},
	}
}

// List returns all memberships for the project in the given org (copies; never
// cross-org).
func (m *MemProjectMembershipStore) List(_ context.Context, orgID, projectID string) ([]ProjectMembership, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	src := m.byOrg[orgID][projectID]
	out := make([]ProjectMembership, 0, len(src))
	for _, pm := range src {
		out = append(out, pm)
	}
	return out, nil
}

// Assign creates or replaces the account's role within the project for the org.
func (m *MemProjectMembershipStore) Assign(_ context.Context, orgID, projectID, accountID string, role saas.Role) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byOrg[orgID] == nil {
		m.byOrg[orgID] = map[string]map[string]ProjectMembership{}
	}
	if m.byOrg[orgID][projectID] == nil {
		m.byOrg[orgID][projectID] = map[string]ProjectMembership{}
	}
	m.byOrg[orgID][projectID][accountID] = ProjectMembership{
		AccountID: accountID,
		ProjectID: projectID,
		Role:      role,
	}
	return nil
}

// Revoke removes the account's membership from the project. It is not an error
// to revoke a membership that does not exist.
func (m *MemProjectMembershipStore) Revoke(_ context.Context, orgID, projectID, accountID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byOrg[orgID] == nil || m.byOrg[orgID][projectID] == nil {
		return nil
	}
	delete(m.byOrg[orgID][projectID], accountID)
	return nil
}

// --- Handlers ---

// validateProjectInOrg checks that the given project id belongs to the caller's
// org. It lists the org's projects and returns (true, nil) if found,
// (false, nil) if not found, or (false, err) on store failure.
func (c *Console) validateProjectInOrg(r *http.Request, orgID, projectID string) (bool, error) {
	projects, err := c.deps.Projects.List(r.Context(), orgID)
	if err != nil {
		return false, err
	}
	for _, p := range projects {
		if p.ID == projectID {
			return true, nil
		}
	}
	return false, nil
}

// handleListProjectMembers returns the list of per-project role assignments for
// the caller's org and project. Gated by PermReadOnly.
func (c *Console) handleListProjectMembers(w http.ResponseWriter, r *http.Request) {
	_, orgID, e, ok := c.authorize(r, saas.PermReadOnly)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	projectID := r.PathValue("id")
	found, err := c.validateProjectInOrg(r, orgID, projectID)
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).
			WithCause("the project list could not be read"))
		return
	}
	if !found {
		apierr.Encode(w, apierr.Get(apierr.CodeNotFound).
			WithCause("the project does not exist or does not belong to this organization"))
		return
	}
	memberships, err := c.deps.ProjectMembers.List(r.Context(), orgID, projectID)
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).
			WithCause("the project members could not be listed"))
		return
	}
	if memberships == nil {
		memberships = []ProjectMembership{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project_id": projectID,
		"members":    memberships,
	})
}

// assignMemberRequest is the body of POST /console/projects/{id}/members.
type assignMemberRequest struct {
	AccountID string    `json:"account_id"`
	Role      saas.Role `json:"role"`
}

// handleAssignProjectMember assigns an account to a role in the project. Gated
// by PermManageProjects.
func (c *Console) handleAssignProjectMember(w http.ResponseWriter, r *http.Request) {
	_, orgID, e, ok := c.authorize(r, saas.PermManageProjects)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	projectID := r.PathValue("id")
	found, err := c.validateProjectInOrg(r, orgID, projectID)
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).
			WithCause("the project list could not be read"))
		return
	}
	if !found {
		apierr.Encode(w, apierr.Get(apierr.CodeNotFound).
			WithCause("the project does not exist or does not belong to this organization"))
		return
	}
	var req assignMemberRequest
	if err := decodeBody(r, &req); err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidJSON).
			WithCause("the assign-member body is not valid JSON"))
		return
	}
	if err := c.deps.ProjectMembers.Assign(r.Context(), orgID, projectID, req.AccountID, req.Role); err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).
			WithCause("the project member could not be assigned"))
		return
	}
	writeJSON(w, http.StatusOK, ProjectMembership{
		AccountID: req.AccountID,
		ProjectID: projectID,
		Role:      req.Role,
	})
}

// handleRevokeProjectMember removes an account's membership from a project.
// Gated by PermManageProjects.
func (c *Console) handleRevokeProjectMember(w http.ResponseWriter, r *http.Request) {
	_, orgID, e, ok := c.authorize(r, saas.PermManageProjects)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	projectID := r.PathValue("id")
	found, err := c.validateProjectInOrg(r, orgID, projectID)
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).
			WithCause("the project list could not be read"))
		return
	}
	if !found {
		apierr.Encode(w, apierr.Get(apierr.CodeNotFound).
			WithCause("the project does not exist or does not belong to this organization"))
		return
	}
	accountID := r.PathValue("accountID")
	if err := c.deps.ProjectMembers.Revoke(r.Context(), orgID, projectID, accountID); err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).
			WithCause("the project member could not be revoked"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project_id": projectID,
		"revoked":    accountID,
	})
}
