# Mitos Go SDK

Mitos gives AI agents isolated, forkable sandboxes: Firecracker microVMs that
restore from snapshots and fork into parallel attempts, so an agent can branch a
warm environment instead of rebuilding it. Run it fully hosted at
[https://mitos.run](https://mitos.run) or self-hosted on your own Kubernetes
cluster.

This is the Go client for the direct sandbox API: create a template, fork a
sandbox, run `exec`, list, and terminate. It uses only the Go standard library
(`net/http`, `encoding/json`, `crypto/rand`, `os`), so there are no third-party
dependencies. It lives in its own nested Go module, so importing it does not pull
the rest of the repository (the controller, forkd, and their dependencies) into
your build.

## Install

```bash
go get github.com/mitos-run/mitos/sdk/go
```

Import it as the `mitos` package:

```go
import mitos "github.com/mitos-run/mitos/sdk/go"
```

## Quickstart (hosted)

Get an API key from [https://mitos.run](https://mitos.run) and set it in the
environment. The base URL defaults to the hosted endpoint. The key is sent as
`Authorization: Bearer <key>` and is never logged.

```bash
export MITOS_API_KEY="sk-..."
```

```go
package main

import (
	"context"
	"fmt"
	"log"

	mitos "github.com/mitos-run/mitos/sdk/go"
)

func main() {
	ctx := context.Background()
	srv := mitos.NewSandboxServer() // base URL + token resolved from the env

	if _, err := srv.CreateTemplate(ctx, "python"); err != nil {
		log.Fatal(err)
	}
	sb, err := srv.Fork(ctx, "python", "") // empty id -> generated sandbox-<hex>
	if err != nil {
		log.Fatal(err)
	}
	defer sb.Terminate(ctx)

	res, err := sb.Exec(ctx, "echo hi")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.Stdout)
}
```

`Fork` is the snapshot-fork primitive: each call forks a warm template into a
fresh, independent sandbox, so parallel attempts start from the same state.

Point at a local standalone server by setting `MITOS_BASE_URL` (for example
`http://localhost:8080`) or passing `mitos.WithBaseURL(...)`.

## Surface

| Method | HTTP | Returns |
| --- | --- | --- |
| `NewSandboxServer(opts ...Option)` | none | `*SandboxServer` |
| `(*SandboxServer).CreateTemplate(ctx, id, opts...)` | `POST /v1/templates` | `*Template` |
| `(*SandboxServer).ListTemplates(ctx)` | `GET /v1/templates` | `[]Template` |
| `(*SandboxServer).Fork(ctx, template, id, opts...)` | `POST /v1/fork` | `*Sandbox` |
| `(*SandboxServer).ListSandboxes(ctx)` | `GET /v1/sandboxes` | `[]ServerSandbox` |
| `(*Sandbox).Exec(ctx, command, opts...)` | `POST /v1/exec` | `*ExecResult` |
| `(*Sandbox).Terminate(ctx)` | `DELETE /v1/sandboxes/{id}` | `error` |

Functional options: `WithBaseURL`, `WithAPIKey`, `WithHTTPClient`,
`WithInitWaitSeconds`, `WithTemplateIdempotencyKey`, `WithForkIdempotencyKey`,
`WithExecTimeout`. Creating calls send a fresh `Idempotency-Key` so a retry is
de-duplicated by the server. `Fork` generates a `sandbox-<hex>` id when `id` is
empty. Every call takes a `context.Context` for cancellation and deadlines.

Value types:

- `Template`: `ID`, `Ready`, `CreatedAt`, `CreationTimeMs`.
- `ServerSandbox`: `ID`, `TemplateID`, `Endpoint`, `CreatedAt`, `ForkTimeMs`.
- `ExecResult`: `ExitCode`, `Stdout`, `Stderr`, `ExecTimeMs`.
- `Sandbox`: `ID`, `Template`, `Endpoint`, `ForkTimeMs`.

## Auth and base URL precedence

Resolved once when you call `NewSandboxServer`:

- Base URL: `WithBaseURL(...)`, else `MITOS_BASE_URL`, else `https://mitos.run`.
- Bearer token: `WithAPIKey(...)`, else `MITOS_API_KEY`, else the `mitos auth
  login` credential file (the `token` field of
  `~/.config/mitos/credentials.json`, honoring `MITOS_CONFIG_DIR`), else
  tokenless.

A missing, unreadable, or non-JSON credential file is never an error: resolution
falls through to tokenless. The token is sent as `Authorization: Bearer <key>`;
the standalone server ignores it, the hosted endpoint verifies it. The token
value is never logged and is redacted from any error body.

## Errors

Every non-2xx response returns a `*mitos.Error`, which parses the server envelope
`{error:{code, message, cause, remediation}}`. Branch on the code with
`errors.Is` / `errors.As`, never on the message text.

```go
res, err := sb.Exec(ctx, "false")
if err != nil {
	var e *mitos.Error
	if errors.As(err, &e) {
		fmt.Println(e.Code)        // e.g. "not_found"
		fmt.Println(e.Status)      // e.g. 404
		fmt.Println(e.Remediation) // actionable hint
	}
	// or test a specific code directly:
	if errors.Is(err, &mitos.Error{Code: "not_found"}) {
		// ...
	}
}
```

The configured API key is redacted from any error body before it becomes the
error cause, so a token a hostile or misconfigured server reflects back never
surfaces in `err.Error()`.

## Sandbox ids

Sandbox ids must match `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$` (the same allowlist
every Mitos SDK enforces). `Fork` validates the id (the explicit one or the
generated `sandbox-<hex>`) and returns a typed `invalid_sandbox_id` error before
sending any request. `ValidSandboxID(id)` exposes the check.

## Tests

The tests use only the standard library (`net/http/httptest`, `testing`). They
spin up a stub server reproducing the sandbox-server wire shapes and assert the
SDK round-trips them, including the `Idempotency-Key` header, the credential-file
auth fallback, and token redaction in errors.

```bash
cd sdk/go
go test ./... -count=1
```

## Scope

This module is direct-mode only. Cluster mode (driving the Kubernetes CRDs) is
served by the Python and TypeScript SDKs. Beyond the create / fork / exec / list
/ terminate surface above, the following direct-mode endpoints are not part of
this module: the files API (`/v1/files/*`), interactive PTY (`/v1/pty`),
`run_code`, per-sandbox network posture, `set_timeout`, `pause` / `resume`, and
`get_host(port)` preview URLs.

## The Mitos SDK family

Mitos ships native clients in six languages. All of them share the same
direct-mode surface (create a template, fork, exec, terminate), so the API maps
1:1 across languages; cluster mode (driving the Kubernetes CRDs) is Python and
TypeScript only.

| Language | Install | Covers |
| --- | --- | --- |
| Python | `pip install mitos-run` | direct + cluster + async |
| TypeScript | `npm install @mitos/sdk` | direct + cluster |
| Ruby | `gem install mitos` | direct |
| Rust | `cargo add mitos` | direct |
| Go | `go get github.com/mitos-run/mitos/sdk/go` | direct |
| Java | build from source | direct |

Project home: [https://mitos.run](https://mitos.run). Source and all six SDKs:
[github.com/mitos-run/mitos](https://github.com/mitos-run/mitos).

## License

Apache-2.0.
</content>
