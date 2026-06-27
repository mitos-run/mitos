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
	if err := s.Revoke("acct1", id); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := s.Resolve("rawtoken"); !errors.Is(err, saas.ErrSessionInvalid) {
		t.Fatalf("resolve after revoke err = %v, want ErrSessionInvalid", err)
	}
}
