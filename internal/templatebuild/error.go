package templatebuild

import (
	"fmt"

	"mitos.run/mitos/api/v1alpha1"
	"mitos.run/mitos/internal/apierr"
)

// StepError is the typed error a failing build step produces. It carries the
// failing step's index and kind so a caller (and an LLM) can fix exactly that
// step, and it maps onto the apierr.CodeBuildFailed envelope (issue #28 typed
// errors, issue #220 build error surfacing). It wraps the underlying cause so
// errors.Is/As reach it.
//
// Security: the message and the envelope context name the step index and kind
// and carry the underlying cause string, but NEVER the raw step command or env
// value, which may contain a secret argument. The step is identified by its
// position and kind only.
type StepError struct {
	Index    int
	StepKind v1alpha1.BuildStepType
	cause    error
}

// NewStepError builds a StepError for the step at index that failed with cause.
func NewStepError(index int, step v1alpha1.BuildStep, cause error) *StepError {
	return &StepError{Index: index, StepKind: step.Type, cause: cause}
}

func (e *StepError) Error() string {
	// Name the step by index and kind only; the command text is not included so a
	// secret in a run command or env value never lands in a log or error message.
	return fmt.Sprintf("template build failed at step %d (%s): %v", e.Index, e.StepKind, e.cause)
}

// Unwrap exposes the underlying cause for errors.Is/As.
func (e *StepError) Unwrap() error { return e.cause }

// Code returns the typed apierr code for a build failure.
func (e *StepError) Code() apierr.Code { return apierr.CodeBuildFailed }

// APIError maps the StepError onto the LLM-legible envelope: the build_failed
// catalogue entry, a cause naming the step, and a context carrying the failing
// step index and kind so the caller fixes exactly that step. The cause string
// carries the underlying error but not the raw command text.
func (e *StepError) APIError() apierr.Error {
	cause := "build step failed"
	if e.cause != nil {
		cause = e.cause.Error()
	}
	return apierr.Get(apierr.CodeBuildFailed).
		WithCause(cause).
		WithContext(map[string]any{
			"step":      e.Index,
			"step_kind": string(e.StepKind),
		})
}
