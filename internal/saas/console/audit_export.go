package console

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"

	"mitos.run/mitos/internal/apierr"
	"mitos.run/mitos/internal/saas"
)

// RetentionStore is the per-org audit-retention policy seam. Get returns the
// configured retention window in days (0 means no policy is set). Set stores a
// new policy. The GC sweep that enforces the policy is the controller's
// responsibility (issue #163); the console stores and exposes the policy only.
//
// Both methods MUST be scoped to the supplied orgID: a Set for orgA must never
// affect orgB's policy, and a Get for orgA must never return orgB's value.
type RetentionStore interface {
	// Get returns the number of days of audit events to retain for the org.
	// Returns 0 if no policy has been set.
	Get(ctx context.Context, orgID string) (int, error)
	// Set stores the retention policy for the org (in days).
	Set(ctx context.Context, orgID string, days int) error
}

// MemRetentionStore is the in-memory tested default for RetentionStore. It is
// safe for concurrent use and never leaks one org's policy to another.
type MemRetentionStore struct {
	mu    sync.RWMutex
	byOrg map[string]int
}

// NewMemRetentionStore returns an empty in-memory retention store.
func NewMemRetentionStore() *MemRetentionStore {
	return &MemRetentionStore{byOrg: map[string]int{}}
}

// Get returns the org's retention days. Returns 0 if no policy is set.
func (m *MemRetentionStore) Get(_ context.Context, orgID string) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.byOrg[orgID], nil
}

// Set stores the org's retention days.
func (m *MemRetentionStore) Set(_ context.Context, orgID string, days int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byOrg[orgID] = days
	return nil
}

// handleAuditExport writes the caller org's audit events as NDJSON (one JSON
// AuditEvent per line). The Content-Disposition header marks the response as a
// file download. Only the caller org's events are ever written; orgB events
// never appear in orgA's export.
func (c *Console) handleAuditExport(w http.ResponseWriter, r *http.Request) {
	_, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	// The export reads up to MaxAuditListLimit (matching MemAuditLog's own
	// per-org retention cap) rather than DefaultAuditListLimit: it is a full
	// export, not a paginated view, so it should read everything the store is
	// guaranteed to still hold.
	events, err := c.deps.Audit.List(r.Context(), orgID, MaxAuditListLimit)
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).
			WithCause("the audit log could not be read"))
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", `attachment; filename="audit.ndjson"`)
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	for _, ev := range events {
		_ = enc.Encode(ev)
	}
}

// retentionBody is the JSON shape for GET and PUT /console/audit/retention.
type retentionBody struct {
	Days int `json:"days"`
}

// handleGetRetention returns the caller org's current audit retention policy in
// days. Returns {"days":0} if no policy has been set.
func (c *Console) handleGetRetention(w http.ResponseWriter, r *http.Request) {
	_, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	days, err := c.deps.Retention.Get(r.Context(), orgID)
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).
			WithCause("the retention policy could not be read"))
		return
	}
	writeJSON(w, http.StatusOK, retentionBody{Days: days})
}

// handleSetRetention stores the caller org's audit retention policy. The body
// must be {"days": N}. The GC sweep that enforces the policy runs in the
// controller (issue #163); this endpoint stores and exposes the policy only.
func (c *Console) handleSetRetention(w http.ResponseWriter, r *http.Request) {
	_, orgID, e, ok := c.authorize(r, saas.PermManageSettings)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	var body retentionBody
	if err := decodeBody(r, &body); err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidJSON).
			WithCause("the retention body is not valid JSON"))
		return
	}
	if err := c.deps.Retention.Set(r.Context(), orgID, body.Days); err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).
			WithCause("the retention policy could not be stored"))
		return
	}
	writeJSON(w, http.StatusOK, retentionBody{Days: body.Days})
}
