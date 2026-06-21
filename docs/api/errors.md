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
| `exec_failed` | The command could not be executed in the sandbox. | Inspect the cause; if it is a transport error retry, otherwise check the forkd logs for the guest agent state. | 500 | `Internal` | `sandbox` |
| `file_failed` | A file operation failed in the sandbox. | Confirm the path exists and is writable; inspect the cause for the underlying error. | 500 | `Internal` | `sandbox`, `path` |
| `internal` | An unclassified internal error occurred. | Retry the request; if it persists, inspect the forkd or sandbox-server logs. | 500 | `Internal` | (none) |
| `invalid_json` | The request body is not valid JSON. | Send a JSON body matching the sandbox API contract for this endpoint. | 400 | `InvalidArgument` | (none) |
| `not_found` | No such sandbox. | Confirm the sandbox id exists and is Ready before calling. | 404 | `NotFound` | `sandbox` |
| `too_many_streams` | The sandbox is at its concurrent-stream limit. | Close an existing streaming exec, run_code, or PTY session for this sandbox before opening another, or raise the forkd --max-streams-per-sandbox ceiling. Existing streams are unaffected. | 429 | `ResourceExhausted` | `sandbox` |
| `unauthorized` | The per-sandbox bearer token is missing or invalid. | Send Authorization: Bearer <token> with the per-sandbox token from the <name>-sandbox-token Secret. | 401 | `Unauthenticated` | (none) |

Rows are sorted by `code` to match `internal/apierr` `Codes()` ordering so a
reviewer can diff the doc against the constants by eye.

### Context field meanings

- `sandbox`: the sandbox id the call targeted.
- `path`: the in-sandbox file path for a `file_failed` error.

A `context` map never contains a secret value, a bearer token, or a credential;
only ids, paths, and operation names.

## SDK behavior

Both SDKs parse the envelope into a structured error so an agent can branch on
`code` without string-matching a message:

- Python: `mitos.errors.AgentRunError` (`code`, `cause`, `remediation`,
  `status`, `context`).
- TypeScript: `AgentRunError` (`code`, `errorCause`, `remediation`).

Both redact any echo of the bearer token from a server body before it becomes
the error `cause`, so a reflected token never surfaces in a message, log, or
thrown value. When a server returns a non-2xx response WITHOUT the envelope (a
proxy 502, a raw transport failure), the SDK synthesizes a code from the HTTP
status (for example `unavailable` for 503, `rate_limited` for 429) and a
status-derived remediation, so a caller still gets `code` + `remediation`. Those
synthesized codes are SDK-local fallbacks for non-enveloped responses; the codes
in the table above are the ones the mitos servers actually emit.

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
| A claim stuck not Ready | `SandboxClaim.status.conditions` | `docs/conditions.md` (`NoCapacity`, `NoHuskPod`, `ActivateFailed`, ...) |
| `unauthorized` on a call | sandbox API / SDK | this catalogue; the per-sandbox token is wrong |
| A fork rejected for secrets | `SandboxFork.status.conditions` | `docs/conditions.md` (`SecretInheritanceDenied`) |

When a control-plane condition explains a runtime failure (a claim that never
went Ready is why a later call returns `not_found`), the remediation here points
the agent at the condition reason, and the condition's operator-action table in
`docs/conditions.md` points back. The two catalogues are maintained together.

## Machine-readable forms

- JSON Schema for the envelope: `docs/api/error-schema.json`.
- Agent-facing index and in-context examples: `llms.txt` at the repo root, which
  references the codes in this table verbatim.
