# Workstream 2: durable Postgres stores Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the onboarding credit ledger, pending-signup store, and browser sessions Postgres-backed so a real hosted signup (and its $5 credit and login) survives a console restart, behind the existing interfaces.

**Architecture:** Add one migration (`0002`) and three pg-backed stores in `internal/saas/pgstore`, mirroring the existing `PgStore` pattern (pgxpool, embedded migrations, parameterized queries). `CreditLedger` and `PendingStore` are already interfaces; `SessionStore` is concrete, so extract a `Sessions` interface that both the in-memory store and the new pg store satisfy. Wire the console to use the durable versions when a DSN is set, in-memory otherwise (the existing dev fallback).

**Tech Stack:** Go, jackc/pgx v5 (`pgxpool`), the existing `internal/saas/pgstore` migration runner, `node:test`-equivalent Go `testing` with the repo's `MITOS_TEST_DATABASE_DSN` skip-guard.

## Global Constraints

- Never use em (U+2014) or en (U+2013) dashes anywhere (source, comments, SQL, Markdown, commit messages). ASCII hyphen-minus only. (CLAUDE.md punctuation rule.)
- Error wrapping: `fmt.Errorf("context: %w", err)`. Octal literals `0o644`. gofmt + golangci-lint clean is a merge requirement.
- Secrets and DSNs are never logged, never in error messages. Reuse the package `redact()` helper on connection errors.
- TDD: write the failing test first; behavior change and its test land in the same commit.
- DCO: every commit MUST carry `Signed-off-by` (`git commit -s`). Conventional commit prefixes (feat, test, refactor). Branch is already `hosted-launch-journey`.
- Stage explicit paths only; never `git add -A`.
- Run from `/Users/jannesstubbemann/repos/mitos-run/mitos`. Build: `go build ./...`. Vet: `go vet ./...`. Test a package: `go test ./internal/saas/pgstore/ -run X -v`.
- Postgres integration tests skip unless `MITOS_TEST_DATABASE_DSN` is set. To actually exercise them locally, start a throwaway Postgres and export the DSN (see Task 1 Step 0). New pg tests MUST follow the existing skip-guard pattern in `internal/saas/pgstore/pgstore_test.go` (`testDSN(t)` + `t.Skip`).
- New pg stores must mirror the EXACT error returns of their in-memory counterparts (read `MemCreditLedger`, `MemPendingStore`, the concrete `SessionStore`): same sentinel errors for not-found / conflict / duplicate. Do not invent new error values.
- Reuse existing pgstore helpers (`isUniqueViolation`, `timePtr`, `redact`) rather than reimplementing them.

---

### Task 1: Migration 0002 (sessions, credit ledger, pending signups, waitlist)

**Files:**
- Create: `internal/saas/pgstore/migrations/0002_onboarding_and_sessions.sql`
- Test: `internal/saas/pgstore/migrate_0002_test.go`

**Interfaces:**
- Consumes: the existing `migrate(ctx, pool)` runner and `Open` (applies all embedded migrations in lexical order).
- Produces: four tables later tasks write to: `sessions`, `credit_ledger`, `pending_signups`, `waitlist_entries`.

- [ ] **Step 0: Start a local Postgres for the test cycle (once for the whole plan)**

Run:
```bash
docker run -d --name mitos-pg -e POSTGRES_PASSWORD=test -e POSTGRES_DB=mitos -p 55432:5432 postgres:16
export MITOS_TEST_DATABASE_DSN="postgres://postgres:test@127.0.0.1:55432/mitos?sslmode=disable"
```
Expected: a container id prints; the DSN is exported in this shell. If `docker` is unavailable, note it in the report and run the suite anyway (the tests will skip, and CI will exercise them); do not fabricate a pass.

- [ ] **Step 1: Write the failing test**

Create `internal/saas/pgstore/migrate_0002_test.go`:

