# mitos Rust SDK

A thin Rust client for the standalone and hosted mitos sandbox-server REST API.
It mirrors the direct-mode surface of the Python SDK
(`sdk/python/mitos/direct.py`), the TypeScript SDK
(`sdk/typescript/src/server.ts`), and the Ruby SDK
(`sdk/ruby/lib/mitos/sandbox_server.rb`): create a template, fork a sandbox, run
`exec`, and terminate.

The crate is blocking (no async runtime) and keeps a small dependency tree:
`ureq` for HTTP, `serde` / `serde_json` for JSON, and `getrandom` for the
idempotency key and the generated sandbox id. It targets Rust 1.74 and later.

## Scope

This crate covers DIRECT mode only: the standalone `cmd/sandbox-server` and the
hosted control plane at `https://mitos.run`. The Kubernetes / cluster mode (the
controller, forkd, and the `mitos.run/v1` CRDs: `Sandbox`, `SandboxPool`,
`Workspace`, `WorkspaceRevision`) is served by the Python and TypeScript SDKs
only and is NOT part of this crate.

## Install

Not yet published to crates.io. Add it as a git or path dependency:

```toml
# Cargo.toml, git dependency
[dependencies]
mitos = { git = "https://github.com/mitos-run/mitos", branch = "main", package = "mitos" }

# or, from a local checkout
[dependencies]
mitos = { path = "path/to/mitos/sdk/rust" }
```

## Quickstart (hosted)

The base URL defaults to the hosted endpoint `https://mitos.run`. Set your API
key in the environment; it is sent as `Authorization: Bearer <key>` and is never
logged.

```bash
export MITOS_API_KEY="sk-..."     # or pass .api_key(...) to the builder
```

```rust
use mitos::SandboxServer;

fn main() -> Result<(), mitos::MitosError> {
    // Base URL + API key resolved from the environment (explicit args override).
    let server = SandboxServer::new();
    server.create_template("python")?;          // build (or get) the template
    let sandbox = server.fork("python")?;        // fork a fresh sandbox

    let result = sandbox.exec("echo hello")?;
    println!("{}", result.exit_code);            // 0
    print!("{}", result.stdout);                 // "hello\n"

    sandbox.terminate()?;
    Ok(())
}
```

Point at a local standalone server by setting `MITOS_BASE_URL`, or with the
builder:

```rust
use mitos::SandboxServer;

let server = SandboxServer::builder()
    .base_url("http://localhost:8080")
    .build();
```

`exec` requires a Ready sandbox: the sandbox-server routes exec through the guest
agent over vsock, so calling `exec` on a sandbox that is not yet up returns a
typed `not_found` error.

## Auth and base-URL precedence

| Setting | Precedence |
| --- | --- |
| Base URL | `.base_url(...)` argument, else `MITOS_BASE_URL`, else `https://mitos.run`. The trailing slash is trimmed. |
| Bearer token | `.api_key(...)` argument, else `MITOS_API_KEY`, else the CLI login credential file, else none (tokenless). |

The credential file is `~/.config/mitos/credentials.json` (honoring
`MITOS_CONFIG_DIR`); only its `"token"` field is read. A missing, unreadable, or
non-JSON credential file is never an error: resolution simply falls through to
tokenless. The token is sent as `Authorization: Bearer <key>`; the standalone
server is tokenless and ignores it, while the hosted front door verifies it. The
token VALUE is never logged and is redacted from any error.

## Surface

| Method | HTTP | Returns | Notes |
| --- | --- | --- | --- |
| `SandboxServer::new()` / `SandboxServer::builder()` | - | `SandboxServer` | Resolves base URL + API key per the precedence above. |
| `SandboxServer::create_template(id)` | `POST /v1/templates` | `Template` | `init_wait_seconds` defaults to 5. Sends a fresh `Idempotency-Key`. |
| `SandboxServer::create_template_opts(id, init_wait_seconds, idempotency_key)` | `POST /v1/templates` | `Template` | Explicit wait and optional caller key. |
| `SandboxServer::list_templates()` | `GET /v1/templates` | `Vec<Template>` | A `null` body maps to an empty vec. |
| `SandboxServer::fork(template)` | `POST /v1/fork` | `Sandbox` | Generates a `sandbox-<hex>` id. Sends a fresh `Idempotency-Key`. |
| `SandboxServer::fork_as(template, id)` | `POST /v1/fork` | `Sandbox` | Explicit id; validated against the allowlist before any request. |
| `SandboxServer::fork_opts(template, id, idempotency_key)` | `POST /v1/fork` | `Sandbox` | Optional id and caller key. |
| `SandboxServer::list_sandboxes()` | `GET /v1/sandboxes` | `Vec<ServerSandbox>` | |
| `Sandbox::exec(command)` | `POST /v1/exec` | `ExecResult` | Default 30 s timeout. Needs a Ready sandbox. |
| `Sandbox::exec_with_timeout(command, timeout)` | `POST /v1/exec` | `ExecResult` | Explicit timeout in seconds. |
| `Sandbox::terminate()` | `DELETE /v1/sandboxes/{id}` | `()` | |

Value types:

- `Template`: `id`, `ready`, `created_at`, `creation_time_ms`.
- `ServerSandbox`: `id`, `template_id`, `endpoint`, `created_at`, `fork_time_ms`.
- `ExecResult`: `exit_code`, `stdout`, `stderr`, `exec_time_ms`, plus `success()`.
- `Sandbox`: `id`, `template_id`, `endpoint`, `fork_time_ms`.

## Errors

Every method returns `Result<T, MitosError>`. On a non-2xx response the SDK
parses the server envelope `{error:{code, message, cause, remediation}}` and
falls back to status-derived defaults for an older or non-mitos server. Branch on
`code`, never on the message text:

```rust
match sandbox.exec("echo hi") {
    Ok(result) => println!("{}", result.stdout),
    Err(e) => {
        eprintln!("{}", e.code);         // e.g. "not_found"
        eprintln!("{}", e.status);       // e.g. 404
        eprintln!("{}", e.remediation);  // actionable hint
    }
}
```

The configured API key value is redacted from any error body before it becomes
the error cause, and never appears in the `Display` output.

## The sandbox id allowlist

Sandbox ids must match `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$` (the same allowlist the
Go daemon, the Python SDK, the TypeScript SDK, and the Ruby SDK enforce). `fork`
and `terminate` validate the id and return a typed `invalid_sandbox_id` error
before sending any request. Use `mitos::valid_sandbox_id` to check an id yourself.

## Tests

The tests use only the standard library: an in-process HTTP/1.1 stub bound to a
loopback ephemeral port (`std::net::TcpListener`) reproduces the sandbox-server
wire shapes and the SDK round trips them. No network access is needed.

```bash
cd sdk/rust
cargo test
cargo fmt --check
cargo clippy --all-targets -- -D warnings
```

## Deferred

Not yet implemented in the Rust SDK (covered by the Python / TypeScript SDKs
where noted):

- Kubernetes / cluster mode (controller, forkd, `mitos.run/v1` CRDs): Python and TypeScript only.
- The files API (`/v1/files/*`).
- Interactive PTY (`/v1/pty`).
- `run_code`: the server exposes a streaming-only route
  (`POST /v1/run_code/stream`); a synchronous Rust wrapper is deferred until a
  non-streaming contract exists.
- Per-sandbox network posture, `set_timeout`, `pause` / `resume`, and
  `get_host(port)` preview URLs.
- crates.io publishing.
