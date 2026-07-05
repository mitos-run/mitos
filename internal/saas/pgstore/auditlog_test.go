package pgstore_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"mitos.run/mitos/internal/saas/console"
	"mitos.run/mitos/internal/saas/pgstore"
)

// TestPgAuditLog covers the console.AuditRecorder contract against the durable
// store: append + org-scoped reverse-chronological list (including the new
// actor/target name and type fields), cross-org isolation (org A never sees
// org B rows), and restart survival (a second pgstore.Open over the same
// database still returns the trail).
func TestPgAuditLog(t *testing.T) {
	dsn := testDSN(t)
	pg, err := pgstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	truncateTables(t, dsn, "audit_log")
	l := pgstore.NewPgAuditLog(pg.Pool())
	ctx := context.Background()
	base := time.Unix(1700000000, 0).UTC()

	events := []console.AuditEvent{
		{
			OrgID: "o1", ActorID: "acct-a", ActorName: "Alice", ActorType: "user",
			Action: "key.create", Target: "k1", TargetType: "key", TargetName: "ci-key",
			Detail: "created key k1", At: base,
		},
		{
			OrgID: "o1", ActorID: "acct-b", ActorName: "Bob", ActorType: "user",
			Action: "key.revoke", Target: "k1", TargetType: "key", TargetName: "ci-key",
			Detail: "revoked key k1", At: base.Add(time.Minute),
		},
		{
			OrgID: "o2", ActorID: "acct-z", ActorName: "Zed", ActorType: "user",
			Action: "key.create", Target: "k9", TargetType: "key", TargetName: "prod-key",
			Detail: "created key k9", At: base.Add(30 * time.Second),
		},
	}
	for _, ev := range events {
		if err := l.Record(ctx, ev); err != nil {
			t.Fatalf("record %s: %v", ev.Action, err)
		}
	}

	// Org-scoped list, most recent first; org o2's row never appears.
	got, err := l.List(ctx, "o1", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []console.AuditEvent{events[1], events[0]}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("list o1 = %+v, want %+v", got, want)
	}

	// Cross-org isolation the other way: o2 sees only its own row.
	got2, err := l.List(ctx, "o2", 0)
	if err != nil {
		t.Fatalf("list o2: %v", err)
	}
	if !reflect.DeepEqual(got2, []console.AuditEvent{events[2]}) {
		t.Fatalf("list o2 = %+v, want only o2's event", got2)
	}

	// An org with no events gets an empty, non-nil slice (matching MemAuditLog).
	empty, err := l.List(ctx, "no-such-org", 0)
	if err != nil {
		t.Fatalf("list empty org: %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("list unknown org = %#v, want empty non-nil slice", empty)
	}

	// A limit smaller than the org's history truncates to the most recent N.
	limited, err := l.List(ctx, "o1", 1)
	if err != nil {
		t.Fatalf("list o1 limit 1: %v", err)
	}
	if !reflect.DeepEqual(limited, []console.AuditEvent{events[1]}) {
		t.Fatalf("list o1 limit 1 = %+v, want only the most recent event", limited)
	}

	// Restart survival: close the first store, open a second one over the same
	// database, and the trail (including the actor/target fields) is still
	// there. This is the whole point of #616.
	pg.Close()
	pg2, err := pgstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(pg2.Close)
	l2 := pgstore.NewPgAuditLog(pg2.Pool())
	after, err := l2.List(ctx, "o1", 0)
	if err != nil {
		t.Fatalf("list after restart: %v", err)
	}
	if !reflect.DeepEqual(after, want) {
		t.Fatalf("list after restart = %+v, want %+v", after, want)
	}
}

// TestPgAuditLogSameInstantOrder pins the tie-break: two events recorded at the
// SAME timestamp come back most-recently-inserted first, matching the reverse
// insertion order MemAuditLog returns.
func TestPgAuditLogSameInstantOrder(t *testing.T) {
	dsn := testDSN(t)
	pg, err := pgstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pg.Close)
	truncateTables(t, dsn, "audit_log")
	l := pgstore.NewPgAuditLog(pg.Pool())
	ctx := context.Background()
	at := time.Unix(1700000000, 0).UTC()

	first := console.AuditEvent{OrgID: "o1", ActorID: "a", Action: "member.add", Target: "m1", At: at}
	second := console.AuditEvent{OrgID: "o1", ActorID: "a", Action: "member.remove", Target: "m1", At: at}
	if err := l.Record(ctx, first); err != nil {
		t.Fatalf("record first: %v", err)
	}
	if err := l.Record(ctx, second); err != nil {
		t.Fatalf("record second: %v", err)
	}

	got, err := l.List(ctx, "o1", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !reflect.DeepEqual(got, []console.AuditEvent{second, first}) {
		t.Fatalf("same-instant order = %+v, want reverse insertion order", got)
	}
}

// TestMigration0009AuditLogTable proves migration 0009 lands the table and its
// org/time index after a full migration run.
func TestMigration0009AuditLogTable(t *testing.T) {
	dsn := testDSN(t)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	openMigrated(t, dsn)

	var exists bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'audit_log')`).Scan(&exists); err != nil {
		t.Fatalf("check audit_log table: %v", err)
	}
	if !exists {
		t.Fatal("table audit_log missing after migration 0009")
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'audit_log_org_created_at')`).Scan(&exists); err != nil {
		t.Fatalf("check audit_log index: %v", err)
	}
	if !exists {
		t.Fatal("index audit_log_org_created_at missing after migration 0009")
	}
}

// TestMigration0013AuditLogActorTargetColumns proves migration 0013 adds the
// actor_name, actor_type, target_type, and target_name columns to the
// pre-existing audit_log table (rather than a new table), so a hosted
// deployment's live audit history is extended in place, not orphaned.
func TestMigration0013AuditLogActorTargetColumns(t *testing.T) {
	dsn := testDSN(t)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	openMigrated(t, dsn)

	for _, col := range []string{"actor_name", "actor_type", "target_type", "target_name"} {
		var exists bool
		if err := pool.QueryRow(context.Background(),
			`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'audit_log' AND column_name = $1)`,
			col,
		).Scan(&exists); err != nil {
			t.Fatalf("check audit_log.%s: %v", col, err)
		}
		if !exists {
			t.Errorf("column audit_log.%s missing after migration 0013", col)
		}
	}
}