```go
package pgstore_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigration0002TablesExist(t *testing.T) {
	dsn := testDSN(t)
	// Open runs all embedded migrations including 0002.
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	openMigrated(t, dsn) // helper below applies migrations via pgstore.Open

	for _, table := range []string{"sessions", "credit_ledger", "pending_signups", "waitlist_entries"} {
		var exists bool
		err := pool.QueryRow(context.Background(),
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`, table).Scan(&exists)
		if err != nil {
			t.Fatalf("check %s: %v", table, err)
		}
		if !exists {
			t.Fatalf("table %s missing after migration 0002", table)
		}
	}
}
```

Add this helper to the test file (it applies migrations by opening the store):

```go
func openMigrated(t *testing.T, dsn string) {
	t.Helper()
	s, err := pgstoreOpen(t, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(s.Close)
}
```

If a `pgstoreOpen` helper does not already exist in the test package, add a thin one that calls `pgstore.Open(context.Background(), dsn)` and returns `(*pgstore.PgStore, error)`. (Check `pgstore_test.go` first; reuse its `Open` usage pattern rather than duplicating.)

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/saas/pgstore/ -run TestMigration0002TablesExist -v`
Expected: with the DSN set, FAIL because the four tables do not exist yet (migration 0002 not created). Without a DSN, it SKIPS (not a pass; create the migration anyway).

- [ ] **Step 3: Write the migration**

Create `internal/saas/pgstore/migrations/0002_onboarding_and_sessions.sql`:

```sql
-- Durable onboarding and session state, so a real signup (its $5 credit, its
-- verify link, its login) survives a console restart. Mirrors the in-memory
-- MemCreditLedger, MemPendingStore, and SessionStore.

CREATE TABLE sessions (
    id         TEXT        PRIMARY KEY,
    token_hash TEXT        NOT NULL UNIQUE,
    account_id TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    label      TEXT        NOT NULL DEFAULT ''
);
CREATE INDEX sessions_account_id_idx ON sessions (account_id);

CREATE TABLE credit_ledger (
    id          BIGSERIAL   PRIMARY KEY,
    org_id      TEXT        NOT NULL,
    kind        TEXT        NOT NULL,
    amount      BIGINT      NOT NULL,
    idem_key    TEXT        NOT NULL DEFAULT '',
    at          TIMESTAMPTZ NOT NULL,
    note        TEXT        NOT NULL DEFAULT ''
);
CREATE INDEX credit_ledger_org_id_idx ON credit_ledger (org_id);
-- Idempotency: a non-empty key is unique per org. Empty keys are non-idempotent
-- and may repeat, so they are excluded from the unique constraint.
CREATE UNIQUE INDEX credit_ledger_org_key_idx ON credit_ledger (org_id, idem_key) WHERE idem_key <> '';

CREATE TABLE pending_signups (
    id         TEXT        PRIMARY KEY,
    email      TEXT        NOT NULL,
    token_hash TEXT        NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    verified   BOOLEAN     NOT NULL DEFAULT FALSE,
    account_id TEXT        NOT NULL DEFAULT ''
);

CREATE TABLE waitlist_entries (
    id         BIGSERIAL   PRIMARY KEY,
    email      TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/saas/pgstore/ -run TestMigration0002TablesExist -v`
Expected: PASS (with the DSN set). Also run `go build ./...` to confirm the embed still compiles.

- [ ] **Step 5: Commit**

```bash
git add internal/saas/pgstore/migrations/0002_onboarding_and_sessions.sql internal/saas/pgstore/migrate_0002_test.go
git commit -s -m "feat(pgstore): migration 0002 for sessions, credit ledger, pending signups"
```

---

### Task 2: PgCreditLedger

**Files:**
- Create: `internal/saas/pgstore/creditledger.go`
- Test: `internal/saas/pgstore/creditledger_test.go`

**Interfaces:**
- Consumes: `billing.CreditLedger` interface (`Append(ctx, LedgerEntry) error`, `Balance(ctx, orgID) (Money, error)`, `Entries(ctx, orgID) ([]LedgerEntry, error)`), `billing.LedgerEntry`, `billing.Money`, `billing.EntryKind`, `billing.ErrDuplicateEntry` (read `internal/saas/billing/ledger.go` for the exact names and the duplicate-key behavior to mirror).
- Produces: `pgstore.PgCreditLedger` implementing `billing.CreditLedger`, constructed from a `*pgxpool.Pool`.

- [ ] **Step 1: Write the failing test**

Create `internal/saas/pgstore/creditledger_test.go`. Mirror the behaviors `MemCreditLedger` is tested for: append, balance is the signed sum, a duplicate non-empty key returns `billing.ErrDuplicateEntry` and does not double-count, empty keys may repeat.

```go
package pgstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"mitos.run/mitos/internal/saas/billing"
	"mitos.run/mitos/internal/saas/pgstore"
)

func TestPgCreditLedger(t *testing.T) {
	dsn := testDSN(t)
	truncateTables(t, dsn, "credit_ledger")
	pg, err := pgstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pg.Close)
	l := pgstore.NewPgCreditLedger(pg.Pool())
	ctx := context.Background()
	now := time.Unix(1700000000, 0).UTC()

	mustAppend := func(e billing.LedgerEntry) {
		t.Helper()
		if err := l.Append(ctx, e); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	mustAppend(billing.LedgerEntry{OrgID: "o1", Kind: billing.KindSignupCredit, Amount: billing.USD(5), Key: "signup:o1", At: now})
	mustAppend(billing.LedgerEntry{OrgID: "o1", Kind: billing.KindUsageDrawdown, Amount: -billing.USD(2), Key: "u1", At: now})

	bal, err := l.Balance(ctx, "o1")
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal != billing.USD(3) {
		t.Fatalf("balance = %d, want %d", bal, billing.USD(3))
	}

	// Duplicate non-empty key is rejected and does not change the balance.
	dupErr := l.Append(ctx, billing.LedgerEntry{OrgID: "o1", Kind: billing.KindSignupCredit, Amount: billing.USD(5), Key: "signup:o1", At: now})
	if !errors.Is(dupErr, billing.ErrDuplicateEntry) {
		t.Fatalf("duplicate key err = %v, want ErrDuplicateEntry", dupErr)
	}
	bal2, _ := l.Balance(ctx, "o1")
	if bal2 != billing.USD(3) {
		t.Fatalf("balance after dup = %d, want unchanged %d", bal2, billing.USD(3))
	}

	entries, err := l.Entries(ctx, "o1")
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
}
```

Add the shared test helper `truncateTables(t, dsn, tables...)` to the test package if not present (generalize the existing `truncateAll`):

```go
func truncateTables(t *testing.T, dsn string, tables ...string) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	for _, tbl := range tables {
		if _, err := pool.Exec(context.Background(), "TRUNCATE "+tbl+" RESTART IDENTITY CASCADE"); err != nil {
			t.Fatalf("truncate %s: %v", tbl, err)
		}
	}
}
```

If `PgStore` does not already expose its pool, add a `func (s *PgStore) Pool() *pgxpool.Pool { return s.pool }` accessor in `pgstore.go` (the new stores need it).

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/saas/pgstore/ -run TestPgCreditLedger -v`
Expected: FAIL to compile (`NewPgCreditLedger` undefined), or FAIL at runtime once it compiles.

- [ ] **Step 3: Write the implementation**

Create `internal/saas/pgstore/creditledger.go`:

```go
package pgstore

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"mitos.run/mitos/internal/saas/billing"
)

