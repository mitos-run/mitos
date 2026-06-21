// Package apierr defines the LLM-legible error envelope returned by the forkd
// sandbox API and the standalone sandbox-server. Every runtime error carries a
// stable machine code, a one-line message, an underlying cause, and an
// actionable remediation, per docs/api/v2-spec.md section 2.3 and issue #28.
//
// The typed Code constants below are the single source of truth for the
// error-code catalogue. docs/api/errors.md is the normative document and is
// checked against these constants by the doc-sync test, so the doc and the code
// cannot drift. hack/check-apierr-remediation.sh is the static guarantee that
// every Error construction carries a non-empty remediation.
//
// Security: an Error never carries a secret value. Cause and message are built
// from sandbox ids, paths, and operation names only; callers must never place a
// token, secret value, or credential into any field. Logging an Error logs the
// code and message, never the request body.
package apierr

import (
	"encoding/json"
	"net/http"
	"sort"
)

// Code is a stable machine-readable error code. The constants below are the
// single source of truth: docs/api/errors.md is checked against them by
// TestDocCatalogueIsInSyncWithCode, and every constant must have a Catalogue
// entry with a non-empty remediation (TestEveryCatalogueEntryHasRemediation).
// Adding a code is an API surface change: add the constant, the Catalogue
// entry, and the docs/api/errors.md row in the same change.
type Code string

// The normative error-code set. Keep in step with docs/api/errors.md.
const (
	// CodeInvalidJSON: the request body is not valid JSON.
	CodeInvalidJSON Code = "invalid_json"
	// CodeBodyTooLarge: the request body exceeds the server size limit.
	CodeBodyTooLarge Code = "body_too_large"
	// CodeUnauthorized: the per-sandbox bearer token is missing or invalid.
	CodeUnauthorized Code = "unauthorized"
	// CodeNotFound: no such sandbox.
	CodeNotFound Code = "not_found"
	// CodeTooManyStreams: the sandbox is at its concurrent-stream limit.
	CodeTooManyStreams Code = "too_many_streams"
	// CodeBudgetExhausted: a budget-gated self-service operation (Fork,
	// Checkpoint, ExtendLifetime) was refused because the sandbox's capability
	// budget for that dimension is spent (issue #25, docs/api/v2-spec.md §3).
	CodeBudgetExhausted Code = "budget_exhausted"
	// CodeRateLimited: the caller has exceeded the request rate limit for this
	// sandbox or endpoint. Distinct from too_many_streams (a concurrent-stream
	// ceiling): rate-limited is a per-window request-rate refusal (issue #216).
	CodeRateLimited Code = "rate_limited"
	// CodeIdleTimeout: the sandbox was reaped after exceeding its idle timeout,
	// so the call hit a sandbox that is no longer running. Distinct from
	// not_found (never existed) and from execution-deadline (issue #216).
	CodeIdleTimeout Code = "idle_timeout"
	// CodeExecTimeout: a command or run_code execution ran past its requested
	// timeout (the execution deadline) and was terminated. This is the
	// execution-deadline case, distinct from idle-timeout and request-canceled
	// (issue #216).
	CodeExecTimeout Code = "exec_timeout"
	// CodeCanceled: the request or stream was canceled by the caller (the client
	// hung up or the context was canceled) before it completed. Distinct from a
	// server-side deadline (issue #216).
	CodeCanceled Code = "canceled"
	// CodeTimeoutTooLarge: the requested timeout exceeds the server ceiling. The
	// timeout is REJECTED with this error, never silently reduced, so a requested
	// deadline is always either honored or rejected (issue #216).
	CodeTimeoutTooLarge Code = "timeout_too_large"
	// CodeExecFailed: the command could not be executed in the sandbox.
	CodeExecFailed Code = "exec_failed"
	// CodeFileFailed: a file operation failed in the sandbox.
	CodeFileFailed Code = "file_failed"
	// CodeBuildFailed: a declarative template build step failed (issue #220). The
	// build recipe was accepted but a step (a run command, an env or workdir step,
	// or a copy) errored, so no snapshot was produced. The context names the
	// failing step index and kind so the caller can fix exactly that step. This is
	// a 422 (the request was well-formed but the recipe could not be carried out),
	// distinct from exec_failed (a runtime command in a live sandbox).
	CodeBuildFailed Code = "build_failed"
	// CodeInternal: an unclassified internal error.
	CodeInternal Code = "internal"
)

