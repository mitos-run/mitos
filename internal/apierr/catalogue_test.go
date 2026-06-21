package apierr

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// TestEveryCodeIsTyped asserts the typed Code constants and the Catalogue map
// agree: every Catalogue entry is keyed by a typed code, and every typed code
// has a Catalogue entry. This is what keeps the doc table and the code from
// drifting: the doc is checked against Codes(), and Codes() is checked here
// against the map the handlers actually use.
func TestEveryCodeIsTyped(t *testing.T) {
	codes := Codes()
	if len(codes) == 0 {
		t.Fatal("Codes() returned no codes")
	}
	for _, c := range codes {
		e, ok := Catalogue[string(c)]
		if !ok {
			t.Errorf("typed code %q has no Catalogue entry", c)
			continue
		}
		if e.Code != string(c) {
			t.Errorf("Catalogue[%q].Code = %q, want %q", c, e.Code, c)
		}
	}
	if len(codes) != len(Catalogue) {
		t.Errorf("Codes() has %d entries, Catalogue has %d; they must be 1:1", len(codes), len(Catalogue))
	}
}

// TestEveryCatalogueEntryHasRemediation is the STATIC remediation guarantee: it
// asserts every entry in the single typed catalogue carries a non-empty
// remediation, so an unexercised error path cannot ship without one. The
// runtime envelope test (internal/daemon/error_envelope_test.go) only covers
// exercised paths; this covers the whole catalogue.
func TestEveryCatalogueEntryHasRemediation(t *testing.T) {
	for _, c := range Codes() {
		e := Get(c)
		if strings.TrimSpace(e.Remediation) == "" {
			t.Errorf("code %q: empty remediation (every error path must carry actionable remediation, issue #28)", c)
		}
		if e.Status < 400 || e.Status > 599 {
			t.Errorf("code %q: status %d is not a 4xx/5xx", c, e.Status)
		}
	}
}

// TestDocCatalogueIsInSyncWithCode asserts every typed code appears in the
// normative doc table (docs/api/errors.md) and the doc lists no code that the
// catalogue does not define. The doc is the single source of truth for agents;
// this test makes the doc and the code unable to drift.
func TestDocCatalogueIsInSyncWithCode(t *testing.T) {
	docPath := repoFile(t, "docs/api/errors.md")
	body, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read %s: %v", docPath, err)
	}
	// Codes appear in the table wrapped in backticks: | `code` | ...
	rowCode := regexp.MustCompile("(?m)^\\| `([a-z_]+)` \\|")
	documented := map[string]bool{}
	for _, m := range rowCode.FindAllStringSubmatch(string(body), -1) {
		documented[m[1]] = true
	}
	for _, c := range Codes() {
		if !documented[string(c)] {
			t.Errorf("code %q is not documented in docs/api/errors.md (add a table row)", c)
		}
	}
	// Reverse: a documented code must exist in the catalogue.
	for code := range documented {
		if _, ok := Catalogue[code]; !ok {
			t.Errorf("docs/api/errors.md documents code %q that the catalogue does not define", code)
		}
	}
}

// TestJSONSchemaEnumIsInSyncWithCode asserts the published JSON Schema's code
// enum lists exactly the catalogue codes, so the machine-readable schema an
// agent consumes cannot drift from the code.
func TestJSONSchemaEnumIsInSyncWithCode(t *testing.T) {
	schemaPath := repoFile(t, "docs/api/error-schema.json")
	body, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("read %s: %v", schemaPath, err)
	}
	var schema struct {
		Properties struct {
			Error struct {
				Properties struct {
					Code struct {
						Enum []string `json:"enum"`
					} `json:"code"`
				} `json:"properties"`
			} `json:"error"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(body, &schema); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	enum := map[string]bool{}
	for _, c := range schema.Properties.Error.Properties.Code.Enum {
		enum[c] = true
	}
	for _, c := range Codes() {
		if !enum[string(c)] {
			t.Errorf("code %q missing from docs/api/error-schema.json enum", c)
		}
	}
	if len(enum) != len(Codes()) {
		t.Errorf("schema enum has %d codes, catalogue has %d", len(enum), len(Codes()))
	}
}

// TestLLMsTxtReferencesEveryCode asserts llms.txt names every code, so the
// agent-facing index stays complete as codes are added.
func TestLLMsTxtReferencesEveryCode(t *testing.T) {
	body, err := os.ReadFile(repoFile(t, "llms.txt"))
	if err != nil {
		t.Fatalf("read llms.txt: %v", err)
	}
	text := string(body)
	for _, c := range Codes() {
		if !strings.Contains(text, "`"+string(c)+"`") {
			t.Errorf("llms.txt does not reference code %q", c)
		}
	}
}

// repoFile resolves a path relative to the repository root from this test file's
// location, so the test runs regardless of the working directory.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// internal/apierr/<file> -> repo root is two dirs up from internal/apierr.
	root := filepath.Join(filepath.Dir(thisFile), "..", "..")
	return filepath.Join(root, rel)
}
