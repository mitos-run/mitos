package console

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// ErrNotFound is returned by the seams when a requested record does not exist OR
// belongs to a different org than the caller. The two cases are deliberately
// indistinguishable so a caller cannot probe another org's id space.
var ErrNotFound = errors.New("console: record not found")

// ErrUnsupported is returned by a seam whose real backend does not exist on
// this deployment yet (a documented follow-up), distinct from ErrNotFound: the
// operation is understood but genuinely cannot be carried out here. The
// console maps it to HTTP 501 so the SPA shows an honest "not available yet"
// state instead of a silent no-op or a fabricated success.
var ErrUnsupported = errors.New("console: operation not supported by this deployment")

// MemSandboxControl is the in-memory SandboxControl used as the tested default
// and by the unit suite. It is the seam the real control-plane query plugs into;
// every method scopes its effect to the supplied org so the cross-org isolation
// property holds at the seam, not just the handler. Safe for concurrent use.
type MemSandboxControl struct {
	mu   sync.RWMutex
	byID map[string]SandboxView
	seq  int

	// execResults / execErrs let a test script a canned Exec outcome per
	// sandbox id via SetExecResult/SetExecErr; an unscripted sandbox returns a
	// zero-value ExecResult (exit 0, no output), which is enough for the
	// handler-level tests (they assert plumbing, not command semantics).
	execResults map[string]ExecResult
	execErrs    map[string]error
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

// Create provisions a new sandbox for org from req.Template, assigning it a
// unique id and recording the requested VCPUs/MemGiB on its view (the fake has
// full fidelity here; a real adapter may not be able to enforce the sizing,
// see clustersandbox.Control.Create).
func (m *MemSandboxControl) Create(_ context.Context, orgID string, req CreateSandboxRequest) (SandboxView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	sb := SandboxView{
		ID:        fmt.Sprintf("sbx-%d", m.seq),
		OrgID:     orgID,
		Template:  req.Template,
		Phase:     "Pending",
		VCPUs:     req.VCPUs,
		MemBytes:  int64(req.MemGiB) << 30,
		CreatedAt: time.Now(),
	}
	m.byID[sb.ID] = sb
	return sb, nil
}

// Fork creates count new sandboxes forked from sandboxID and returns their
// ids in creation order. sandboxID must belong to org (ErrNotFound otherwise);
// each child inherits the source's template and sizing, mirroring what a real
// fork carries forward from its source snapshot.
func (m *MemSandboxControl) Fork(_ context.Context, orgID, sandboxID string, count int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	src, ok := m.byID[sandboxID]
	if !ok || src.OrgID != orgID {
		return nil, ErrNotFound
	}
	ids := make([]string, 0, count)
	for i := 0; i < count; i++ {
		m.seq++
		id := fmt.Sprintf("%s-fork-%d", sandboxID, m.seq)
		m.byID[id] = SandboxView{
			ID:        id,
			OrgID:     orgID,
			Template:  src.Template,
			Phase:     "Pending",
			VCPUs:     src.VCPUs,
			MemBytes:  src.MemBytes,
			CreatedAt: time.Now(),
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// SetExecResult scripts the ExecResult MemSandboxControl.Exec returns for
// sandboxID (test/wiring helper).
func (m *MemSandboxControl) SetExecResult(sandboxID string, res ExecResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.execResults == nil {
		m.execResults = map[string]ExecResult{}
	}
	m.execResults[sandboxID] = res
}

// SetExecErr scripts the error MemSandboxControl.Exec returns for sandboxID,
// e.g. ErrUnsupported (test/wiring helper).
func (m *MemSandboxControl) SetExecErr(sandboxID string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.execErrs == nil {
		m.execErrs = map[string]error{}
	}
	m.execErrs[sandboxID] = err
}

// Exec runs cmd in the org's sandbox. sandboxID must belong to org (ErrNotFound
// otherwise); the result is whatever was scripted via SetExecResult/SetExecErr,
// defaulting to a zero-value success (exit 0, no output) when unscripted.
func (m *MemSandboxControl) Exec(_ context.Context, orgID, sandboxID, _ string, _ int) (ExecResult, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sb, ok := m.byID[sandboxID]
	if !ok || sb.OrgID != orgID {
		return ExecResult{}, ErrNotFound
	}
	if err, ok := m.execErrs[sandboxID]; ok {
		return ExecResult{}, err
	}
	return m.execResults[sandboxID], nil
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
