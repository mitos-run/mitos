# Mitos Rust SDK

Mitos gives AI agents isolated, forkable sandboxes: Firecracker microVMs that
restore from snapshots and fork into parallel attempts, so an agent can branch a
warm environment instead of rebuilding it. Run it fully hosted at
[https://mitos.run](https://mitos.run) or self-hosted on your own Kubernetes
cluster.

This is the Rust client for the direct sandbox API: create a template, fork a
sandbox, run `exec`, and terminate. The crate is blocking (no async runtime) and
keeps a small dependency tree: `ureq` for HTTP, `serde` / `serde_json` for JSON,
and `getrandom` for the idempotency key and the generated sandbox id. It targets
Rust 1.74 and later.

## Install

```bash
cargo add mitos
```

It is published on crates.io.

## Quickstart (hosted)

Get an API key from [https://mitos.run](https://mitos.run) and set it in the
environment. The base URL defaults to the hosted endpoint. The key is sent as
`Authorization: Bearer <key>` and is never logged.

```bash
export MITOS_API_KEY="sk-..."     # or pass .api_key(...) to the builder
```

```rust
use mitos::SandboxServer;

fn main() -> Result<(), mitos::MitosError> {
    // Base URL + API key resolved from the environment (explicit args override).
    let server = SandboxServer::new();
    server.create_template("python")?;          // build (or get) the template
    let sandbox = server.fork("python")?;        // fork a fresh, independent sandbox

    let result = sandbox.exec("echo hello")?;
    println!("{}", result.exit_code);            // 0
    print!("{}", result.stdout);                 // "hello\n"

    sandbox.terminate()?;
    Ok(())
}
```

`fork` is the snapshot-fork primitive: each call forks a warm template into a
fresh, independent sandbox, so parallel attempts start from the same state.

Point at a local standalone server by setting `MITOS_BASE_URL`, or with the
builder:

```rust
use mitos::SandboxServer;

let server = SandboxServer::builder()
    .base_url("http://localhost:8080")
    .build();
```

## Surface

| Method | HTTP | Returns |
| --- | --- | --- |
| `SandboxServer::new()` / `SandboxServer::builder()` | none | `SandboxServer` |
| `SandboxServer::create_template(id)` | `POST /v1/templates` | `Template` |
| `SandboxServer::create_template_opts(id, init_wait_seconds, idempotency_key)` | `POST /v1/templates` | `Template` |
| `SandboxServer::list_templates()` | `GET /v1/templates` | `Vec<Template>` |
| `SandboxServer::fork(template)` | `POST /v1/fork` | `Sandbox` |
| `SandboxServer::fork_as(template, id)` | `POST /v1/fork` | `Sandbox` |
| `SandboxServer::fork_opts(template, id, idempotency_key)` | `POST /v1/fork` | `Sandbox` |
| `SandboxServer::list_sandboxes()` | `GET /v1/sandboxes` | `Vec<ServerSandbox>` |
| `Sandbox::exec(command)` | `POST /v1/exec` | `ExecResult` |
| `Sandbox::exec_with_timeout(command, timeout)` | `POST /v1/exec` | `ExecResult` |
| `Sandbox::terminate()` | `DELETE /v1/sandboxes/{id}` | `()` |

`fork` defaults `init_wait_seconds` to 5 and generates a `sandbox-<hex>` id; both
creating calls send a fresh `Idempotency-Key` so a retry is de-duplicated by the
server. `exec` defaults to a 30 second timeout and needs a Ready sandbox.

Value types:

- `Template`: `id`, `ready`, `created_at`, `creation_time_ms`.
- `ServerSandbox`: `id`, `template_id`, `endpoint`, `created_at`, `fork_time_ms`.
- `ExecResult`: `exit_code`, `stdout`, `stderr`, `exec_time_ms`, plus `success()`.
- `Sandbox`: `id`, `template_id`, `endpoint`, `fork_time_ms`.

## Auth and base URL precedence

| Setting | Precedence |
| --- | --- |
| Base URL | `.base_url(...)`, else `MITOS_BASE_URL`, else `https://mitos.run` (trailing slash trimmed). |
| Bearer token | `.api_key(...)`, else `MITOS_API_KEY`, else the credential file, else tokenless. |

The credential file is `~/.config/mitos/credentials.json` (honoring
`MITOS_CONFIG_DIR`); only its `"token"` field is read. A missing, unreadable, or
non-JSON credential file is never an error: resolution falls through to
tokenless. The token is sent as `Authorization: Bearer <key>`; the standalone
server ignores it, the hosted endpoint verifies it. The token value is never
logged and is redacted from any error.

## Errors

Every method returns `Result<T, MitosError>`. On a non-2xx response the SDK
parses the server envelope `{error:{code, message, cause, remediation}}` and
falls back to status-derived defaults for a non-mitos server. Branch on `code`,
never on the message text.

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

The configured API key is redacted from any error body before it becomes the
error cause, and never appears in the `Display` output.

## Sandbox ids

Sandbox ids must match `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$` (the same allowlist
every Mitos SDK enforces). `fork` and `terminate` validate the id and return a
typed `invalid_sandbox_id` error before sending any request. Use
`mitos::valid_sandbox_id` to check an id yourself.

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

## Scope

This crate is direct-mode only today. Cluster mode (driving the Kubernetes CRDs)
ships in the Python and TypeScript SDKs and is planned for this crate too, for
full parity (tracked in #305). Beyond the create / fork / exec /
terminate surface above, the following direct-mode endpoints are not part of this
crate: the files API (`/v1/files/*`), interactive PTY (`/v1/pty`), `run_code`,
per-sandbox network posture, `set_timeout`, `pause` / `resume`, and
`get_host(port)` preview URLs.

## The Mitos SDK family

Mitos ships native clients in six languages. All of them share the same
direct-mode surface (create a template, fork, exec, terminate), so the API maps
1:1 across languages; cluster mode (driving the Kubernetes CRDs) ships in Python
and TypeScript today and is planned for the rest, for full parity (#296).

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
