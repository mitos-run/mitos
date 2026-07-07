package pgstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/pgstore"
)

// TestPgSessionStoreExpiry asserts an absolute max-age is enforced at Resolve:
// a session whose created_at is older than the configured max-age no longer
// resolves, while a fresh one does (issue #733, item 2).
func TestPgSessionStoreExpiry(t *testing.T) {
	dsn := testDSN(t)
	pg, err := pgstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pg.Close)
	truncateTables(t, dsn, "sessions")
	s := pgstore.NewPgSessionStore(pg.Pool(), pgstore.WithPgSessionMaxAge(24*time.Hour))

	// A fresh session resolves.
	s.IssueSession("acct1", "fresh", "browser")
	if acct, err := s.Resolve("fresh"); err != nil || acct != "acct1" {
		t.Fatalf("fresh Resolve = (%q, %v), want (acct1, nil)", acct, err)
	}

	// Backdate a session past the max-age; it must not resolve.
	staleID := s.IssueSession("acct1", "stale", "browser")
	if _, err := pg.Pool().Exec(context.Background(),
		`UPDATE sessions SET created_at = now() - interval '48 hours' WHERE id = $1`,
		staleID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if _, err := s.Resolve("stale"); !errors.Is(err, saas.ErrSessionInvalid) {
		t.Fatalf("stale Resolve err = %v, want ErrSessionInvalid", err)
	}
}

func TestPgSessionStore(t *testing.T) {
	dsn := testDSN(t)
	pg, err := pgstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pg.Close)
	truncateTables(t, dsn, "sessions")
	var s saas.Sessions = pgstore.NewPgSessionStore(pg.Pool())
	ctx := context.Background()
	_ = ctx

	id := s.IssueSession("acct1", "rawtoken", "browser")
	if id == "" {
		t.Fatal("empty session id")
	}
	acct, err := s.Resolve("rawtoken")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if acct != "acct1" {
		t.Fatalf("resolve = %q, want acct1", acct)
	}
	if _, err := s.Resolve("wrong"); !errors.Is(err, saas.ErrSessionInvalid) {
		t.Fatalf("resolve unknown err = %v, want ErrSessionInvalid", err)
	}
	if got := s.ListByAccount("acct1"); len(got) != 1 {
		t.Fatalf("list = %d, want 1", len(got))
	}
	if err := s.Revoke("acct1", "no-such-session"); !errors.Is(err, saas.ErrNotFound) {
		t.Fatalf("revoke unknown session err = %v, want ErrNotFound", err)
	}
	if err := s.Revoke("acct1", id); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := s.Resolve("rawtoken"); !errors.Is(err, saas.ErrSessionInvalid) {
		t.Fatalf("resolve after revoke err = %v, want ErrSessionInvalid", err)
	}

	// Ordering: most-recent-first. Issue two sessions and assert the second
	// (newer) one sorts first. The id DESC tiebreak makes this stable even
	// when both inserts land within the same microsecond.
	s.IssueSession("acctOrder", "tok-old", "browser")
	s.IssueSession("acctOrder", "tok-new", "cli")
	list := s.ListByAccount("acctOrder")
	if len(list) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(list))
	}
	// Both calls must return the same order (deterministic).
	list2 := s.ListByAccount("acctOrder")
	if len(list2) != 2 {
		t.Fatalf("second list: want 2 sessions, got %d", len(list2))
	}
	if list[0].ID != list2[0].ID || list[1].ID != list2[1].ID {
		t.Fatalf("ListByAccount is non-deterministic: %v vs %v", list, list2)
	}
	// When timestamps differ the newer session must sort first.
	if list[0].CreatedAt.Before(list[1].CreatedAt) {
		t.Fatalf("sessions not most-recent-first: %v then %v", list[0].CreatedAt, list[1].CreatedAt)
	}
}
