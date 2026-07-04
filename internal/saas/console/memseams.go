package console

import (
	"context"
	"errors"
	"sort"
	"sync"
)

// ErrNotFound is returned by the seams when a requested record does not exist OR
// belongs to a different org than the caller. The two cases are deliberately
// indistinguishable so a caller cannot probe another org's id space.
var ErrNotFound = errors.New("console: record not found")

// MemSandboxControl is the in-memory SandboxControl used as the tested default
// and by the unit suite. It is the seam the real control-plane query plugs into;
// every method scopes its effect to the supplied org so the cross-org isolation
// property holds at the seam, not just the handler. Safe for concurrent use.
type MemSandboxControl struct {
	mu   sync.RWMutex
	byID map[string]SandboxView
}

// NewMemSandboxControl returns an empty in-memory sandbox control.
func NewMemSandboxControl() *MemSandboxControl {
	return &MemSandboxControl{byID: map[string]SandboxView{}}
}

// Add seeds a sandbox (test/wiring helper). The sandbox carries its own OrgID,
// which is the only org that can ever see or terminate it.
func (m *MemSandboxControl) Add(s SandboxView) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byID[s.ID] = s
}

// List returns the org's sandboxes, sorted by id for a stable listing.
func (m *MemSandboxControl) List(_ context.Context, orgID string) ([]SandboxView, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []SandboxView{}
	for _, s := range m.byID {
		if s.OrgID == orgID {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Get returns the org's sandbox by id. A sandbox owned by a different org is
// reported as ErrNotFound, indistinguishable from a missing one.
func (m *MemSandboxControl) Get(_ context.Context, orgID, sandboxID string) (SandboxView, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.byID[sandboxID]
	if !ok || s.OrgID != orgID {
		return SandboxView{}, ErrNotFound
	}
	return s, nil
}

// Terminate removes the org's sandbox by id. A sandbox owned by a different org
// is reported as ErrNotFound and is NOT terminated.
func (m *MemSandboxControl) Terminate(_ context.Context, orgID, sandboxID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.byID[sandboxID]
	if !ok || s.OrgID != orgID {
		return ErrNotFound
	}
	delete(m.byID, sandboxID)
	return nil
}

// MemTemplateLister is the in-memory TemplateLister tested default.
type MemTemplateLister struct {
	mu  sync.RWMutex
	all []TemplateView
}

// NewMemTemplateLister returns an empty in-memory template lister.
func NewMemTemplateLister() *MemTemplateLister {
	return &MemTemplateLister{}
}

// Add seeds a template (test/wiring helper).
func (m *MemTemplateLister) Add(t TemplateView) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.all = append(m.all, t)
}

// List returns only the org's templates, sorted by name.
func (m *MemTemplateLister) List(_ context.Context, orgID string) ([]TemplateView, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []TemplateView{}
	for _, t := range m.all {
		if t.OrgID == orgID {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// maxAuditEventsPerOrg bounds MemAuditLog's per-org history so a long-running
// dev/self-host process without Postgres configured cannot grow this map
// without limit: once an org crosses the cap, Record drops the oldest event to
// make room for the new one. Postgres deployments have no such cap (retention
// there is the operator's own pruning policy, issue #163); this is purely a
// memory-safety backstop for the in-memory fallback.
const maxAuditEventsPerOrg = 10000

// MemAuditLog is the in-memory AuditRecorder tested default. It is append-only
// (up to maxAuditEventsPerOrg, after which the oldest event is dropped) and
// org-scoped; List returns a copy in reverse-chronological order. Safe for
// concurrent use.
type MemAuditLog struct {
	mu    sync.Mutex
	byOrg map[string][]AuditEvent
}

// NewMemAuditLog returns an empty in-memory audit log.
func NewMemAuditLog() *MemAuditLog {
	return &MemAuditLog{byOrg: map[string][]AuditEvent{}}
}

// Record appends an event to its org's log. The event carries no secret. If
// the org's log is at maxAuditEventsPerOrg, the oldest event is dropped first
// so memory never grows past the cap.
func (m *MemAuditLog) Record(_ context.Context, ev AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	events := append(m.byOrg[ev.OrgID], ev)
	if len(events) > maxAuditEventsPerOrg {
		events = events[len(events)-maxAuditEventsPerOrg:]
	}
	m.byOrg[ev.OrgID] = events
	return nil
}

// List returns up to limit of the org's events, most recent first. limit <= 0
// defaults to DefaultAuditListLimit. It never returns another org's events.
func (m *MemAuditLog) List(_ context.Context, orgID string, limit int) ([]AuditEvent, error) {
	if limit <= 0 {
		limit = DefaultAuditListLimit
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	src := m.byOrg[orgID]
	n := len(src)
	if n > limit {
		n = limit
	}
	out := make([]AuditEvent, n)
	for i := 0; i < n; i++ {
		out[i] = src[len(src)-1-i]
	}
	return out, nil
}