// PgCreditLedger is the durable, append-only credit ledger. A balance is the
// signed sum of an org's entries; a non-empty idempotency key is unique per org.
type PgCreditLedger struct {
	pool *pgxpool.Pool
}

func NewPgCreditLedger(pool *pgxpool.Pool) *PgCreditLedger { return &PgCreditLedger{pool: pool} }

var _ billing.CreditLedger = (*PgCreditLedger)(nil)

func (l *PgCreditLedger) Append(ctx context.Context, e billing.LedgerEntry) error {
	const q = `
        INSERT INTO credit_ledger (org_id, kind, amount, idem_key, at, note)
        VALUES ($1, $2, $3, $4, $5, $6)`
	_, err := l.pool.Exec(ctx, q, e.OrgID, string(e.Kind), int64(e.Amount), e.Key, e.At, e.Note)
	if e.Key != "" && isUniqueViolation(err) {
		return billing.ErrDuplicateEntry
	}
	if err != nil {
		return fmt.Errorf("append ledger entry: %w", err)
	}
	return nil
}

func (l *PgCreditLedger) Balance(ctx context.Context, orgID string) (billing.Money, error) {
	const q = `SELECT COALESCE(SUM(amount), 0) FROM credit_ledger WHERE org_id = $1`
	var sum int64
	if err := l.pool.QueryRow(ctx, q, orgID).Scan(&sum); err != nil {
		return 0, fmt.Errorf("ledger balance: %w", err)
	}
	return billing.Money(sum), nil
}

