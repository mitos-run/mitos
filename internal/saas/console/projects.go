package console

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Project is one org's project container. Projects scope resources (sandboxes,
// secrets, templates) and the per-project role assignments introduced in B3.
type Project struct {
	ID          string    `json:"id"`
	OrgID       string    `json:"org_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

// ProjectStore is the org-scoped seam the projects view reads and writes. List
// MUST return only the named org's projects; Create appends a new project and
// returns it. The real implementation writes to the database; the in-memory
// fake is the tested default.
type ProjectStore interface {
	List(ctx context.Context, orgID string) ([]Project, error)
	Create(ctx context.Context, orgID, name, description string) (Project, error)
}

// MemProjectStore is the in-memory tested default. It stores per-org project
// slices and never returns one org's projects to another.
type MemProjectStore struct {
	mu    sync.RWMutex
	byOrg map[string][]Project
	n     int
	Now   func() time.Time
}

// NewMemProjectStore returns an empty in-memory project store.
func NewMemProjectStore() *MemProjectStore {
	return &MemProjectStore{
		byOrg: map[string][]Project{},
		Now:   time.Now,
	}
}

// List returns only the named org's projects (a copy; cross-org never leaks).
func (m *MemProjectStore) List(_ context.Context, orgID string) ([]Project, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	src := m.byOrg[orgID]
	out := make([]Project, len(src))
	copy(out, src)
	return out, nil
}

// Create appends a new project for the org and returns it.
func (m *MemProjectStore) Create(_ context.Context, orgID, name, description string) (Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.n++
	p := Project{
		ID:          fmt.Sprintf("proj_%s_%d", orgID, m.n),
		OrgID:       orgID,
		Name:        name,
		Description: description,
		CreatedAt:   m.Now(),
	}
	m.byOrg[orgID] = append(m.byOrg[orgID], p)
	return p, nil
}
