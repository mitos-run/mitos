package pgstore

import (
	"strings"
	"testing"
)

// TestMigrationsNeverUseConcurrently guards a deploy-breaking foot-gun: the
// embedded runner applies every migration INSIDE a transaction (applyMigration
// pairs the SQL with its schema_migrations bookkeeping atomically), and
// CREATE INDEX CONCURRENTLY (or any CONCURRENTLY form) cannot run in a
// transaction block. A migration using it would ERROR at startup, and the
// fail-closed migration gate would keep the binary down on that release. This
// test runs without a database (the FS is embedded), so the mistake is caught
// at unit-test time, not at rollout.
func TestMigrationsNeverUseConcurrently(t *testing.T) {
	names, err := migrationNames()
	if err != nil {
		t.Fatalf("migrationNames: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("no embedded migrations found; the embed glob is broken")
	}
	for _, name := range names {
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(strings.ToUpper(stripSQLLineComments(string(body))), "CONCURRENTLY") {
			t.Errorf("%s uses CONCURRENTLY, which cannot run inside the runner's per-migration transaction and would fail the startup migration gate; use the plain (blocking) form", name)
		}
	}
}

// stripSQLLineComments drops "--" line comments so a migration may EXPLAIN why
// it avoids CONCURRENTLY without tripping the guard on the word itself.
func stripSQLLineComments(sql string) string {
	var b strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}
