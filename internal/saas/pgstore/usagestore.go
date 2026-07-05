package pgstore

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"mitos.run/mitos/internal/usage"
)

// PgUsageStore is the durable Postgres implementation of usage.UsageStore. It is
// the production persistence for billable per-org usage records (issue #211): the
// controller's collector upserts each (org, sandbox, window) record into it, and
// the org-scoped usage API serves reads of it, so metered consumption survives a
// controller restart instead of living only in the in-memory MemUsageStore.
//
// The in-memory usage.MemUsageStore is the behavioral reference; this store
// passes the same usagestoretest contract (idempotent upsert, per-org isolation,
// half-open period bounds, per-org cumulative totals).
//
// All queries are parameterized and org-scoped: ListRecords and TotalsByOrg only
// ever read the named org's rows, so org A can never see org B's usage. No value
// is ever interpolated into SQL.
type PgUsageStore struct {
	pool *pgxpool.Pool
}

// NewPgUsageStore returns a PgUsageStore backed by pool. The schema (the
// usage_records table) is created by the pgstore migrations Open runs; callers
// share the pool an Open-ed PgStore exposes via Pool.
func NewPgUsageStore(pool *pgxpool.Pool) *PgUsageStore { return &PgUsageStore{pool: pool} }

// compile-time assertions that PgUsageStore satisfies the usage contracts.
var (
	_ usage.UsageStore     = (*PgUsageStore)(nil)
	_ usage.TotalsProvider = (*PgUsageStore)(nil)
)

// UpsertRecord writes rec, REPLACING any existing record for the same
// (OrgID, SandboxID, Window) key. The ON CONFLICT DO UPDATE makes it idempotent:
// because usage.Integrate is pure over a window's samples, replaying overlapping
// or duplicate scrapes recomputes the same value, so a re-upsert is a no-op and a
// node loss or controller restart can never double-bill a window. The window is
// stored in UTC so the key is stable regardless of the caller's location.
func (s *PgUsageStore) UpsertRecord(ctx context.Context, rec usage.UsageRecord) error {
	const q = `
        INSERT INTO usage_records
            (org_id, sandbox_id, window_start, vcpu_seconds, mem_gib_seconds, storage_gib_hours, egress_bytes, gpu_seconds, region)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
        ON CONFLICT (org_id, sandbox_id, window_start) DO UPDATE SET
            vcpu_seconds      = EXCLUDED.vcpu_seconds,
            mem_gib_seconds   = EXCLUDED.mem_gib_seconds,
            storage_gib_hours = EXCLUDED.storage_gib_hours,
            egress_bytes      = EXCLUDED.egress_bytes,
            gpu_seconds       = EXCLUDED.gpu_seconds,
            region            = EXCLUDED.region`
	_, err := s.pool.Exec(ctx, q,
		rec.OrgID, rec.SandboxID, rec.Window.UTC(),
		rec.VCPUSeconds, rec.MemGiBSeconds, rec.StorageGiBHours, rec.EgressBytes, rec.GPUSeconds, rec.Region)
	if err != nil {
		return fmt.Errorf("upsert usage record: %w", err)
	}
	return nil
}

// ListRecords returns the org's records whose Window falls in the half-open
// interval [from, to), sorted by (SandboxID, Window). A zero from means no lower
// bound; a zero to means no upper bound. It only ever returns the named org's
// records, never another org's, which is the store-level half of the usage API's
// cross-org isolation. The returned slice is non-nil even when empty.
func (s *PgUsageStore) ListRecords(ctx context.Context, orgID string, from, to time.Time) ([]usage.UsageRecord, error) {
	const q = `
        SELECT org_id, sandbox_id, window_start, vcpu_seconds, mem_gib_seconds, storage_gib_hours, egress_bytes, gpu_seconds, region
        FROM usage_records
        WHERE org_id = $1
          AND ($2::timestamptz IS NULL OR window_start >= $2)
          AND ($3::timestamptz IS NULL OR window_start <  $3)
        ORDER BY sandbox_id, window_start`
	rows, err := s.pool.Query(ctx, q, orgID, nullableTime(from), nullableTime(to))
	if err != nil {
		return nil, fmt.Errorf("list usage records: %w", err)
	}
	defer rows.Close()

	out := []usage.UsageRecord{}
	for rows.Next() {
		var r usage.UsageRecord
		if err := rows.Scan(
			&r.OrgID, &r.SandboxID, &r.Window,
			&r.VCPUSeconds, &r.MemGiBSeconds, &r.StorageGiBHours, &r.EgressBytes, &r.GPUSeconds, &r.Region,
		); err != nil {
			return nil, fmt.Errorf("scan usage record: %w", err)
		}
		r.Window = r.Window.UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate usage records: %w", err)
	}
	return out, nil
}

// TotalsByOrg returns each org's cumulative usage Totals: the sum of every stored
// record for that org. Unlike the in-memory store (whose cumulative is a
// delta-tracked figure that survives in-memory eviction), the durable store keeps
// every record, so the totals are a direct aggregate over the table and are the
// true billing system of record. An org with no records is absent from the map.
//
// TotalsByOrg has no period bound by design: it is the lifetime cumulative the
// per-org Prometheus series reads, mirroring usage.MemUsageStore.TotalsByOrg.
func (s *PgUsageStore) TotalsByOrg() map[string]usage.Totals {
	const q = `
        SELECT org_id,
               COALESCE(SUM(vcpu_seconds), 0),
               COALESCE(SUM(mem_gib_seconds), 0),
               COALESCE(SUM(storage_gib_hours), 0),
               COALESCE(SUM(egress_bytes), 0),
               COALESCE(SUM(gpu_seconds), 0)
        FROM usage_records
        GROUP BY org_id`
	out := map[string]usage.Totals{}
	rows, err := s.pool.Query(context.Background(), q)
	if err != nil {
		// TotalsByOrg has no error return (it backs a best-effort metric); a query
		// failure yields an empty snapshot rather than a panic.
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var org string
		var t usage.Totals
		if err := rows.Scan(&org, &t.VCPUSeconds, &t.MemGiBSeconds, &t.StorageGiBHours, &t.EgressBytes, &t.GPUSeconds); err != nil {
			return map[string]usage.Totals{}
		}
		out[org] = t
	}
	if rows.Err() != nil {
		return map[string]usage.Totals{}
	}
	return out
}

// nullableTime maps the zero time to a SQL NULL so the "no bound" cases in
// ListRecords select the IS NULL branch, and a real time to itself in UTC.
func nullableTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}
