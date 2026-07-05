package pgstore

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"mitos.run/mitos/internal/saas/console"
)

// PgAuditLog is the durable, append-only org audit trail (issue #616). It
// implements console.AuditRecorder over the audit_log table so the trail the
// console audit view and NDJSON export read survives a console restart. Rows
// are never updated; the only permitted delete is retention pruning by age.
// The console stores the per-org retention policy (days) but does not enforce
// it today for the in-memory store either: the enforcement sweep is the
// controller GC follow-up (issue #163), so this store deliberately ships
// durability only and adds no pruning loop of its own.
type PgAuditLog struct {
	pool *pgxpool.Pool
}

// NewPgAuditLog returns a PgAuditLog backed by pool.
func NewPgAuditLog(pool *pgxpool.Pool) *PgAuditLog { return &PgAuditLog{pool: pool} }

// compile-time assertion that PgAuditLog satisfies the AuditRecorder contract.
var _ console.AuditRecorder = (*PgAuditLog)(nil)

// Record appends one audit event. The event carries no secret (the
// AuditRecorder contract): Detail is a non-secret, human-legible summary.
// actor_name, actor_type, target_type, and target_name were added by migration
// 0013 alongside the original org_id/actor/action/target/detail/created_at
// columns from migration 0009; all four default to an empty string at the
// database level so rows written before this field existed still read back
// as the zero value.
func (l *PgAuditLog) Record(ctx context.Context, ev console.AuditEvent) error {
	const q = `
        INSERT INTO audit_log (org_id, actor, actor_name, actor_type, action, target, target_type, target_name, detail, created_at)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`
	if _, err := l.pool.Exec(ctx, q,
		ev.OrgID, ev.ActorID, ev.ActorName, ev.ActorType,
		ev.Action, ev.Target, ev.TargetType, ev.TargetName,
		ev.Detail, ev.At,
	); err != nil {
		return fmt.Errorf("record audit event: %w", err)
	}
	return nil
}

// List returns up to limit of the org's events, most recent first; id breaks a
// created_at tie so same-instant events keep reverse insertion order, matching
// MemAuditLog. limit <= 0 defaults to console.DefaultAuditListLimit. It only
// ever returns the named org's events and always returns a non-nil slice.
// Timestamps are normalized to UTC so the round trip is deterministic and
// store-equivalent (see timeVal).
func (l *PgAuditLog) List(ctx context.Context, orgID string, limit int) ([]console.AuditEvent, error) {
	if limit <= 0 {
		limit = console.DefaultAuditListLimit
	}
	const q = `
        SELECT org_id, actor, actor_name, actor_type, action, target, target_type, target_name, detail, created_at
        FROM audit_log WHERE org_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2`
	rows, err := l.pool.Query(ctx, q, orgID, limit)
	if err != nil {
		return nil, fmt.Errorf("list audit events: %w", err)
	}
	defer rows.Close()
	out := []console.AuditEvent{}
	for rows.Next() {
		var ev console.AuditEvent
		if err := rows.Scan(
			&ev.OrgID, &ev.ActorID, &ev.ActorName, &ev.ActorType,
			&ev.Action, &ev.Target, &ev.TargetType, &ev.TargetName,
			&ev.Detail, &ev.At,
		); err != nil {
			return nil, fmt.Errorf("scan audit event: %w", err)
		}
		ev.At = ev.At.UTC()
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate audit events: %w", err)
	}
	return out, nil
}