func (l *PgCreditLedger) Entries(ctx context.Context, orgID string) ([]billing.LedgerEntry, error) {
	const q = `SELECT org_id, kind, amount, idem_key, at, note FROM credit_ledger WHERE org_id = $1 ORDER BY id`
	rows, err := l.pool.Query(ctx, q, orgID)
	if err != nil {
		return nil, fmt.Errorf("ledger entries: %w", err)
	}
	defer rows.Close()
	var out []billing.LedgerEntry
	for rows.Next() {
		var e billing.LedgerEntry
		var kind string
		var amount int64
		if err := rows.Scan(&e.OrgID, &kind, &amount, &e.Key, &e.At, &e.Note); err != nil {
			return nil, fmt.Errorf("scan ledger entry: %w", err)
		}
		e.Kind = billing.EntryKind(kind)
		e.Amount = billing.Money(amount)
		out = append(out, e)
	}
	return out, rows.Err()
}
```

Note: confirm `billing.ErrDuplicateEntry` is the exact exported name in `ledger.go`; if the in-memory store returns a differently named sentinel for a duplicate key, use that one. Confirm `LedgerEntry` field names (`OrgID`, `Kind`, `Amount`, `Key`, `At`, `Note`) match `ledger.go` exactly before writing.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/saas/pgstore/ -run TestPgCreditLedger -v`
Expected: PASS (DSN set). Run `go vet ./internal/saas/pgstore/`.

- [ ] **Step 5: Commit**

```bash
git add internal/saas/pgstore/creditledger.go internal/saas/pgstore/creditledger_test.go internal/saas/pgstore/pgstore.go
git commit -s -m "feat(pgstore): durable PgCreditLedger"
```

---

### Task 3: PgPendingStore

**Files:**
- Create: `internal/saas/pgstore/pendingstore.go`
- Test: `internal/saas/pgstore/pendingstore_test.go`

**Interfaces:**
- Consumes: `onboarding.PendingStore` (`PutPending`, `GetPendingByTokenHash`, `MarkVerified`, `AddWaitlist`, `Waitlist`), `onboarding.PendingSignup`, `onboarding.WaitlistEntry`, and the exact not-found error `MemPendingStore.GetPendingByTokenHash` returns (read `internal/saas/onboarding/service.go`).
- Produces: `pgstore.PgPendingStore` implementing `onboarding.PendingStore`.

- [ ] **Step 1: Write the failing test**

Create `internal/saas/pgstore/pendingstore_test.go`. Mirror `MemPendingStore` behavior: put then get-by-hash round-trips; unknown hash returns the same not-found error the Mem store returns; `MarkVerified` flips `Verified` and sets `AccountID`; waitlist append and list.

```go
package pgstore_test

import (
	"context"
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
	got2, _ := s.GetPendingByTokenHash(ctx, "h1")
	if !got2.Verified || got2.AccountID != "acct1" {
		t.Fatalf("after verify = %+v", got2)
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
```

Add an unknown-hash assertion that matches the Mem store's exact error (read it and assert with `errors.Is` against the same sentinel).

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/saas/pgstore/ -run TestPgPendingStore -v`
Expected: FAIL (undefined `NewPgPendingStore`).

- [ ] **Step 3: Write the implementation**

Create `internal/saas/pgstore/pendingstore.go`:

```go
package pgstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"mitos.run/mitos/internal/saas/onboarding"
)

// PgPendingStore is the durable onboarding pending-signup and waitlist store.
type PgPendingStore struct {
	pool *pgxpool.Pool
}

