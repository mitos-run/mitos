package templatebuild

import (
	"errors"
	"strings"
	"testing"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/apierr"
)

// TestBuildErrorCarriesTypedCodeAndNamesFailingStep asserts a failing build step
// produces an apierr.Error with the build_failed code whose context names the
// failing step index and kind and whose remediation is actionable.
func TestBuildErrorCarriesTypedCodeAndNamesFailingStep(t *testing.T) {
	step := v1.BuildStep{Type: v1.BuildStepRun, Run: "pip install nonexistent-pkg"}
	be := NewStepError(2, step, errors.New("exit status 1"))

	if be.Code() != apierr.CodeBuildFailed {
		t.Fatalf("code = %q, want build_failed", be.Code())
	}
	env := be.APIError()
	if env.Code != string(apierr.CodeBuildFailed) {
		t.Fatalf("envelope code = %q, want build_failed", env.Code)
	}
	if env.Context["step"] != 2 {
		t.Errorf("context step = %v, want 2", env.Context["step"])
	}
	if env.Context["step_kind"] != string(v1.BuildStepRun) {
		t.Errorf("context step_kind = %v, want run", env.Context["step_kind"])
	}
	if env.Remediation == "" {
		t.Error("build error must carry a remediation")
	}
	// The cause carries the underlying error but the message names the step so an
	// LLM caller can fix exactly it.
	if !strings.Contains(be.Error(), "step 2") {
		t.Errorf("error string should name the failing step, got %q", be.Error())
	}
}

// TestBuildErrorDoesNotLeakStepRunSecretValues asserts the error string and
// envelope cause carry the underlying error and the step index/kind, but never
// the raw command text (which may contain a secret arg). Only the step kind and
// index identify the step.
func TestBuildErrorRedactsCommandText(t *testing.T) {
	step := v1.BuildStep{Type: v1.BuildStepRun, Run: "echo TOPSECRET | login --token=abc123"}
	be := NewStepError(0, step, errors.New("boom"))
	if strings.Contains(be.Error(), "TOPSECRET") || strings.Contains(be.Error(), "abc123") {
		t.Errorf("build error leaked the command text: %q", be.Error())
	}
	env := be.APIError()
	for _, v := range env.Context {
		if s, ok := v.(string); ok && (strings.Contains(s, "TOPSECRET") || strings.Contains(s, "abc123")) {
			t.Errorf("context leaked the command text: %v", env.Context)
		}
	}
}

// TestBuildErrorUnwrapsCause asserts errors.Is/As reach the wrapped cause so
// callers can branch on the underlying error.
func TestBuildErrorUnwrapsCause(t *testing.T) {
	sentinel := errors.New("sentinel")
	be := NewStepError(1, v1.BuildStep{Type: v1.BuildStepRun, Run: "x"}, sentinel)
	if !errors.Is(be, sentinel) {
		t.Error("build error should unwrap to its cause")
	}
}
