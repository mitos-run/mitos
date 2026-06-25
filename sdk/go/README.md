# Mitos Go SDK

Mitos gives AI agents isolated, forkable sandboxes: Firecracker microVMs that
restore from snapshots and fork into parallel attempts, so an agent can branch a
warm environment instead of rebuilding it. Run it fully hosted at
[https://mitos.run](https://mitos.run) or self-hosted on your own Kubernetes
cluster.

This is the Go client for both surfaces:

- **Direct mode** (`SandboxServer`): the standalone or hosted sandbox API, create
  a template, fork a sandbox, run `exec`, list, and terminate.
- **Cluster mode** (`AgentRun`): drive the `mitos.run/v1` CRDs (`SandboxPool`,
  `Sandbox`, `Workspace`) directly on a Kubernetes cluster, the same surface the
  Python and TypeScript SDKs expose.

It uses only the Go standard library (`net/http`, `crypto/tls`, `encoding/json`,
`crypto/rand`, `os`), so there are no third-party dependencies: cluster mode is a
minimal Kubernetes REST client, not `k8s.io/client-go`. It lives in its own
nested Go module, so importing it does not pull the rest of the repository (the
controller, forkd, and their dependencies) into your build.

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
| `(*Sandbox).Exec(ctx, command, opts...)` | Connect `Sandbox/ExecStream` | `*ExecResult` |
| `(*Sandbox).ExecStream(ctx, command, opts...)` | Connect `Sandbox/ExecStream` | `*ExecStream` |
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
- `ExecStream`: `Recv() (*ExecChunk, error)` (`io.EOF` at exit), `Result() ExecResult`, `Close() error`.
- `ExecChunk`: `Stdout`, `Stderr` (`[]byte`; exactly one is set per chunk).
- `Sandbox`: `ID`, `Template`, `Endpoint`, `ForkTimeMs`.

## Streaming exec

`Exec` runs over the Connect `sandbox.v1.Sandbox` runtime protocol (issue #24) and
returns the buffered result. For live output, `ExecStream` yields stdout/stderr
chunks as they are produced. The Go SDK speaks Connect over the standard library
alone (no gRPC runtime, no codegen), so it stays dependency-free.

```go
stream, err := sb.ExecStream(ctx, "for i in 1 2 3; do echo line $i; sleep 1; done")
if err != nil {
	log.Fatal(err)
}
defer stream.Close()

for {
	chunk, err := stream.Recv()
	if errors.Is(err, io.EOF) {
		break
	}
	if err != nil {
		log.Fatal(err) // a *mitos.Error on a sandbox-side failure
	}
	os.Stdout.Write(chunk.Stdout)
	os.Stderr.Write(chunk.Stderr)
}
fmt.Printf("exit %d\n", stream.Result().ExitCode)
```

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

## Cluster mode (Kubernetes)

`AgentRun` is the operator path: it drives the `mitos.run/v1` CRDs directly, so a
sandbox is born from a `SandboxPool` (or forked from another sandbox) and the
controller drives it to `Ready`. This is the Go analogue of the Python and
TypeScript `AgentRun`.

```go
ctx := context.Background()

// In a pod: mitos.WithInCluster(). Locally: mitos.WithKubeconfig("") loads
// $KUBECONFIG, then ~/.kube/config.
ar, err := mitos.NewAgentRun(mitos.WithNamespace("agents"), mitos.WithInCluster())
if err != nil {
	log.Fatal(err)
}

// One-liner: ensure the default pool for the image exists, then start a sandbox.
sb, err := ar.Sandbox(ctx, "python:3.12")
if err != nil {
	log.Fatal(err)
}
defer sb.Terminate(ctx)

// Or claim from an explicit pool with env, a TTL, and a workspace binding.
sb2, err := ar.Create(ctx,
	mitos.WithPool("my-pool"),
	mitos.WithEnv(map[string]string{"FOO": "bar"}),
	mitos.WithTTL("30m"),
	mitos.WithWorkspace("ws-a"),
)

// Reconnect to a running sandbox by name across processes.
again, err := ar.FromName(ctx, sb.Name)

// Inspect pool capacity.
ps, err := ar.PoolStatus(ctx, "my-pool")
fmt.Println(ps.ReadySnapshots, ps.Desired)
```

| Method | CRD op | Returns |
| --- | --- | --- |
| `NewAgentRun(opts ...AgentRunOption)` | none | `*AgentRun` |
| `(*AgentRun).Sandbox(ctx, image, opts...)` | get-or-create `SandboxPool`, create `Sandbox` | `*ClusterSandbox` |
| `(*AgentRun).Create(ctx, opts...)` | create `Sandbox` (`spec.source.poolRef`) | `*ClusterSandbox` |
| `(*AgentRun).Get(ctx, name)` / `FromName(ctx, name)` | get `Sandbox` | `*ClusterSandbox` |
| `(*AgentRun).List(ctx, pool)` | list `Sandbox`, filter by pool | `[]*ClusterSandbox` |
| `(*AgentRun).PoolStatus(ctx, name)` | get `SandboxPool` status | `*PoolStatus` |
| `(*AgentRun).CreateWorkspace(ctx, name)` | create `Workspace` | `*Workspace` |
| `(*AgentRun).Workspace(name)` | lazy handle | `*Workspace` |
| `(*AgentRun).GetWorkspace(ctx, name)` | get `Workspace` (404 -> `workspace_not_found`) | `*Workspace` |
| `(*AgentRun).ListWorkspaces(ctx)` | list `Workspace` | `[]*Workspace` |
| `(*ClusterSandbox).Terminate(ctx)` | delete `Sandbox` | `error` |

Connection options: `WithInCluster()` (service-account mount), `WithKubeconfig(path)`
(current context; empty path uses `$KUBECONFIG` then `~/.kube/config`),
`WithNamespace(ns)`, and `WithAllowDefaultPool(bool)`. The kubeconfig parser
supports a common subset (server, CA, bearer token, or client cert/key for
mutual-TLS clusters like kind and minikube); it does not support exec credential
plugins or `auth-provider` blocks. Per-sandbox bearer tokens read from the
`<name>-sandbox-token` Secret are held in memory only and never logged.

`DefaultPoolName(image)` is exported and computes the default-pool slug
identically to the Python and TypeScript SDKs (lowercase, `/` and `:` to `-`,
other unsafe characters collapsed, bounded to 40 characters, prefixed
`mitos-default-`).

## Scope

Beyond the direct-mode create / fork / exec / list / terminate surface and the
cluster-mode surface above, the following direct-mode endpoints are not yet part
of this module: the files API (`/v1/files/*`), interactive PTY (`/v1/pty`),
`run_code`, per-sandbox network posture, `set_timeout`, `pause` / `resume`, and
`get_host(port)` preview URLs.

## The Mitos SDK family

Mitos ships native clients in six languages. All of them share the same
direct-mode surface (create a template, fork, exec, terminate), so the API maps
1:1 across languages; cluster mode (driving the Kubernetes CRDs) ships in Python,
TypeScript, and Go today and is planned for the rest, for full parity (#296).

| Language | Install | Covers |
| --- | --- | --- |
| Python | `pip install mitos-run` | direct + cluster + async |
| TypeScript | `npm install @mitos/sdk` | direct + cluster |
| Go | `go get github.com/mitos-run/mitos/sdk/go` | direct + cluster |
| Ruby | `gem install mitos` | direct |
| Rust | `cargo add mitos` | direct |
| Java | build from source | direct |

Project home: [https://mitos.run](https://mitos.run). Source and all six SDKs:
[github.com/mitos-run/mitos](https://github.com/mitos-run/mitos).

## License

Apache-2.0.
</content>
