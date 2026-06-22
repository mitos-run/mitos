package mitos

import "strings"

// Error is the LLM-legible error returned by the mitos SDK. It mirrors the
// server envelope {error:{code, message, cause, remediation}} and the Python
// AgentRunError, the TypeScript AgentRunError, the Ruby MitosError, and the Java
// MitosException.
//
// Code is a stable, machine-readable identifier callers branch on (with
// errors.Is, never the message text). Cause is the underlying detail (the server
// body, redacted of any bearer token). Remediation is a short actionable hint.
// Status is the HTTP status when the error came from a response, or 0 otherwise
// (for example an invalid id rejected before any request is sent).
//
// Security: an Error never carries a secret value. The SDK redacts the
// configured bearer token from any response body before it becomes a cause, so a
// token a hostile or misconfigured server reflects into its error body never
// surfaces in Error.Error().
type Error struct {
	// Code is the stable, machine-readable error code. Branch on this with
	// errors.Is(err, &mitos.Error{Code: ...}), not on the message text.
	Code string
	// Message is a human-readable summary. It never contains a secret value.
	Message string
	// Cause is the underlying detail (the server body, redacted of any token).
	Cause string
	// Remediation is a short, actionable hint for resolving the error.
	Remediation string
	// Status is the HTTP status code, or 0 when the error did not come from a
	// response.
	Status int
}

// Error renders the error as a single line. It includes the code, message,
// cause, and remediation but NEVER the bearer token, which the SDK has already
// redacted from the cause.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("[")
	b.WriteString(e.Code)
	b.WriteString("] ")
	b.WriteString(e.Message)
	if e.Cause != "" {
		b.WriteString(" | cause: ")
		b.WriteString(e.Cause)
	}
	if e.Remediation != "" {
		b.WriteString(" | remediation: ")
		b.WriteString(e.Remediation)
	}
	return b.String()
}

// Is reports whether target is an *Error with the same Code, so callers can
// write errors.Is(err, &mitos.Error{Code: "not_found"}). A target with an empty
// Code matches any *Error, so errors.Is(err, &mitos.Error{}) tests "is this an
// SDK error" without pinning the code.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok {
		return false
	}
	if t.Code == "" {
		return true
	}
	return e.Code == t.Code
}