func NewPgPendingStore(pool *pgxpool.Pool) *PgPendingStore { return &PgPendingStore{pool: pool} }

var _ onboarding.PendingStore = (*PgPendingStore)(nil)

func (s *PgPendingStore) PutPending(ctx context.Context, p onboarding.PendingSignup) error {
	const q = `
        INSERT INTO pending_signups (id, email, token_hash, created_at, expires_at, verified, account_id)
        VALUES ($1, $2, $3, $4, $5, $6, $7)
        ON CONFLICT (id) DO UPDATE SET
            email      = EXCLUDED.email,
            token_hash = EXCLUDED.token_hash,
            created_at = EXCLUDED.created_at,
            expires_at = EXCLUDED.expires_at,
            verified   = EXCLUDED.verified,
            account_id = EXCLUDED.account_id`
	_, err := s.pool.Exec(ctx, q, p.ID, p.Email, p.TokenHash, p.CreatedAt, p.ExpiresAt, p.Verified, p.AccountID)
	if err != nil {
		return fmt.Errorf("put pending signup: %w", err)
	}
	return nil
}

func (s *PgPendingStore) GetPendingByTokenHash(ctx context.Context, tokenHash string) (onboarding.PendingSignup, error) {
	const q = `SELECT id, email, token_hash, created_at, expires_at, verified, account_id FROM pending_signups WHERE token_hash = $1`
	var p onboarding.PendingSignup
	err := s.pool.QueryRow(ctx, q, tokenHash).Scan(&p.ID, &p.Email, &p.TokenHash, &p.CreatedAt, &p.ExpiresAt, &p.Verified, &p.AccountID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Return the SAME sentinel MemPendingStore returns for a missing hash.
		return onboarding.PendingSignup{}, onboarding.ErrPendingNotFound // confirm exact name in service.go; replace if different
	}
	if err != nil {
		return onboarding.PendingSignup{}, fmt.Errorf("get pending signup: %w", err)
	}
	return p, nil
}

