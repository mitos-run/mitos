# Error-code catalogue (normative)

This is the NORMATIVE catalogue of the runtime error `code`s the forkd sandbox
API, the standalone sandbox-server, the guest agent, and both SDKs can emit. It
is the single source of truth for the LLM-legible error contract of
`docs/api/v2-spec.md` section 2.3.

The PRIMARY reader of every runtime error is a language model. Every error
carries a stable machine `code`, a one-sentence `cause`, an actionable
`remediation`, and structured `context`; never a bare code. The wire shape is
the envelope:

```json
{
  "error": {
    "code": "not_found",
    "message": "no such sandbox",
    "cause": "no sandbox registered for id sb-7",
    "remediation": "Confirm the sandbox id exists and is Ready before calling.",
    "context": { "sandbox": "sb-7" }
  }
}
```

`code` and `remediation` are always populated. `message` is the catalogue's
one-line summary; `cause` is the call-site detail (sandbox ids, paths, operation
names only, never a secret value); `context` is optional structured fields.

This catalogue and the typed Go constants in `internal/apierr` cannot drift:
the constants are the source of truth, `internal/apierr` `Codes()` enumerates
them, and `TestDocCatalogueIsInSyncWithCode` fails the build if a code here is
missing from the table or a table row names a code the catalogue does not
define. `TestEveryCatalogueEntryHasRemediation` plus the CI lint
(`hack/check-apierr-remediation.sh`, wired into the `go-lint` job) are the
STATIC guarantee that no error path, even an unexercised one, can ship without a
non-empty `remediation`.

This catalogue is JOINT with the controller condition reason-code catalogue in
`docs/conditions.md`: those reason codes are the asynchronous control-plane
reasons surfaced on CRD `status.conditions`; the codes below are the synchronous
runtime errors on the sandbox API and SDK call paths. They do not overlap and do
not contradict; an agent reads conditions for "why is my claim not Ready" and
reads these for "why did my exec/file/run_code call fail". See the
cross-reference table at the end.

## Runtime error codes

`Status` is the HTTP status the forkd sandbox API and sandbox-server send and the
gRPC status the controller<->forkd path maps to (the gRPC mapping column). The
`context` fields are the structured keys a caller may rely on for that code.

