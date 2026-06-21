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
	// CodeExecFailed: the command could not be executed in the sandbox.
	CodeExecFailed Code = "exec_failed"
	// CodeFileFailed: a file operation failed in the sandbox.
	CodeFileFailed Code = "file_failed"
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
