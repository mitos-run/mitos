package console

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"mitos.run/mitos/internal/apierr"
	"mitos.run/mitos/internal/saas"
)

// ResourceProjectStore is the org-scoped seam that records which project a
// given resource (e.g. a sandbox) belongs to. All methods are scoped to orgID:
// a write for orgA must never affect orgB, and a read for orgA must never return
// orgB data. An unassigned resource returns "" from Project.
type ResourceProjectStore interface {
	// Project returns the project id assigned to the resource, or "" if none.
	Project(ctx context.Context, orgID, resourceType, resourceID string) (string, error)
	// SetProject assigns or clears the project for the resource. An empty
	// projectID clears the assignment.
	SetProject(ctx context.Context, orgID, resourceType, resourceID, projectID string) error
}

// MemResourceProjectStore is the in-memory tested default for
// ResourceProjectStore. It is keyed first by org so that cross-org reads always
// return "". Safe for concurrent use.
type MemResourceProjectStore struct {
	mu    sync.RWMutex
	byOrg map[string]map[string]string // orgID -> "type:id" -> projectID
}

// NewMemResourceProjectStore returns an empty in-memory resource-project store.
func NewMemResourceProjectStore() *MemResourceProjectStore {
	return &MemResourceProjectStore{
		byOrg: map[string]map[string]string{},
	}
}

func storeKey(resourceType, resourceID string) string {
	return resourceType + ":" + resourceID
}

// Project returns the project id assigned to the resource for the org, or "" if
// none is assigned.
func (m *MemResourceProjectStore) Project(_ context.Context, orgID, resourceType, resourceID string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.byOrg[orgID] == nil {
		return "", nil
	}
	return m.byOrg[orgID][storeKey(resourceType, resourceID)], nil
}

// SetProject assigns or clears the project for the resource. An empty projectID
// removes the mapping from the store.
func (m *MemResourceProjectStore) SetProject(_ context.Context, orgID, resourceType, resourceID, projectID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if projectID == "" {
		if m.byOrg[orgID] != nil {
			delete(m.byOrg[orgID], storeKey(resourceType, resourceID))
		}
		return nil
	}
	if m.byOrg[orgID] == nil {
		m.byOrg[orgID] = map[string]string{}
	}
	m.byOrg[orgID][storeKey(resourceType, resourceID)] = projectID
	return nil
}

// setSandboxProjectRequest is the body of PUT /console/sandboxes/{id}/project.
type setSandboxProjectRequest struct {
	ProjectID string `json:"project_id"`
}

// handleSetSandboxProject assigns or clears the project for a sandbox. Gated by
// PermManageProjects. If project_id is non-empty it must belong to the caller's
// org; empty project_id clears the assignment.
func (c *Console) handleSetSandboxProject(w http.ResponseWriter, r *http.Request) {
	_, orgID, e, ok := c.authorize(r, saas.PermManageProjects)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	sandboxID := r.PathValue("id")
	var req setSandboxProjectRequest
	if err := decodeBody(r, &req); err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidJSON).
			WithCause("the set-sandbox-project body is not valid JSON"))
		return
	}
	if req.ProjectID != "" {
		found, err := c.validateProjectInOrg(r, orgID, req.ProjectID)
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
	}
	if err := c.deps.ResourceProjects.SetProject(r.Context(), orgID, "sandbox", sandboxID, req.ProjectID); err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).
			WithCause(fmt.Sprintf("the sandbox project could not be stored: %s", err.Error())))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sandbox_id": sandboxID,
		"project_id": req.ProjectID,
	})
}