| Code | Cause (one sentence) | Remediation | HTTP | gRPC | Context fields |
| --- | --- | --- | --- | --- | --- |
| `body_too_large` | The request body exceeds the server size limit. | Reduce the payload; file content is hex-encoded and bounded by the server. | 413 | `ResourceExhausted` | (none) |
| `build_failed` | A declarative template build step failed, so no snapshot was produced. | A declarative build step failed; no snapshot was produced. The context names the failing step (step index and step_kind) and carries its cause. Fix that step and rebuild; cached steps before it are reused. | 422 | `FailedPrecondition` | `step`, `step_kind` |
| `budget_exhausted` | A budget-gated self-service operation (fork, checkpoint, extend-lifetime) was refused because the sandbox capability budget for that dimension is spent. | This is a creator-set capability budget; the sandbox cannot widen its own. Request a larger budget from the orchestrator or operator that created this sandbox (raise spec.budget on the parent Sandbox), or proceed within the remaining budget reported by the Budget call. The context names the exhausted dimension and the remaining allowance. | 403 | `PermissionDenied` | `sandbox`, `dimension`, `remaining` |
| `canceled` | The request was canceled by the caller before it completed. | The request was canceled by the caller (the client closed the connection or canceled the context). Retry the call if the cancellation was not intentional. | 499 | `Canceled` | `sandbox` |
| `exec_failed` | The command could not be executed in the sandbox. | Inspect the cause; if it is a transport error retry, otherwise check the forkd logs for the guest agent state. | 500 | `Internal` | `sandbox` |
| `exec_timeout` | A command or run_code execution ran past its requested timeout and was terminated. | The command exceeded its execution deadline and was killed. Raise the timeout on the exec or run_code call, or split the work into shorter steps. The context carries the timeout_s that was hit. | 504 | `DeadlineExceeded` | `sandbox`, `timeout_s` |
| `file_failed` | A file operation failed in the sandbox. | Confirm the path exists and is writable; inspect the cause for the underlying error. | 500 | `Internal` | `sandbox`, `path` |
| `forbidden` | The customer API key is valid but is not permitted for this action: it lacks the required scope, or the targeted resource belongs to a different organization (the public gateway org-isolation refusal). | The API key is valid but cannot perform this action: it either lacks the required scope or targets a resource owned by a different organization. Use a key for the owning organization with the required scope. | 403 | `PermissionDenied` | `op` |
| `idle_timeout` | The sandbox was reaped after exceeding its idle timeout, so the call hit a sandbox that is no longer running. | The sandbox idled out and was reaped; create a fresh sandbox (or fork from a checkpoint) and retry. Raise the idle timeout on the parent Sandbox or call set_timeout to keep an idle sandbox alive longer. | 410 | `FailedPrecondition` | `sandbox` |
| `internal` | An unclassified internal error occurred. | Retry the request; if it persists, inspect the forkd or sandbox-server logs. | 500 | `Internal` | (none) |
| `invalid_input` | The request body is syntactically valid JSON but fails a semantic business rule (negative value, ordering constraint, missing required field). | Correct the value in the request body; consult the endpoint documentation for the valid range and business rules. | 400 | `InvalidArgument` | (none) |
| `invalid_json` | The request body is not valid JSON. | Send a JSON body matching the sandbox API contract for this endpoint. | 400 | `InvalidArgument` | (none) |
| `not_found` | No such sandbox. | Confirm the sandbox id exists and is Ready before calling. | 404 | `NotFound` | `sandbox` |
| `quota_exceeded` | The customer organization exceeded a hosted-plan quota (concurrent sandboxes, monthly usage, or a rate ceiling) enforced by the public gateway, distinct from the per-sandbox budget_exhausted and rate_limited. | The organization hit a hosted-plan quota (concurrent sandboxes, monthly usage, or a rate ceiling). Reduce concurrency or wait for the window to reset, or raise the plan limit for this organization. The context names the exceeded dimension and the limit. | 429 | `ResourceExhausted` | `op`, `dimension`, `limit` |
| `rate_limited` | The caller exceeded the request rate limit for this sandbox or endpoint. | Back off and retry after the delay in the context retry_after_ms; this is a per-window request-rate limit, distinct from too_many_streams (the concurrent-stream ceiling). | 429 | `ResourceExhausted` | `sandbox`, `retry_after_ms` |
| `timeout_too_large` | The requested timeout exceeds the server ceiling. | Request a timeout at or below the ceiling reported in the context (max_timeout_s); the server never silently reduces a requested timeout, it rejects it so the deadline you set is the deadline you get. | 400 | `InvalidArgument` | `sandbox`, `requested_s`, `max_timeout_s` |
| `too_many_streams` | The sandbox is at its concurrent-stream limit. | Close an existing streaming exec, run_code, or PTY session for this sandbox before opening another, or raise the forkd --max-streams-per-sandbox ceiling. Existing streams are unaffected. | 429 | `ResourceExhausted` | `sandbox` |
| `unauthorized` | The per-sandbox bearer token is missing or invalid. | Send Authorization: Bearer <token> with the per-sandbox token from the <name>-sandbox-token Secret. | 401 | `Unauthenticated` | (none) |

Rows are sorted by `code` to match `internal/apierr` `Codes()` ordering so a
reviewer can diff the doc against the constants by eye.

### Context field meanings

- `sandbox`: the sandbox id the call targeted.
- `path`: the in-sandbox file path for a `file_failed` error.
- `timeout_s`: the execution deadline (seconds) an `exec_timeout` hit.
- `requested_s`: the timeout (seconds) the caller asked for on a `timeout_too_large` error.
- `max_timeout_s`: the server ceiling (seconds) on a `timeout_too_large` error.
- `retry_after_ms`: the back-off delay (milliseconds) a `rate_limited` caller should wait.

A `context` map never contains a secret value, a bearer token, or a credential;
only ids, paths, and operation names.

## Timeout determinism and the timeout family

A requested timeout is HONORED or REJECTED, never silently reduced.
The four timeout/cancel codes are discriminable so a caller can tell them apart
without parsing a message:

- `timeout_too_large` (400): the requested exec/run_code timeout exceeds the
  server ceiling. The server rejects it with `requested_s` and `max_timeout_s`
  in the context; it never clamps the value down behind the caller's back.
- `exec_timeout` (504): the command ran past its requested (honored) timeout and
  was terminated. This is the execution deadline, with `timeout_s` in context.
