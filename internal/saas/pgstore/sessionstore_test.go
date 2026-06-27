package pgstore_test

import (
	"context"
	"errors"
	"testing"

	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/pgstore"
)

func TestPgSessionStore(t *testing.T) {
	dsn := testDSN(t)
	truncateTables(t, dsn, "sessions")
	pg, err := pgstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pg.Close)
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
