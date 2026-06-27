package pgstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"mitos.run/mitos/internal/saas/onboarding"
	"mitos.run/mitos/internal/saas/pgstore"
)

func TestPgPendingStore(t *testing.T) {
	dsn := testDSN(t)
	truncateTables(t, dsn, "pending_signups", "waitlist_entries")
	pg, err := pgstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pg.Close)
	s := pgstore.NewPgPendingStore(pg.Pool())
	ctx := context.Background()
	now := time.Unix(1700000000, 0).UTC()

	// Unknown hash must return ErrPendingNotFound.
	_, unknownErr := s.GetPendingByTokenHash(ctx, "no-such-hash")
	if !errors.Is(unknownErr, onboarding.ErrPendingNotFound) {
		t.Fatalf("unknown hash: got %v, want onboarding.ErrPendingNotFound", unknownErr)
	}

	p := onboarding.PendingSignup{ID: "p1", Email: "a@b.com", TokenHash: "h1", CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour)}
	if err := s.PutPending(ctx, p); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.GetPendingByTokenHash(ctx, "h1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Email != "a@b.com" || got.Verified {
		t.Fatalf("got = %+v", got)
	}
	if err := s.MarkVerified(ctx, "h1", "acct1"); err != nil {
		t.Fatalf("markverified: %v", err)
	}
	got2, err := s.GetPendingByTokenHash(ctx, "h1")
	if err != nil {
		t.Fatalf("get after verify: %v", err)
	}
	if !got2.Verified || got2.AccountID != "acct1" {
		t.Fatalf("after verify = %+v", got2)
	}

	if err := s.MarkVerified(ctx, "no-such-hash", "x"); !errors.Is(err, onboarding.ErrPendingNotFound) {
		t.Fatalf("MarkVerified unknown hash err = %v, want ErrPendingNotFound", err)
	}

	if err := s.AddWaitlist(ctx, onboarding.WaitlistEntry{Email: "w@b.com", CreatedAt: now}); err != nil {
		t.Fatalf("waitlist add: %v", err)
	}
	wl, err := s.Waitlist(ctx)
	if err != nil {
		t.Fatalf("waitlist list: %v", err)
	}
	if len(wl) != 1 || wl[0].Email != "w@b.com" {
		t.Fatalf("waitlist = %+v", wl)
	}
}