// Error is one LLM-legible error. Status is the HTTP status to send and is not
// serialized into the body.
type Error struct {
	Code        string         `json:"code"`
	Message     string         `json:"message"`
	Cause       string         `json:"cause,omitempty"`
	Remediation string         `json:"remediation"`
	Context     map[string]any `json:"context,omitempty"`
	Status      int            `json:"-"`
}

// WithCause returns a copy of e with the cause set. It does not mutate e, so a
// Catalogue entry can be reused safely. The cause must never contain a secret
// value.
func (e Error) WithCause(cause string) Error {
	e.Cause = cause
	return e
}

// WithContext returns a copy of e with the context map set. The context must
// never contain a secret value.
func (e Error) WithContext(ctx map[string]any) Error {
	e.Context = ctx
	return e
}

// envelope is the wire shape: {"error": {...}}.
type envelope struct {
	Error Error `json:"error"`
}

// Encode writes e as the JSON envelope with e.Status. Code and Remediation are
// always populated by the Catalogue, so a well-formed Error always satisfies the
// CI lint that rejects an error response lacking code or remediation.
func Encode(w http.ResponseWriter, e Error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(envelope{Error: e})
}

// Catalogue is the stable set of runtime error codes, keyed by the string form
// of the typed Code constants above. Handlers pick the closest entry and attach
// a cause with WithCause. Adding an entry is a documented surface change; keep
// docs/api/errors.md (the single source of truth) and docs/api/v2-spec.md in
// step. The doc-sync test enforces the doc matches these entries.
var Catalogue = map[string]Error{
	string(CodeInvalidJSON): {
		Code:        string(CodeInvalidJSON),
		Message:     "request body is not valid JSON",
		Remediation: "Send a JSON body matching the sandbox API contract for this endpoint.",
		Status:      http.StatusBadRequest,
	},
	string(CodeBodyTooLarge): {
		Code:        string(CodeBodyTooLarge),
		Message:     "request body exceeds the size limit",
		Remediation: "Reduce the payload; file content is hex-encoded and bounded by the server.",
		Status:      http.StatusRequestEntityTooLarge,
	},
	string(CodeUnauthorized): {
		Code:        string(CodeUnauthorized),
		Message:     "the bearer token is missing or invalid for this sandbox",
		Remediation: "Send Authorization: Bearer <token> with the per-sandbox token from the <name>-sandbox-token Secret.",
		Status:      http.StatusUnauthorized,
	},
	string(CodeNotFound): {
		Code:        string(CodeNotFound),
		Message:     "no such sandbox",
		Remediation: "Confirm the sandbox id exists and is Ready before calling.",
		Status:      http.StatusNotFound,
	},
	string(CodeTooManyStreams): {
		Code:        string(CodeTooManyStreams),
		Message:     "the sandbox is at its concurrent-stream limit",
		Remediation: "Close an existing streaming exec, run_code, or PTY session for this sandbox before opening another, or raise the forkd --max-streams-per-sandbox ceiling. Existing streams are unaffected.",
		Status:      http.StatusTooManyRequests,
	},
	string(CodeBudgetExhausted): {
		Code:    string(CodeBudgetExhausted),
		Message: "the sandbox capability budget for this operation is exhausted",
		// Remediation names the orchestrator escalation path: budgets are
		// creator-set, so the in-sandbox agent cannot widen its own; it must ask
		// the orchestrator (or operator) that created the sandbox to raise the
		// budget on the parent Sandbox object, or run within the remaining budget.
		Remediation: "This is a creator-set capability budget; the sandbox cannot widen its own. Request a larger budget from the orchestrator or operator that created this sandbox (raise spec.budget on the parent Sandbox), or proceed within the remaining budget reported by the Budget call. The context names the exhausted dimension and the remaining allowance.",
		Status:      http.StatusForbidden,
	},
	string(CodeRateLimited): {
		Code:        string(CodeRateLimited),
		Message:     "the request rate limit for this sandbox was exceeded",
		Remediation: "Back off and retry after the delay in the context retry_after_ms; this is a per-window request-rate limit, distinct from too_many_streams (the concurrent-stream ceiling).",
		Status:      http.StatusTooManyRequests,
	},
	string(CodeIdleTimeout): {
		Code:    string(CodeIdleTimeout),
		Message: "the sandbox was reaped after exceeding its idle timeout",
		// Distinct from not_found so a caller can tell "it idled out" from "it
		// never existed" without parsing the message. The remediation names the
		// create-a-fresh-sandbox path and the set-timeout lever.
		Remediation: "The sandbox idled out and was reaped; create a fresh sandbox (or fork from a checkpoint) and retry. Raise the idle timeout on the parent Sandbox or call set_timeout to keep an idle sandbox alive longer.",
		Status:      http.StatusGone,
	},
	string(CodeExecTimeout): {
		Code:    string(CodeExecTimeout),
		Message: "the execution ran past its requested timeout and was terminated",
		// Distinct from idle_timeout: this is the per-command execution deadline,
		// not sandbox inactivity. The context carries the timeout_s that was hit.
		Remediation: "The command exceeded its execution deadline and was killed. Raise the timeout on the exec or run_code call, or split the work into shorter steps. The context carries the timeout_s that was hit.",
		Status:      http.StatusGatewayTimeout,
	},
	string(CodeCanceled): {
		Code:    string(CodeCanceled),
		Message: "the request was canceled before it completed",
		// HTTP 499 (client closed request) is the nginx convention for a caller
		// that hung up; it is intentionally distinct from a server deadline.
		Remediation: "The request was canceled by the caller (the client closed the connection or canceled the context). Retry the call if the cancellation was not intentional.",
		Status:      499,
	},
	string(CodeTimeoutTooLarge): {
		Code:    string(CodeTimeoutTooLarge),
		Message: "the requested timeout exceeds the server ceiling",
		// Determinism rule (issue #216): a requested timeout is HONORED or
		// REJECTED, never silently reduced. The context carries the requested
		// value and the ceiling so the caller can pick a value at or under it.
		Remediation: "Request a timeout at or below the ceiling reported in the context (max_timeout_s); the server never silently reduces a requested timeout, it rejects it so the deadline you set is the deadline you get.",
		Status:      http.StatusBadRequest,
	},
	string(CodeExecFailed): {
		Code:        string(CodeExecFailed),
		Message:     "the command could not be executed in the sandbox",
		Remediation: "Inspect the cause; if it is a transport error retry, otherwise check the forkd logs for the guest agent state.",
		Status:      http.StatusInternalServerError,
	},
	string(CodeFileFailed): {
		Code:        string(CodeFileFailed),
		Message:     "the file operation failed in the sandbox",
		Remediation: "Confirm the path exists and is writable; inspect the cause for the underlying error.",
		Status:      http.StatusInternalServerError,
	},
	string(CodeBuildFailed): {
		Code:    string(CodeBuildFailed),
		Message: "a template build step failed",
		// The remediation names the failing-step pattern: the build context carries
		// the step index and kind, so the caller fixes exactly that step and
		// rebuilds. Cached steps before it are reused, so a rebuild only re-runs the
		// failing step and everything after it.
		Remediation: "A declarative build step failed; no snapshot was produced. The context names the failing step (step index and step_kind) and carries its cause. Fix that step and rebuild; cached steps before it are reused.",
		Status:      http.StatusUnprocessableEntity,
	},
	string(CodeInternal): {
		Code:        string(CodeInternal),
		Message:     "an internal error occurred",
		Remediation: "Retry the request; if it persists, inspect the forkd or sandbox-server logs.",
		Status:      http.StatusInternalServerError,
	},
}

// Codes returns every typed code in the catalogue, sorted, so the doc-sync test
// and any generator iterate a stable set.
func Codes() []Code {
	codes := make([]Code, 0, len(Catalogue))
	for c := range Catalogue {
		codes = append(codes, Code(c))
	}
	sort.Slice(codes, func(i, j int) bool { return codes[i] < codes[j] })
	return codes
}

// Get returns the catalogue entry for a typed code. An unknown code falls back
// to the generic internal error so a caller always has a well-formed Error.
func Get(code Code) Error {
	return Lookup(string(code))
}

// Lookup returns the catalogue entry for code, falling back to the generic
// internal error so a handler always has a well-formed Error.
func Lookup(code string) Error {
	if e, ok := Catalogue[code]; ok {
		return e
	}
	return Catalogue[string(CodeInternal)]
}
