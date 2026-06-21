package usage

import (
	"context"
	"sort"
	"sync"
	"time"
)

// UsageStore is the pluggable persistence seam for billable usage records,
// mirroring the saas.Store pattern from issue #210. MemUsageStore is the tested
// in-memory default; a durable Postgres store is a documented follow-up
// (docs/saas/usage-pipeline.md) whose natural primary key is
// (org_id, sandbox_id, window_start), the same idempotency key UpsertRecord uses.
//
// The load-bearing contract is on UpsertRecord: writing a record for a key that
// already exists REPLACES it with the supplied (recomputed) value, never adds to
// it. Because Integrate is pure over a window's samples, replaying overlapping or
// duplicate samples recomputes the same record value, so a duplicate or late
// scrape, a node loss, or a controller restart can never double-bill a window.
type UsageStore interface {
	// UpsertRecord writes rec, replacing any existing record for the same
	// (OrgID, SandboxID, Window) key. It is idempotent: upserting the same value
	// twice leaves the store unchanged.
	UpsertRecord(ctx context.Context, rec UsageRecord) error
	// ListRecords returns an org's records whose Window falls in [from, to). A zero
	// from means no lower bound; a zero to means no upper bound. Records are
	// returned sorted by (SandboxID, Window). It only ever returns the named org's
	// records, never another org's, which is the store-level half of the usage
	// API's cross-org isolation.
	ListRecords(ctx context.Context, orgID string, from, to time.Time) ([]UsageRecord, error)
}

// recordKey is the idempotency key for a stored usage record.
type recordKey struct {
	org     string
	sandbox string
	window  time.Time
}

// MemUsageStore is the in-memory UsageStore used as the tested default and by the
// unit suite. It is safe for concurrent use and loses all data on process exit;
// it is the seam the durable store plugs into, not a production store.
type MemUsageStore struct {
	mu      sync.RWMutex
	records map[recordKey]UsageRecord
}

// NewMemUsageStore returns an empty in-memory usage store.
func NewMemUsageStore() *MemUsageStore {
	return &MemUsageStore{records: map[recordKey]UsageRecord{}}
}

// UpsertRecord stores rec, replacing any record with the same key. The replace
// (not add) semantics are what make the pipeline idempotent on (sandbox, window).
func (s *MemUsageStore) UpsertRecord(_ context.Context, rec UsageRecord) error {
	k := recordKey{org: rec.OrgID, sandbox: rec.SandboxID, window: rec.Window.UTC()}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[k] = rec
	return nil
}

// ListRecords returns the org's records in the half-open window [from, to),
// sorted by (SandboxID, Window). It never returns another org's records.
func (s *MemUsageStore) ListRecords(_ context.Context, orgID string, from, to time.Time) ([]UsageRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []UsageRecord{}
	for k, r := range s.records {
		if k.org != orgID {
			continue
		}
		if !from.IsZero() && r.Window.Before(from) {
			continue
		}
		if !to.IsZero() && !r.Window.Before(to) {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SandboxID != out[j].SandboxID {
			return out[i].SandboxID < out[j].SandboxID
		}
		return out[i].Window.Before(out[j].Window)
	})
	return out, nil
}
