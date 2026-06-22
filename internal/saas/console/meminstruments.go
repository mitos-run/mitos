package console

import (
	"context"
	"sync"
)

// MemInstruments is the in-memory InstrumentsSource tested default and the seam
// the real #211/#33 aggregator plugs into. It holds one measured snapshot per
// org; Snapshot returns only the named org's metrics (a zero snapshot for an org
// with no measurements yet). Safe for concurrent use.
type MemInstruments struct {
	mu    sync.RWMutex
	byOrg map[string]Instruments
}

// NewMemInstruments returns an empty in-memory instruments source.
func NewMemInstruments() *MemInstruments {
	return &MemInstruments{byOrg: map[string]Instruments{}}
}

// Set seeds or replaces an org's measured snapshot (test/wiring helper).
func (m *MemInstruments) Set(in Instruments) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byOrg[in.OrgID] = in
}

// Snapshot returns the org's measured snapshot, or a zero snapshot scoped to the
// org if none has been recorded yet. It never returns another org's metrics.
func (m *MemInstruments) Snapshot(_ context.Context, orgID string) (Instruments, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	in, ok := m.byOrg[orgID]
	if !ok {
		return Instruments{OrgID: orgID}, nil
	}
	return in, nil
}
