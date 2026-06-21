package apierr

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEncodeWritesEnvelopeWithCodeAndRemediation(t *testing.T) {
	rr := httptest.NewRecorder()
	Encode(rr, Error{
		Code:        "not_found",
		Message:     "sandbox not found",
		Cause:       "no sandbox registered for id sb-1",
		Remediation: "Confirm the sandbox id exists and is Ready before calling.",
		Status:      404,
	})

	if rr.Code != 404 {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}

	var got struct {
		Error struct {
			Code        string `json:"code"`
			Message     string `json:"message"`
			Cause       string `json:"cause"`
			Remediation string `json:"remediation"`
		} `json:"error"`
	}
	if err := json.NewDecoder(bytes.NewReader(rr.Body.Bytes())).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error.Code != "not_found" {
		t.Fatalf("code = %q, want not_found", got.Error.Code)
	}
	if got.Error.Remediation == "" {
		t.Fatal("remediation must not be empty")
	}
	if got.Error.Message != "sandbox not found" {
		t.Fatalf("message = %q", got.Error.Message)
	}
}

func TestBudgetExhaustedCarriesCodeAndOrchestratorRemediation(t *testing.T) {
	e := Get(CodeBudgetExhausted)
	if e.Code != string(CodeBudgetExhausted) {
		t.Fatalf("code = %q, want budget_exhausted", e.Code)
	}
	if e.Status != 403 {
		t.Fatalf("status = %d, want 403 (creator-scope refusal, not retryable by the sandbox)", e.Status)
	}
	if e.Remediation == "" {
		t.Fatal("budget_exhausted must carry a remediation")
	}
	// The remediation must name the orchestrator escalation path: the in-sandbox
	// agent cannot widen its own creator-set budget (issue #25 §3).
	if !strings.Contains(e.Remediation, "orchestrator") {
		t.Errorf("remediation must name the orchestrator escalation path, got %q", e.Remediation)
	}
	// A budget_exhausted error carries the exhausted dimension and remaining
	// allowance as structured context; assert the WithContext copy preserves the
	// catalogue code and remediation.
	withCtx := e.WithContext(map[string]any{"sandbox": "sb-7", "dimension": "maxForks", "remaining": 0})
	if withCtx.Code != e.Code || withCtx.Remediation != e.Remediation {
		t.Fatal("WithContext must preserve code and remediation")
	}
	if withCtx.Context["dimension"] != "maxForks" {
		t.Fatalf("context dimension = %v, want maxForks", withCtx.Context["dimension"])
	}
}

// TestTimeoutFamilyCodesAreDistinct asserts the timeout/cancel family is
// discriminable: idle-timeout, execution-deadline, request-canceled, and the
// requested-timeout-too-large rejection each have their own typed code and a
// distinct HTTP status, so a caller branches on the code, never on message text
// (issue #216).
func TestTimeoutFamilyCodesAreDistinct(t *testing.T) {
	cases := []struct {
		code   Code
		status int
	}{
		{CodeIdleTimeout, 410},
		{CodeExecTimeout, 504},
		{CodeCanceled, 499},
		{CodeTimeoutTooLarge, 400},
		{CodeRateLimited, 429},
	}
	seen := map[string]bool{}
	for _, c := range cases {
		e := Get(c.code)
		if e.Code != string(c.code) {
			t.Errorf("Get(%q).Code = %q, want %q", c.code, e.Code, c.code)
		}
		if e.Status != c.status {
			t.Errorf("code %q: status = %d, want %d", c.code, e.Status, c.status)
		}
		if e.Remediation == "" {
			t.Errorf("code %q: empty remediation", c.code)
		}
		if seen[e.Code] {
			t.Errorf("code %q: duplicate", c.code)
		}
		seen[e.Code] = true
	}
}

// TestBuildFailedNamesTheFailingStep asserts the build_failed code exists, is a
// 422 (the build recipe was processed but a step failed), and its remediation
// tells the caller to inspect the named failing step (issue #220).
func TestBuildFailedNamesTheFailingStep(t *testing.T) {
	e := Get(CodeBuildFailed)
	if e.Code != string(CodeBuildFailed) {
		t.Fatalf("code = %q, want build_failed", e.Code)
	}
	if e.Status != 422 {
		t.Fatalf("status = %d, want 422", e.Status)
	}
	if e.Remediation == "" {
		t.Fatal("build_failed must carry a remediation")
	}
	withCtx := e.WithContext(map[string]any{"step": 2, "step_kind": "run"})
	if withCtx.Context["step"] != 2 {
		t.Fatalf("context step not carried: %v", withCtx.Context)
	}
}

func TestCatalogueEntriesAllCarryCodeAndRemediation(t *testing.T) {
	for name, e := range Catalogue {
		if e.Code == "" {
			t.Errorf("catalogue %q: empty code", name)
		}
		if e.Remediation == "" {
			t.Errorf("catalogue %q (%s): empty remediation", name, e.Code)
		}
		if e.Status < 400 || e.Status > 599 {
			t.Errorf("catalogue %q (%s): status %d not a 4xx/5xx", name, e.Code, e.Status)
		}
	}
}

func TestWithCausePreservesCatalogueFieldsAndDoesNotMutate(t *testing.T) {
	base := Catalogue["not_found"]
	withCause := base.WithCause("no sandbox registered for id sb-9")
	if withCause.Cause != "no sandbox registered for id sb-9" {
		t.Fatalf("cause = %q", withCause.Cause)
	}
	if base.Cause != "" {
		t.Fatal("WithCause must not mutate the catalogue entry")
	}
	if withCause.Code != base.Code || withCause.Remediation != base.Remediation {
		t.Fatal("WithCause must preserve code and remediation")
	}
}

func TestEnvelopeJSONKeysAreStable(t *testing.T) {
	rr := httptest.NewRecorder()
	Encode(rr, Catalogue["not_found"].WithCause("x"))
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := raw["error"]; !ok {
		t.Fatal("top-level key must be \"error\"")
	}
	var inner map[string]json.RawMessage
	if err := json.Unmarshal(raw["error"], &inner); err != nil {
		t.Fatalf("unmarshal inner: %v", err)
	}
	for _, k := range []string{"code", "message", "remediation"} {
		if _, ok := inner[k]; !ok {
			t.Errorf("error object missing required key %q", k)
		}
	}
}
