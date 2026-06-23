# mitos Go SDK

A thin, dependency-free (standard-library only) Go client for the standalone and
hosted mitos sandbox-server REST API. It mirrors the direct-mode surface of the
Python SDK (`sdk/python/mitos/direct.py`), the TypeScript SDK
(`sdk/typescript/src/server.ts`), the Ruby SDK (`sdk/ruby`), the Rust SDK
(`sdk/rust`), and the Java SDK (`sdk/java`): create a template, fork a sandbox,
run `exec`, list, and terminate.

The SDK uses only the Go standard library (`net/http`, `encoding/json`,
`crypto/rand`, `os`), so there are no third-party dependencies. It lives in its
own nested Go module, so importing it does NOT pull the rest of the
`mitos.run/mitos` repository (the controller, forkd, and their dependencies)
into your build.

## Scope

This SDK covers DIRECT mode only: the standalone `cmd/sandbox-server` and the
hosted control plane at `https://mitos.run`. The Kubernetes / cluster mode (the
controller, forkd, and the `mitos.run/v1` CRDs: `Sandbox`, `SandboxPool`,
`Workspace`, `WorkspaceRevision`) is served by the Python and TypeScript SDKs
and is NOT part of this module.

## Install

```bash
go get github.com/mitos-run/mitos/sdk/go
```

Import it as the `mitos` package:

```go
import mitos "github.com/mitos-run/mitos/sdk/go"
```

A branded `mitos.run/go` vanity import path is a documented follow-up: it needs a
`go-import` meta tag served from `mitos.run` (which already hosts the project
site). Until that lands, the GitHub path above resolves directly.

## Quickstart (hosted)

The base URL defaults to the hosted endpoint `https://mitos.run`. Set your API
key in the environment; it is sent as `Authorization: Bearer <key>` and is never
logged or placed in an error message.

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

Self-hosted or local standalone users opt out of the hosted default by setting
`MITOS_BASE_URL` (for example `http://localhost:8080`) or passing
`mitos.WithBaseURL(...)`. The standalone server is tokenless and ignores the
bearer header; the hosted front door verifies it.

## Surface

| Method | HTTP | Returns | Notes |
| --- | --- | --- | --- |
| `NewSandboxServer(opts ...Option)` | - | `*SandboxServer` | Functional options: `WithBaseURL`, `WithAPIKey`, `WithHTTPClient`. |
| `(*SandboxServer).CreateTemplate(ctx, id, opts...)` | `POST /v1/templates` | `*Template` | Sends a fresh `Idempotency-Key`. `WithInitWaitSeconds`, `WithTemplateIdempotencyKey`. |
| `(*SandboxServer).ListTemplates(ctx)` | `GET /v1/templates` | `[]Template` | |
| `(*SandboxServer).Fork(ctx, template, id, opts...)` | `POST /v1/fork` | `*Sandbox` | Generates a `sandbox-<hex>` id when `id` is empty; validates against the id allowlist (typed error before any request). Sends a fresh `Idempotency-Key`. `WithForkIdempotencyKey`. |
| `(*SandboxServer).ListSandboxes(ctx)` | `GET /v1/sandboxes` | `[]ServerSandbox` | |
| `(*Sandbox).Exec(ctx, command, opts...)` | `POST /v1/exec` | `*ExecResult` | Needs a Ready sandbox. `WithExecTimeout`. |
| `(*Sandbox).Terminate(ctx)` | `DELETE /v1/sandboxes/{id}` | `error` | |

Every call takes a `context.Context` for cancellation and deadlines.

Value types:

- `Template`: `ID`, `Ready`, `CreatedAt`, `CreationTimeMs`.
- `ServerSandbox`: `ID`, `TemplateID`, `Endpoint`, `CreatedAt`, `ForkTimeMs`.
- `ExecResult`: `ExitCode`, `Stdout`, `Stderr`, `ExecTimeMs`.
- `Sandbox`: `ID`, `Template`, `Endpoint`, `ForkTimeMs`.

## Auth and base-URL precedence

Resolved once when you call `NewSandboxServer`:

- Base URL: `WithBaseURL(...)`, else `MITOS_BASE_URL`, else `https://mitos.run`.
- Bearer token: `WithAPIKey(...)`, else `MITOS_API_KEY`, else the `mitos auth
  login` credential file (the `token` field of
  `~/.config/mitos/credentials.json`, honoring `MITOS_CONFIG_DIR`), else
  tokenless.

A missing, unreadable, or non-JSON credential file is NOT an error: it just
yields no token so the SDK stays usable tokenless. The path rule mirrors
`internal/credfile`, the single source of truth shared with the CLI. The token
VALUE is never logged and is redacted from any error body.

## Errors

Every non-2xx response returns a `*mitos.Error`, which parses the server
envelope `{error:{code, message, cause, remediation}}`. Branch on the code with
`errors.Is`, never on the message text:

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

## The sandbox id allowlist

Sandbox ids must match `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$` (the same allowlist the
Go daemon and the other SDKs enforce). `Fork` validates the id (the explicit one
or the generated `sandbox-<hex>`) and returns a typed `invalid_sandbox_id` error
BEFORE sending any request. `ValidSandboxID(id)` exposes the check.

## Tests

The tests use only the standard library (`net/http/httptest`, `testing`). They
spin up a stub server reproducing the sandbox-server wire shapes and assert the
SDK round-trips them, including the `Idempotency-Key` header, the credential-file
auth fallback, and token redaction in errors.

```bash
cd sdk/go
go test ./... -count=1
```

## Deferred

Not yet implemented in the Go SDK (covered by the Python / TypeScript SDKs):

- Kubernetes / cluster mode (controller, forkd, `mitos.run/v1` CRDs).
- The files API (`/v1/files/*`).
- Interactive PTY (`/v1/pty`).
- `run_code`: the server exposes a streaming-only route
  (`POST /v1/run_code/stream`); a synchronous Go wrapper is deferred until a
  non-streaming contract exists.
- Per-sandbox network posture, `set_timeout`, `pause` / `resume`, and
  `get_host(port)` preview URLs.
- The branded `mitos.run/go` vanity import path (needs a `go-import` meta tag on
  `mitos.run`).
