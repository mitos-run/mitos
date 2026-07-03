package pgstore_test

import (
	"context"
	"testing"
	"time"

	"mitos.run/mitos/internal/saas/pgstore"
	"mitos.run/mitos/internal/saas/quota"
)

// TestPgSuspensionStore proves the durable kill-switch store matches the
// MemSuspensionStore contract: suspend/read/lift round trip, re-suspend keeps
// the FIRST suspension time while updating reason/note/hold, and lift reports
// whether the org had been suspended. Requires MITOS_TEST_DATABASE_DSN (skips
// otherwise); Open runs the 0008_suspensions.sql migration.
func TestPgSuspensionStore(t *testing.T) {
	dsn := testDSN(t)
	pg, err := pgstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pg.Close)
	truncateTables(t, dsn, "suspensions")
	s := pgstore.NewPgSuspensionStore(pg.Pool())
	ctx := context.Background()

	// A never-suspended org reads as not suspended, no error.
	if _, ok, err := s.IsSuspended(ctx, "org-clean"); err != nil || ok {
		t.Fatalf("IsSuspended missing = ok %v, err %v; want false, nil", ok, err)
	}

	// Suspend then read back the full record.
	at := time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)
	want := quota.Suspension{OrgID: "org-a", Reason: quota.ReasonAbuseSignal, Note: "egress spike", At: at, ManualHold: true}
	if err := s.Suspend(ctx, want); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	got, ok, err := s.IsSuspended(ctx, "org-a")
	if err != nil || !ok {
		t.Fatalf("IsSuspended after Suspend = ok %v, err %v; want true, nil", ok, err)
	}
	if !got.At.Equal(want.At) {
		t.Fatalf("At = %v, want %v", got.At, want.At)
	}
	got.At = want.At // normalize for the struct compare below.
	if got != want {
		t.Fatalf("IsSuspended = %+v, want %+v", got, want)
	}

	// Re-suspend: reason/note/hold update, the FIRST suspension time is kept
	// (MemSuspensionStore semantics).
	later := at.Add(time.Hour)
	if err := s.Suspend(ctx, quota.Suspension{OrgID: "org-a", Reason: quota.ReasonDunning, Note: "retries exhausted", At: later, ManualHold: false}); err != nil {
		t.Fatalf("re-Suspend: %v", err)
	}
	got2, ok2, err := s.IsSuspended(ctx, "org-a")
	if err != nil || !ok2 {
		t.Fatalf("IsSuspended after re-Suspend = ok %v, err %v; want true, nil", ok2, err)
	}
	if got2.Reason != quota.ReasonDunning || got2.Note != "retries exhausted" || got2.ManualHold {
		t.Fatalf("re-Suspend record = %+v, want updated reason/note/hold", got2)
	}
	if !got2.At.Equal(at) {
		t.Fatalf("re-Suspend At = %v, want the FIRST suspension time %v", got2.At, at)
	}

	// Suspension isolation: another org is unaffected.
	if _, ok, err := s.IsSuspended(ctx, "org-b"); err != nil || ok {
		t.Fatalf("IsSuspended other org = ok %v, err %v; want false, nil", ok, err)
	}

	// Lift reports true for a suspended org, and the org then reads clean.
	if lifted, err := s.Lift(ctx, "org-a"); err != nil || !lifted {
		t.Fatalf("Lift = %v, %v; want true, nil", lifted, err)
	}
	if _, ok, err := s.IsSuspended(ctx, "org-a"); err != nil || ok {
		t.Fatalf("IsSuspended after Lift = ok %v, err %v; want false, nil", ok, err)
	}

	// Lift on a not-suspended org reports false, no error (idempotent).
	if lifted, err := s.Lift(ctx, "org-a"); err != nil || lifted {
		t.Fatalf("second Lift = %v, %v; want false, nil", lifted, err)
	}
}

// TestPgSuspensionStoreSurvivesReopen proves durability: a suspension written
// through one store handle (one gateway replica / one process lifetime) is read
// by a fresh handle over a new pool, which is exactly the restart and
// cross-replica property issue #615 requires.
func TestPgSuspensionStoreSurvivesReopen(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()

	pg1, err := pgstore.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open first: %v", err)
	}
	truncateTables(t, dsn, "suspensions")
	s1 := pgstore.NewPgSuspensionStore(pg1.Pool())
	if err := s1.Suspend(ctx, quota.Suspension{OrgID: "org-durable", Reason: quota.ReasonEmergencyStop, Note: "big red button", At: time.Now().UTC(), ManualHold: true}); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	pg1.Close() // the "restart": the first replica's process is gone.

	pg2, err := pgstore.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open second: %v", err)
	}
	t.Cleanup(pg2.Close)
	s2 := pgstore.NewPgSuspensionStore(pg2.Pool())
	got, ok, err := s2.IsSuspended(ctx, "org-durable")
	if err != nil || !ok {
		t.Fatalf("IsSuspended after reopen = ok %v, err %v; want true, nil", ok, err)
	}
	if got.Reason != quota.ReasonEmergencyStop || !got.ManualHold {
		t.Fatalf("record after reopen = %+v, want the emergency stop with manual hold", got)
	}
}
