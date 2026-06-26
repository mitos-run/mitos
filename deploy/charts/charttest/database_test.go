package charttest

import (
	"strings"
	"testing"
)

// TestDatabaseDSNAbsentByDefault asserts the default render injects no
// MITOS_DATABASE_DSN env: with no database.dsnSecretRef.name the gateway and
// console run in-memory (dev only), and nothing references a database Secret.
func TestDatabaseDSNAbsentByDefault(t *testing.T) {
	out := render(t)
	if strings.Contains(out, "MITOS_DATABASE_DSN") {
		t.Fatal("MITOS_DATABASE_DSN rendered when database.dsnSecretRef.name is unset; default must be in-memory")
	}
}

// TestDatabaseDSNInjectedAsSecretKeyRef asserts that setting
// database.dsnSecretRef.name injects MITOS_DATABASE_DSN into BOTH the gateway and
// the console as a secretKeyRef to that Secret and key, NEVER as a plaintext
// value. The DSN carries the database password, so a plaintext value in a
// rendered manifest would leak it.
func TestDatabaseDSNInjectedAsSecretKeyRef(t *testing.T) {
	out := render(t, "database.dsnSecretRef.name=mitos-db")

	gateway := section(t, out, "kind: Deployment", "mitos-gateway")
	mustSecretKeyRef(t, gateway, "mitos-db", "dsn")

	console := section(t, out, "kind: Deployment", "mitos-console")
	mustSecretKeyRef(t, console, "mitos-db", "dsn")

	// The DSN must never appear as a plaintext env value anywhere in the render.
	if strings.Contains(out, `name: MITOS_DATABASE_DSN`) && strings.Contains(out, `value: "postgres://`) {
		t.Fatal("MITOS_DATABASE_DSN rendered with a plaintext value; it must be a secretKeyRef")
	}
}

// TestDatabaseDSNCustomKey asserts database.dsnSecretRef.key overrides the
// Secret key the env is sourced from.
func TestDatabaseDSNCustomKey(t *testing.T) {
	out := render(t, "database.dsnSecretRef.name=mitos-db", "database.dsnSecretRef.key=connection")
	console := section(t, out, "kind: Deployment", "mitos-console")
	mustSecretKeyRef(t, console, "mitos-db", "connection")
}

// mustSecretKeyRef asserts the rendered document contains a MITOS_DATABASE_DSN
// env entry that is a secretKeyRef to the given Secret name and key, and that it
// is not a plaintext value.
func mustSecretKeyRef(t *testing.T, doc, secretName, key string) {
	t.Helper()
	idx := strings.Index(doc, "name: MITOS_DATABASE_DSN")
	if idx < 0 {
		t.Fatalf("MITOS_DATABASE_DSN env not found in document:\n%s", doc)
	}
	rest := doc[idx:]
	// Scope to the entry: stop at the next env entry or the ports block.
	if end := strings.Index(rest[len("name: MITOS_DATABASE_DSN"):], "- name:"); end >= 0 {
		rest = rest[:end]
	}
	for _, want := range []string{"valueFrom:", "secretKeyRef:", "name: \"" + secretName + "\"", "key: \"" + key + "\""} {
		if !strings.Contains(rest, want) {
			t.Errorf("MITOS_DATABASE_DSN env missing %q:\n%s", want, rest)
		}
	}
	if strings.Contains(rest, "value: ") {
		t.Errorf("MITOS_DATABASE_DSN must be a secretKeyRef, not a plaintext value:\n%s", rest)
	}
}