- `idle_timeout` (410): the sandbox was reaped for inactivity, so a later call
  hit a sandbox that is gone. Distinct from `not_found` (never existed).
- `canceled` (499): the caller hung up or canceled the request context before it
  completed.

The exec/run_code timeout ceiling is the forkd / sandbox-server
`--max-exec-timeout-seconds` flag, default 86400 (24 hours). A request that omits
a timeout takes the per-endpoint default (exec 30s, run_code 60s); a request at
or under the ceiling is honored exactly; a request over the ceiling is rejected
with `timeout_too_large`. The SDKs validate the same ceiling client-side so an
over-ceiling timeout is rejected before the request is sent, and map the server
codes to typed exceptions (see below).

## SDK behavior

Both SDKs parse the envelope into a structured error so an agent can branch on
`code` without string-matching a message:

- Python: `mitos.errors.AgentRunError` (`code`, `cause`, `remediation`,
  `status`, `context`).
- TypeScript: `AgentRunError` (`code`, `errorCause`, `remediation`).

Both SDKs additionally raise a TYPED subclass per code so a caller branches on
the exception TYPE, never on the message. The factory parses the envelope and
selects the subclass; an unknown code falls back to the base type. The mapping:

| Code | Python type | TypeScript type |
| --- | --- | --- |
| `idle_timeout` | `IdleTimeoutError` | `IdleTimeoutError` |
| `exec_timeout` | `ExecutionDeadlineError` | `ExecutionDeadlineError` |
| `canceled` | `RequestCanceledError` | `RequestCanceledError` |
| `timeout_too_large` | `TimeoutTooLargeError` | `TimeoutTooLargeError` |
| `rate_limited` | `RateLimitedError` | `RateLimitedError` |
| `not_found` | `NotFoundError` | `NotFoundError` |
| `unauthorized` | `UnauthorizedError` | `UnauthorizedError` |

The execution-deadline case is also reached when an `exec` returns the
conventional timeout exit code 124: the SDKs raise `ExecutionDeadlineError` so
"the command timed out" is a typed branch, not an exit-code comparison.

Both redact any echo of the bearer token from a server body before it becomes
the error `cause`, so a reflected token never surfaces in a message, log, or
thrown value. When a server returns a non-2xx response WITHOUT the envelope (a
proxy 502, a raw transport failure), the SDK synthesizes a code from the HTTP
status (for example `unavailable` for 503, `rate_limited` for 429) and a
status-derived remediation, so a caller still gets `code` + `remediation`. Those
synthesized codes are SDK-local fallbacks for non-enveloped responses; the codes
in the table above are the ones the Mitos servers actually emit.

## gRPC status details

The controller<->forkd gRPC path (`internal/daemon`) returns `google.golang.org/grpc/status`
codes; the `gRPC` column above is the mapping a gateway uses to translate a gRPC
failure into the HTTP envelope. Authn/authz on the gRPC channel is mTLS +
identity (`internal/daemon/authz.go`): a missing client certificate is
`Unauthenticated` and a wrong identity is `PermissionDenied`; those are
transport-level and map to the `unauthorized` envelope at the HTTP boundary.

## Cross-reference to controller conditions

`docs/conditions.md` is the catalogue of asynchronous control-plane reason codes
on CRD `status.conditions`. They are JOINT with this catalogue, not duplicative:

| You see | Where | Read |
| --- | --- | --- |
| `not_found` on an exec/file call | sandbox API / SDK | this catalogue; the sandbox is gone or never became Ready |
| A claim stuck not Ready | `Sandbox.status.conditions` | `docs/conditions.md` (`NoCapacity`, `NoHuskPod`, `ActivateFailed`, ...) |
| `unauthorized` on a call | sandbox API / SDK | this catalogue; the per-sandbox token is wrong |
| A fork rejected for secrets | `Sandbox.status.conditions` | `docs/conditions.md` (`SecretInheritanceDenied`) |

When a control-plane condition explains a runtime failure (a claim that never
went Ready is why a later call returns `not_found`), the remediation here points
the agent at the condition reason, and the condition's operator-action table in
`docs/conditions.md` points back. The two catalogues are maintained together.

## Machine-readable forms

- JSON Schema for the envelope: `docs/api/error-schema.json`.
- Agent-facing index and in-context examples: `llms.txt` at the repo root, which
  references the codes in this table verbatim.
