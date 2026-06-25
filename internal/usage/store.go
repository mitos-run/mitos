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

// TotalsProvider is the optional interface a UsageStore implements to expose its
// per-org cumulative usage totals. The per-org Prometheus series is driven from
// THIS (the same cumulative number the bill rolls up), not from a single cycle's
// pruned sample buffer, so the metric is monotonic and never drops a known org.
// MemUsageStore implements it; the durable store will too.
type TotalsProvider interface {
	// TotalsByOrg returns each org's cumulative usage Totals: the sum of every
	// settled UsageRecord the store has ever seen for that org, surviving record
	// eviction. It is best-effort across process restarts (the durable store is the
	// billing system of record); within a process it is monotonic per org.
	TotalsByOrg() map[string]Totals
}

// recordKey is the idempotency key for a stored usage record.
type recordKey struct {
	org     string
	sandbox string
	window  time.Time
}

// DefaultRetentionWindows is how many distinct windows of records MemUsageStore
// keeps before evicting the oldest. At the 60s default window this is 24h of
// per-(org, sandbox) records available to the usage API; the per-org cumulative
// Totals survive eviction so the billed total is never lost when a window ages
// out of the in-memory record map.
const DefaultRetentionWindows = 24 * 60

// MemUsageStore is the in-memory UsageStore used as the tested default and by the
// unit suite. It is safe for concurrent use and loses all data on process exit;
// it is the seam the durable store plugs into, not a production store.
//
// MEMORY BOUND (issue #164): the record map is bounded to the most recent
// retentionWindows distinct windows. Records in older windows are evicted on each
// upsert so a long-running controller does not leak one entry per
// (org, sandbox, window) forever. Eviction does NOT corrupt the billed totals: a
// separate per-org cumulative Totals is updated by the DELTA each upsert applies
// to its key, so the cumulative grows monotonically and survives eviction. That
// in-memory cumulative is best-effort across controller restarts; the durable
// store (follow-up) is the billing system of record.
type MemUsageStore struct {
	mu               sync.RWMutex
	records          map[recordKey]UsageRecord
	retentionWindows int

	// cumByOrg is the per-org cumulative usage, the sum of every settled record's
	// billable units across the store's lifetime. It is updated by the delta of
	// each upsert (so a re-scrape that replaces a key with the same value is a
	// no-op, and a corrected value moves the cumulative by exactly the correction),
	// and it survives record eviction. The per-org metric reads this.
	cumByOrg map[string]Totals
}

// NewMemUsageStore returns an empty in-memory usage store with the default
// retention horizon (DefaultRetentionWindows distinct windows).
func NewMemUsageStore() *MemUsageStore {
	return NewMemUsageStoreWithRetention(DefaultRetentionWindows)
}

// NewMemUsageStoreWithRetention returns an empty in-memory usage store that keeps
// the most recent retentionWindows distinct windows of records before evicting
// the oldest. A non-positive retentionWindows falls back to the default. The
// per-org cumulative Totals are unaffected by retention: they survive eviction.
func NewMemUsageStoreWithRetention(retentionWindows int) *MemUsageStore {
	if retentionWindows <= 0 {
		retentionWindows = DefaultRetentionWindows
	}
	return &MemUsageStore{
		records:          map[recordKey]UsageRecord{},
		retentionWindows: retentionWindows,
		cumByOrg:         map[string]Totals{},
	}
}

// UpsertRecord stores rec, replacing any record with the same key. The replace
// (not add) semantics are what make the pipeline idempotent on (sandbox, window).
// It also advances the per-org cumulative Totals by the DELTA between the new and
// the previous value for this key (zero on an identical re-scrape), then evicts
// records in windows older than the retention horizon so the record map stays
// bounded. The cumulative is NOT touched by eviction.
func (s *MemUsageStore) UpsertRecord(_ context.Context, rec UsageRecord) error {
	k := recordKey{org: rec.OrgID, sandbox: rec.SandboxID, window: rec.Window.UTC()}
	s.mu.Lock()
	defer s.mu.Unlock()

	prev, existed := s.records[k]
	s.records[k] = rec

	// Advance the per-org cumulative by the delta this upsert applies to its key.
	// A first write contributes the whole record; a re-scrape that replaces the key
	// with the same value contributes zero (the delta is zero), so the cumulative
	// stays idempotent on (org, sandbox, window) exactly like the record map.
	cum := s.cumByOrg[rec.OrgID]
	cum.VCPUSeconds += rec.VCPUSeconds
	cum.MemGiBSeconds += rec.MemGiBSeconds
	cum.StorageGiBHours += rec.StorageGiBHours
	cum.EgressBytes += rec.EgressBytes
	cum.GPUSeconds += rec.GPUSeconds
	if existed {
		cum.VCPUSeconds -= prev.VCPUSeconds
		cum.MemGiBSeconds -= prev.MemGiBSeconds
		cum.StorageGiBHours -= prev.StorageGiBHours
		cum.EgressBytes -= prev.EgressBytes
		cum.GPUSeconds -= prev.GPUSeconds
	}
	s.cumByOrg[rec.OrgID] = cum

	s.evictOldWindowsLocked()
	return nil
}

// evictOldWindowsLocked drops records whose window is older than the most recent
// retentionWindows distinct windows. It bounds the record map under a long-running
// controller. It is a no-op while the number of distinct windows is within the
// horizon. The caller holds s.mu. The per-org cumulative Totals are intentionally
// NOT adjusted: they are the billed totals and must survive eviction.
func (s *MemUsageStore) evictOldWindowsLocked() {
	// Collect the distinct windows present. Cheap relative to a scrape cadence; the
	// map only holds a bounded number of windows in steady state.
	seen := map[time.Time]struct{}{}
	for k := range s.records {
		seen[k.window] = struct{}{}
	}
	if len(seen) <= s.retentionWindows {
		return
	}
	windows := make([]time.Time, 0, len(seen))
	for w := range seen {
		windows = append(windows, w)
	}
	sort.Slice(windows, func(i, j int) bool { return windows[i].Before(windows[j]) })
	cutoff := windows[len(windows)-s.retentionWindows]
	for k := range s.records {
		if k.window.Before(cutoff) {
			delete(s.records, k)
		}
	}
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

// TotalsByOrg returns a snapshot of each org's cumulative usage Totals. It is the
// monotonic, eviction-surviving number the per-org metric reads. An org with no
// usage is absent from the map. The returned map is a copy the caller may keep.
func (s *MemUsageStore) TotalsByOrg() map[string]Totals {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]Totals, len(s.cumByOrg))
	for org, t := range s.cumByOrg {
		out[org] = t
	}
	return out
}