func (s *PgPendingStore) MarkVerified(ctx context.Context, tokenHash, accountID string) error {
	const q = `UPDATE pending_signups SET verified = TRUE, account_id = $2 WHERE token_hash = $1`
	tag, err := s.pool.Exec(ctx, q, tokenHash, accountID)
	if err != nil {
		return fmt.Errorf("mark verified: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return onboarding.ErrPendingNotFound // same sentinel as above
	}
	return nil
}

func (s *PgPendingStore) AddWaitlist(ctx context.Context, e onboarding.WaitlistEntry) error {
	const q = `INSERT INTO waitlist_entries (email, created_at) VALUES ($1, $2)`
	if _, err := s.pool.Exec(ctx, q, e.Email, e.CreatedAt); err != nil {
		return fmt.Errorf("add waitlist: %w", err)
	}
	return nil
}

func (s *PgPendingStore) Waitlist(ctx context.Context) ([]onboarding.WaitlistEntry, error) {
	const q = `SELECT email, created_at FROM waitlist_entries ORDER BY id`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list waitlist: %w", err)
	}
	defer rows.Close()
	var out []onboarding.WaitlistEntry
	for rows.Next() {
		var e onboarding.WaitlistEntry
		if err := rows.Scan(&e.Email, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan waitlist: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
```

IMPORTANT: `onboarding.ErrPendingNotFound` is a placeholder name. Before writing, read `internal/saas/onboarding/service.go` and use the EXACT sentinel `MemPendingStore.GetPendingByTokenHash` returns for a missing hash; if the Mem store returns a generic error, replicate it identically. Do not introduce a new error value.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/saas/pgstore/ -run TestPgPendingStore -v`
Expected: PASS (DSN set). `go vet ./internal/saas/pgstore/`.

- [ ] **Step 5: Commit**

```bash
git add internal/saas/pgstore/pendingstore.go internal/saas/pgstore/pendingstore_test.go
git commit -s -m "feat(pgstore): durable PgPendingStore"
```

---

### Task 4: Sessions interface + PgSessionStore

**Files:**
- Modify: `internal/saas/session.go` (extract a `Sessions` interface; keep the concrete in-memory type implementing it)
- Create: `internal/saas/pgstore/sessionstore.go`
- Test: `internal/saas/pgstore/sessionstore_test.go`
- Modify: `internal/saas/session.go` consumers only if they reference the concrete type where the interface now fits (e.g. `NewSessionService`)

**Interfaces:**
- Consumes: the concrete `SessionStore` methods (`IssueSession(accountID, token, label) string`, `Resolve(token) (string, error)`, `ListByAccount(accountID) []Session`, `Revoke(accountID, sessionID) error`, `RevokeAll(accountID)`), `Session` struct, `ErrSessionInvalid`, and `hashSession` (session token is sha256-hex).
- Produces: a `saas.Sessions` interface and `pgstore.PgSessionStore` implementing it.

- [ ] **Step 1: Write the failing test**

Create `internal/saas/pgstore/sessionstore_test.go`:

```go
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
```

Note: `IssueSession`/`Resolve` here have no `ctx` in the concrete signature today. Keep the `Sessions` interface signature IDENTICAL to the concrete methods (no ctx) so the in-memory type satisfies it unchanged; the pg store opens short-lived `context.Background()` internally. If the concrete methods do take a ctx, match that instead. Read `session.go` and mirror exactly.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/saas/pgstore/ -run TestPgSessionStore -v`
Expected: FAIL (undefined `saas.Sessions` and `pgstore.NewPgSessionStore`).

- [ ] **Step 3: Extract the interface**

In `internal/saas/session.go`, add the interface mirroring the concrete method set EXACTLY (copy signatures verbatim from the concrete `SessionStore`; example shape, adjust to the real signatures):

```go
// Sessions is the browser-session backend. The in-memory SessionStore and the
// durable pgstore.PgSessionStore both implement it.
type Sessions interface {
	IssueSession(accountID, token, label string) string
	Issue(accountID, token string)
	Resolve(token string) (string, error)
	ListByAccount(accountID string) []Session
	Revoke(accountID, sessionID string) error
	RevokeAll(accountID string)
}

var _ Sessions = (*SessionStore)(nil)
```

Do not change `SessionStore`'s methods; only add the interface and the assertion. If `NewSessionService` takes a concrete `*SessionStore`, change its parameter to `Sessions` so the pg store can be injected (verify no other behavior changes).

- [ ] **Step 4: Write the pg implementation**

Create `internal/saas/pgstore/sessionstore.go`. Reuse the session hashing from `saas` (sha256-hex). If `hashSession` is unexported, replicate the same sha256-hex hashing inline (it is a stable, non-secret transform), with a comment pointing at `session.go` as the source of truth.

```go
package pgstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"mitos.run/mitos/internal/saas"
)

// PgSessionStore is the durable browser-session store. It stores only the
// sha256 hash of the session token (matching internal/saas/session.go), never
// the raw token.
type PgSessionStore struct {
	pool *pgxpool.Pool
}

func NewPgSessionStore(pool *pgxpool.Pool) *PgSessionStore { return &PgSessionStore{pool: pool} }

var _ saas.Sessions = (*PgSessionStore)(nil)

func hashSession(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *PgSessionStore) IssueSession(accountID, token, label string) string {
	id := "sess-" + hashSession(token)[:16]
	const q = `
        INSERT INTO sessions (id, token_hash, account_id, created_at, label)
        VALUES ($1, $2, $3, now(), $4)
        ON CONFLICT (token_hash) DO NOTHING`
	// Best-effort: errors here surface on Resolve as ErrSessionInvalid.
	_, _ = s.pool.Exec(context.Background(), q, id, hashSession(token), accountID, label)
	return id
}

func (s *PgSessionStore) Issue(accountID, token string) { s.IssueSession(accountID, token, "") }

func (s *PgSessionStore) Resolve(token string) (string, error) {
	const q = `SELECT account_id FROM sessions WHERE token_hash = $1`
	var acct string
	err := s.pool.QueryRow(context.Background(), q, hashSession(token)).Scan(&acct)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", saas.ErrSessionInvalid
	}
	if err != nil {
		return "", fmt.Errorf("resolve session: %w", err)
	}
	return acct, nil
}

func (s *PgSessionStore) ListByAccount(accountID string) []saas.Session {
	const q = `SELECT id, account_id, created_at, label FROM sessions WHERE account_id = $1 ORDER BY created_at`
	rows, err := s.pool.Query(context.Background(), q, accountID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []saas.Session
	for rows.Next() {
		var ss saas.Session
		if err := rows.Scan(&ss.ID, &ss.AccountID, &ss.CreatedAt, &ss.Label); err != nil {
			return out
		}
		out = append(out, ss)
	}
	return out
}

func (s *PgSessionStore) Revoke(accountID, sessionID string) error {
	const q = `DELETE FROM sessions WHERE account_id = $1 AND id = $2`
	if _, err := s.pool.Exec(context.Background(), q, accountID, sessionID); err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	return nil
}

func (s *PgSessionStore) RevokeAll(accountID string) {
	const q = `DELETE FROM sessions WHERE account_id = $1`
	_, _ = s.pool.Exec(context.Background(), q, accountID)
}
```

Match the concrete `SessionStore`'s exact method signatures (including the session-id format if other code depends on it; if the in-memory store uses a counter id, the pg id format only needs to be unique and stable per token, which the hash-prefix gives). If `Revoke` on the in-memory store enforces sole-owner semantics (account must own the session), the SQL `WHERE account_id = $1 AND id = $2` already enforces that.

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/saas/pgstore/ -run TestPgSessionStore -v && go build ./...`
Expected: PASS; build clean.

- [ ] **Step 6: Commit**

```bash
git add internal/saas/session.go internal/saas/pgstore/sessionstore.go internal/saas/pgstore/sessionstore_test.go
git commit -s -m "feat(saas): Sessions interface and durable PgSessionStore"
```

---

### Task 5: Wire durable stores into the console

**Files:**
- Modify: `cmd/console/main.go` (session store selection)
- Modify: `cmd/console/onboarding.go` (pending store + credit ledger selection)
- Create: `internal/saas/pgstore/resolve_onboarding.go` (resolver helpers returning durable-or-memory)
- Test: `internal/saas/pgstore/resolve_onboarding_test.go`

**Interfaces:**
- Consumes: `pgstore.Open`/`Pool`, the three new stores, the in-memory fallbacks (`onboarding.NewMemPendingStore`, `billing.NewMemCreditLedger`, `saas.NewSessionStore`), `pgstore.EnvDSN`.
- Produces: helper functions that return the durable store when a DSN/pool is available and the in-memory one otherwise, so the console wiring stays a one-line swap.

- [ ] **Step 1: Write the failing test**

Create `internal/saas/pgstore/resolve_onboarding_test.go`:

```go
package pgstore_test

import (
	"context"
	"testing"

	"mitos.run/mitos/internal/saas/pgstore"
)

func TestResolveOnboardingStoresWithPool(t *testing.T) {
	dsn := testDSN(t)
	pg, err := pgstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pg.Close)
	ledger, pending, sessions := pgstore.OnboardingStores(pg.Pool())
	if ledger == nil || pending == nil || sessions == nil {
		t.Fatal("expected durable stores from a non-nil pool")
	}
}

func TestOnboardingStoresNilPoolPanicsOrNil(t *testing.T) {
	// Documents intent: callers pass a non-nil pool only when a DSN is set;
	// the console falls back to Mem stores when there is no pool (no call here).
	t.Skip("documentation test; console handles the no-pool branch")
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/saas/pgstore/ -run TestResolveOnboardingStores -v`
Expected: FAIL (undefined `OnboardingStores`).

- [ ] **Step 3: Write the resolver**

Create `internal/saas/pgstore/resolve_onboarding.go`:

```go
package pgstore

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/billing"
	"mitos.run/mitos/internal/saas/onboarding"
)

// OnboardingStores returns the durable credit ledger, pending store, and session
// store backed by the given pool. Callers pass a non-nil pool only when Postgres
// is configured; otherwise they use the in-memory constructors directly.
func OnboardingStores(pool *pgxpool.Pool) (billing.CreditLedger, onboarding.PendingStore, saas.Sessions) {
	return NewPgCreditLedger(pool), NewPgPendingStore(pool), NewPgSessionStore(pool)
}
```

- [ ] **Step 4: Wire the console**

In `cmd/console/onboarding.go`, where `onboarding.NewService(...)` is built with `onboarding.NewMemPendingStore()` and `billing.NewMemCreditLedger()`, select durable versions when a pool is available. Thread the `*pgstore.PgStore` (or its pool) from `main.go` into the onboarding setup, and choose:

```go
// pseudostructure: adapt to the actual function params in onboarding.go
var ledger billing.CreditLedger
var pending onboarding.PendingStore
if pool != nil { // pool is non-nil when a DSN was configured
    ledger, pending, _ = pgstore.OnboardingStores(pool)
} else {
    ledger, pending = billing.NewMemCreditLedger(), onboarding.NewMemPendingStore()
}
```

In `cmd/console/main.go`, replace `sessionStore := saas.NewSessionStore()` with a selection: if a pool is available use `pgstore.NewPgSessionStore(pool)` (typed as `saas.Sessions`), else `saas.NewSessionStore()`. To get the pool, have `pgstore.ResolveStore` also return the `*PgStore` (or add a sibling `ResolveStoreWithPool`) so `main.go` can reach `pg.Pool()`; if `ResolveStore` returns only the `saas.Store` interface today, add a small `ResolveStoreWithPool` that returns `(saas.Store, *pgxpool.Pool, func(), error)` where the pool is nil in the in-memory branch. Keep the existing `ResolveStore` for back-compat.

Confirm `NewSessionService` (if used) accepts the `saas.Sessions` interface after Task 4; pass whichever store was selected.

- [ ] **Step 5: Run build, vet, and the full saas + pgstore tests**

Run:
```bash
go build ./... && go vet ./... && go test ./internal/saas/... -v
```
Expected: build and vet clean; saas tests pass; pgstore integration tests pass with the DSN set (or skip without it). Confirm the console still starts in the in-memory branch (no DSN) and the durable branch (DSN set) by reading the selection code paths.

- [ ] **Step 6: Commit**

```bash
git add internal/saas/pgstore/resolve_onboarding.go internal/saas/pgstore/resolve_onboarding_test.go cmd/console/main.go cmd/console/onboarding.go
git commit -s -m "feat(console): use durable Postgres stores for credit, pending signups, sessions when DSN is set"
```

- [ ] **Step 7: Tear down the test Postgres (cleanup)**

Run: `docker rm -f mitos-pg 2>/dev/null || true`

---

## Self-Review

**1. Spec coverage (workstream-2 durability requirement):** The spec's Component 4 asks for Postgres-backed sessions, credit ledger, and pending store behind existing interfaces, via a new migration. Task 1 (migration), Task 2 (credit ledger), Task 3 (pending), Task 4 (sessions + interface extraction), Task 5 (wiring) cover it. SpendCapStore/StatusStore/SuspensionStore durability are explicitly out of scope here (workstream 4/6), matching the spec's note.

**2. Placeholder scan:** The two deliberate "confirm the exact name" notes (the duplicate-entry sentinel in Task 2, the not-found sentinel in Task 3, the concrete session signatures in Task 4) are not placeholders for the engineer to invent; they are instructions to read the in-memory source and mirror it exactly, with the example code given. The Task 5 console wiring is described as pseudostructure because the exact param threading depends on the current `onboarding.go` signature; the engineer adapts the named selection logic. Every store has complete, runnable implementation code.

**3. Type consistency:** `billing.CreditLedger`, `billing.LedgerEntry`, `billing.Money`, `onboarding.PendingStore`, `onboarding.PendingSignup`, `onboarding.WaitlistEntry`, `saas.Sessions`, `saas.Session`, `saas.ErrSessionInvalid`, and `pgstore.PgStore.Pool()` are used consistently across tasks. The `OnboardingStores` return triple in Task 5 matches the three stores built in Tasks 2-4.

Risk called out for the executor: the integration tests require a real Postgres (`MITOS_TEST_DATABASE_DSN`). Without docker, they skip; the executor must say so rather than claim a pass, and rely on CI (which sets the DSN) to exercise them.
