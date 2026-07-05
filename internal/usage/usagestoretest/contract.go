// Package usagestoretest holds the shared behavioral contract for
// usage.UsageStore. It is run against BOTH the in-memory MemUsageStore (the
// reference implementation) and the durable Postgres PgUsageStore so the durable
// store is proven equivalent to the reference, behavior for behavior: idempotent
// upsert, per-org isolation on ListRecords, half-open period bounds, and (for a
// store that also implements usage.TotalsProvider) per-org cumulative totals.
//
// The contract lives in a non-test .go file so both the usage package test and
// the pgstore package test can import and run it without duplicating assertions,
// mirroring internal/saas/storetest.
package usagestoretest

import (
	"context"
	"testing"
	"time"

	"mitos.run/mitos/internal/usage"
)

// w returns a wall-clock-aligned window start n minutes after a fixed epoch. The
// epoch is recent so the records stay inside MemUsageStore's default retention
// horizon (a contract run must not trip eviction).
func w(n int) time.Time {
	return time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Minute)
}

// RunContract exercises the full usage.UsageStore contract against the store the
// factory returns. The factory must return a FRESH, empty store on each call so
// the subtests do not see each other's data. Every subtest gets its own store.
//
// A store that additionally implements usage.TotalsProvider is exercised for the
// per-org cumulative totals contract too; a store that does not is skipped for
// that subtest.
func RunContract(t *testing.T, factory func(t *testing.T) usage.UsageStore) {
	t.Helper()

	t.Run("EmptyStoreListsNoRecords", func(t *testing.T) {
		s := factory(t)
		recs, err := s.ListRecords(context.Background(), "org-unknown", time.Time{}, time.Time{})
		if err != nil {
			t.Fatalf("ListRecords on empty store: %v", err)
		}
		if len(recs) != 0 {
			t.Fatalf("empty store returned %d records, want 0", len(recs))
		}
		if recs == nil {
			t.Fatalf("ListRecords returned a nil slice; want a non-nil empty slice")
		}
	})

	t.Run("UpsertAndListRoundTripSorted", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		// Insert out of order across two sandboxes and two windows; ListRecords must
		// return them sorted by (SandboxID, Window).
		recs := []usage.UsageRecord{
			{OrgID: "org-a", SandboxID: "sb-2", Window: w(1), VCPUSeconds: 2, MemGiBSeconds: 1, StorageGiBHours: 0.5, EgressBytes: 20, GPUSeconds: 3},
			{OrgID: "org-a", SandboxID: "sb-1", Window: w(1), VCPUSeconds: 1, MemGiBSeconds: 2, StorageGiBHours: 0.25, EgressBytes: 10, GPUSeconds: 1},
			{OrgID: "org-a", SandboxID: "sb-1", Window: w(0), VCPUSeconds: 4, MemGiBSeconds: 3, StorageGiBHours: 0.1, EgressBytes: 5, GPUSeconds: 0, Region: "fra"},
		}
		for _, r := range recs {
			if err := s.UpsertRecord(ctx, r); err != nil {
				t.Fatalf("UpsertRecord: %v", err)
			}
		}
		got, err := s.ListRecords(ctx, "org-a", time.Time{}, time.Time{})
		if err != nil {
			t.Fatalf("ListRecords: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("got %d records, want 3", len(got))
		}
		// Expected order: (sb-1, w0), (sb-1, w1), (sb-2, w1).
		wantOrder := []struct {
			sb  string
			win time.Time
		}{{"sb-1", w(0)}, {"sb-1", w(1)}, {"sb-2", w(1)}}
		for i, want := range wantOrder {
			if got[i].SandboxID != want.sb || !got[i].Window.Equal(want.win) {
				t.Fatalf("record[%d] = (%s, %s), want (%s, %s)", i, got[i].SandboxID, got[i].Window, want.sb, want.win)
			}
		}
		// Values for (sb-1, w0) must round trip, including Region (issue #712
		// phase 0: a best-effort dimension, not part of the idempotency key).
		r0 := got[0]
		if r0.VCPUSeconds != 4 || r0.MemGiBSeconds != 3 || r0.StorageGiBHours != 0.1 || r0.EgressBytes != 5 || r0.GPUSeconds != 0 || r0.Region != "fra" {
			t.Fatalf("value round trip mismatch for (sb-1, w0): %+v", r0)
		}
		// (sb-1, w1) was upserted with no Region: it must round trip as empty
		// rather than inheriting the sibling record's value.
		if got[1].Region != "" {
			t.Fatalf("record[1] Region = %q, want empty (no region was set for this record)", got[1].Region)
		}
	})

	t.Run("UpsertIsIdempotentReplaceNotAdd", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		key := usage.UsageRecord{OrgID: "org-a", SandboxID: "sb-1", Window: w(0), VCPUSeconds: 10, EgressBytes: 100, GPUSeconds: 2}
		// Upsert the same value twice: the store must hold exactly one record with
		// the unchanged value, never two and never a doubled value.
		if err := s.UpsertRecord(ctx, key); err != nil {
			t.Fatalf("UpsertRecord 1: %v", err)
		}
		if err := s.UpsertRecord(ctx, key); err != nil {
			t.Fatalf("UpsertRecord 2 (same value): %v", err)
		}
		got, _ := s.ListRecords(ctx, "org-a", time.Time{}, time.Time{})
		if len(got) != 1 {
			t.Fatalf("after duplicate upsert: %d records, want 1 (idempotent on (org, sandbox, window))", len(got))
		}
		if got[0].VCPUSeconds != 10 || got[0].EgressBytes != 100 || got[0].GPUSeconds != 2 {
			t.Fatalf("duplicate upsert changed the value: %+v", got[0])
		}
		// Upsert a corrected value for the same key: it must REPLACE, not add.
		corrected := key
		corrected.VCPUSeconds = 25
		corrected.EgressBytes = 250
		if err := s.UpsertRecord(ctx, corrected); err != nil {
			t.Fatalf("UpsertRecord corrected: %v", err)
		}
		got, _ = s.ListRecords(ctx, "org-a", time.Time{}, time.Time{})
		if len(got) != 1 {
			t.Fatalf("after corrected upsert: %d records, want 1 (replace not add)", len(got))
		}
		if got[0].VCPUSeconds != 25 || got[0].EgressBytes != 250 {
			t.Fatalf("corrected upsert did not replace: %+v", got[0])
		}
	})

	t.Run("ListRecordsIsPerOrgIsolated", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		if err := s.UpsertRecord(ctx, usage.UsageRecord{OrgID: "org-a", SandboxID: "a-sb", Window: w(0), VCPUSeconds: 1}); err != nil {
			t.Fatalf("upsert org-a: %v", err)
		}
		if err := s.UpsertRecord(ctx, usage.UsageRecord{OrgID: "org-b", SandboxID: "b-sb", Window: w(0), VCPUSeconds: 99}); err != nil {
			t.Fatalf("upsert org-b: %v", err)
		}
		a, _ := s.ListRecords(ctx, "org-a", time.Time{}, time.Time{})
		if len(a) != 1 || a[0].SandboxID != "a-sb" {
			t.Fatalf("org-a got %+v, want exactly a-sb", a)
		}
		for _, r := range a {
			if r.OrgID != "org-a" {
				t.Fatalf("cross-org record leaked into org-a list: %+v", r)
			}
		}
		b, _ := s.ListRecords(ctx, "org-b", time.Time{}, time.Time{})
		if len(b) != 1 || b[0].SandboxID != "b-sb" {
			t.Fatalf("org-b got %+v, want exactly b-sb", b)
		}
	})

	t.Run("ListRecordsHonorsHalfOpenPeriodBounds", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		for i := 0; i < 4; i++ {
			if err := s.UpsertRecord(ctx, usage.UsageRecord{OrgID: "org-a", SandboxID: "sb", Window: w(i), VCPUSeconds: float64(i)}); err != nil {
				t.Fatalf("upsert w%d: %v", i, err)
			}
		}
		// [w1, w3): includes w1 and w2, excludes w0 (before from) and w3 (>= to).
		got, err := s.ListRecords(ctx, "org-a", w(1), w(3))
		if err != nil {
			t.Fatalf("ListRecords bounded: %v", err)
		}
		if len(got) != 2 || !got[0].Window.Equal(w(1)) || !got[1].Window.Equal(w(2)) {
			t.Fatalf("bounded [w1,w3) = %+v, want exactly w1 and w2", got)
		}
		// Zero from = no lower bound; to = w(1) excludes w1 and later, leaving only w0.
		got, _ = s.ListRecords(ctx, "org-a", time.Time{}, w(1))
		if len(got) != 1 || !got[0].Window.Equal(w(0)) {
			t.Fatalf("[,w1) = %+v, want exactly w0", got)
		}
		// Zero to = no upper bound; from = w(2) leaves w2 and w3.
		got, _ = s.ListRecords(ctx, "org-a", w(2), time.Time{})
		if len(got) != 2 || !got[0].Window.Equal(w(2)) || !got[1].Window.Equal(w(3)) {
			t.Fatalf("[w2,) = %+v, want w2 and w3", got)
		}
	})

	t.Run("TotalsByOrgCumulativeAndIsolated", func(t *testing.T) {
		s := factory(t)
		tp, ok := s.(usage.TotalsProvider)
		if !ok {
			t.Skip("store does not implement usage.TotalsProvider")
		}
		// org-a: two records across windows; org-b: one record.
		mustUpsert(t, s, usage.UsageRecord{OrgID: "org-a", SandboxID: "sb", Window: w(0), VCPUSeconds: 3, EgressBytes: 30, GPUSeconds: 1})
		mustUpsert(t, s, usage.UsageRecord{OrgID: "org-a", SandboxID: "sb", Window: w(1), VCPUSeconds: 4, EgressBytes: 40, GPUSeconds: 2})
		mustUpsert(t, s, usage.UsageRecord{OrgID: "org-b", SandboxID: "sb", Window: w(0), VCPUSeconds: 100, EgressBytes: 1, GPUSeconds: 0})

		totals := tp.TotalsByOrg()
		a := totals["org-a"]
		if a.VCPUSeconds != 7 || a.EgressBytes != 70 || a.GPUSeconds != 3 {
			t.Fatalf("org-a totals = %+v, want vcpu=7 egress=70 gpu=3", a)
		}
		b := totals["org-b"]
		if b.VCPUSeconds != 100 || b.EgressBytes != 1 {
			t.Fatalf("org-b totals = %+v, want vcpu=100 egress=1", b)
		}
		// A correction to an existing key must move the total by exactly the delta,
		// not add a second contribution.
		mustUpsert(t, s, usage.UsageRecord{OrgID: "org-a", SandboxID: "sb", Window: w(0), VCPUSeconds: 5, EgressBytes: 50, GPUSeconds: 1})
		a = tp.TotalsByOrg()["org-a"]
		// w0 went 3->5 (vcpu) and 30->50 (egress); w1 unchanged (4, 40).
		if a.VCPUSeconds != 9 || a.EgressBytes != 90 || a.GPUSeconds != 3 {
			t.Fatalf("org-a totals after correction = %+v, want vcpu=9 egress=90 gpu=3", a)
		}
	})
}

func mustUpsert(t *testing.T, s usage.UsageStore, r usage.UsageRecord) {
	t.Helper()
	if err := s.UpsertRecord(context.Background(), r); err != nil {
		t.Fatalf("UpsertRecord: %v", err)
	}
}
