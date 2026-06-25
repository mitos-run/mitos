package console

import (
	"context"
	"net/http"
	"sync"

	"mitos.run/mitos/internal/apierr"
)

// DataRetentionPolicy is the per-org data retention configuration. Zero values
// mean no limit is set (keep forever). LegalHold pauses all automated deletion
// for the org regardless of the individual day limits.
//
// The GC sweep that enforces these limits runs in the controller (issue #163).
// A legal hold pauses deletion: no sandbox metadata, logs, or usage data is
// deleted while LegalHold is true, even if the day limits have elapsed.
type DataRetentionPolicy struct {
	SandboxMetadataDays int  `json:"sandbox_metadata_days"`
	LogsDays            int  `json:"logs_days"`
	UsageDays           int  `json:"usage_days"`
	LegalHold           bool `json:"legal_hold"`
}

// DataRetentionStore is the per-org data-retention policy seam. Get returns the
// current policy for an org (zero value = keep forever). Set stores a new
// policy. Both methods MUST be scoped to the supplied orgID: a Set for orgA
// must never affect orgB's policy, and a Get for orgA must never return orgB's
// value.
type DataRetentionStore interface {
	Get(ctx context.Context, orgID string) (DataRetentionPolicy, error)
	Set(ctx context.Context, orgID string, p DataRetentionPolicy) error
}

// MemDataRetentionStore is the in-memory tested default for DataRetentionStore.
// It is safe for concurrent use and never leaks one org's policy to another.
// An org with no stored policy returns the zero value (keep forever).
type MemDataRetentionStore struct {
	mu    sync.RWMutex
	byOrg map[string]DataRetentionPolicy
}

// NewMemDataRetentionStore returns an empty in-memory data-retention store.
func NewMemDataRetentionStore() *MemDataRetentionStore {
	return &MemDataRetentionStore{byOrg: map[string]DataRetentionPolicy{}}
}

// Get returns the org's data retention policy. Returns the zero policy (keep
// forever) if no policy has been set.
func (m *MemDataRetentionStore) Get(_ context.Context, orgID string) (DataRetentionPolicy, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.byOrg[orgID], nil
}

// Set stores the org's data retention policy.
func (m *MemDataRetentionStore) Set(_ context.Context, orgID string, p DataRetentionPolicy) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byOrg[orgID] = p
	return nil
}

// handleGetDataRetention returns the caller org's current data retention policy.
// Returns the zero policy (keep forever) if no policy has been set.
func (c *Console) handleGetDataRetention(w http.ResponseWriter, r *http.Request) {
	_, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	p, err := c.deps.DataRetention.Get(r.Context(), orgID)
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).
			WithCause("the data retention policy could not be read"))
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// handleSetDataRetention stores the caller org's data retention policy. The
// body must be a DataRetentionPolicy object. The GC sweep that enforces the
// policy runs in the controller (issue #163); this endpoint stores and exposes
// the policy only. A legal hold pauses all automated deletion for the org.
func (c *Console) handleSetDataRetention(w http.ResponseWriter, r *http.Request) {
	_, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	var body DataRetentionPolicy
	if err := decodeBody(r, &body); err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidJSON).
			WithCause("the retention body is not valid JSON"))
		return
	}
	if err := c.deps.DataRetention.Set(r.Context(), orgID, body); err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).
			WithCause("the data retention policy could not be stored"))
		return
	}
	writeJSON(w, http.StatusOK, body)
}
