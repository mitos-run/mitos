package credfile

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCreds(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}
}

func TestToken_ReturnsTokenFromFile(t *testing.T) {
	dir := t.TempDir()
	writeCreds(t, dir, `{"token":"file-tok","email":"a@b.c","default_org":"org-1"}`)
	t.Setenv("MITOS_CONFIG_DIR", dir)

	got, err := Token()
	if err != nil {
		t.Fatalf("Token() error = %v, want nil", err)
	}
	if got != "file-tok" {
		t.Fatalf("Token() = %q, want %q", got, "file-tok")
	}
}

func TestToken_MissingFileIsEmptyNoError(t *testing.T) {
	dir := t.TempDir() // empty: no credentials.json
	t.Setenv("MITOS_CONFIG_DIR", dir)

	got, err := Token()
	if err != nil {
		t.Fatalf("Token() on missing file error = %v, want nil", err)
	}
	if got != "" {
		t.Fatalf("Token() on missing file = %q, want empty", got)
	}
}

func TestToken_InvalidJSONIsEmptyNoError(t *testing.T) {
	dir := t.TempDir()
	writeCreds(t, dir, `{ not valid json`)
	t.Setenv("MITOS_CONFIG_DIR", dir)

	got, err := Token()
	if err != nil {
		t.Fatalf("Token() on invalid json error = %v, want nil", err)
	}
	if got != "" {
		t.Fatalf("Token() on invalid json = %q, want empty", got)
	}
}

func TestToken_NoTokenFieldIsEmpty(t *testing.T) {
	dir := t.TempDir()
	writeCreds(t, dir, `{"email":"a@b.c"}`)
	t.Setenv("MITOS_CONFIG_DIR", dir)

	got, err := Token()
	if err != nil {
		t.Fatalf("Token() error = %v, want nil", err)
	}
	if got != "" {
		t.Fatalf("Token() = %q, want empty", got)
	}
}
