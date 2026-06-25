# Mitos Rust SDK

Mitos gives AI agents isolated, forkable sandboxes: Firecracker microVMs that
restore from snapshots and fork into parallel attempts, so an agent can branch a
warm environment instead of rebuilding it. Run it fully hosted at
[https://mitos.run](https://mitos.run) or self-hosted on your own Kubernetes
cluster.

This is the native Rust client for Mitos, at parity with the Python and
TypeScript SDKs. It covers both modes:

- **Direct mode** (`SandboxServer`): the standalone and hosted REST API. Create
  a template, fork a sandbox, run `exec`, and terminate.
- **Cluster mode** (`AgentRun`): the Kubernetes operator path, driving the
  declarative CRDs (`SandboxPool`, `Sandbox`, `Workspace`) in the `mitos.run/v1`
  API group on your own cluster.

The crate is blocking (no async runtime). Direct mode keeps a tiny dependency
tree: `ureq` for HTTP, `serde` / `serde_json` for JSON, and `getrandom` for the
idempotency key and the generated sandbox id. Cluster mode reuses the same
`ureq` transport and the rustls it re-exports, trusting the cluster CA; it adds
only a small YAML parser (`serde_yml`) for kubeconfig and a PEM reader
(`rustls-pemfile`) for the CA and client certificates. It targets Rust 1.74 and
later.

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
| `Sandbox::exec(command)` | `POST /sandbox.v1.Sandbox/ExecStream` | `ExecResult` |
| `Sandbox::exec_with_timeout(command, timeout)` | `POST /sandbox.v1.Sandbox/ExecStream` | `ExecResult` |
| `Sandbox::terminate()` | `DELETE /v1/sandboxes/{id}` | `()` |

`fork` defaults `init_wait_seconds` to 5 and generates a `sandbox-<hex>` id; both
creating calls send a fresh `Idempotency-Key` so a retry is de-duplicated by the
server. `exec` defaults to a 30 second timeout and needs a Ready sandbox; it runs
over the Connect `sandbox.v1.Sandbox/ExecStream` runtime protocol (the sandbox id
rides the `X-Sandbox-Id` header) and drains the streamed stdout, stderr, and exit
frames into an `ExecResult`.

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

## Cluster mode (Kubernetes)

When you run Mitos on your own Kubernetes cluster, `AgentRun` is the operator
path: instead of a REST server it drives the declarative CRDs (`SandboxPool`,
`Sandbox`, `Workspace`) in the `mitos.run/v1` API group. This is the same
surface as the Python `AgentRun` (`pip install mitos-run`) and the TypeScript
SDK, ported one-to-one to idiomatic Rust.

The one-liner is `sandbox(image)`: it gets-or-creates a default pool named
`mitos-default-<image-slug>` (with an inline `spec.template.image`), then claims
a `Sandbox` from it.

```rust
use mitos::AgentRun;

fn main() -> Result<(), mitos::MitosError> {
    // A kubeconfig (None resolves $KUBECONFIG, then $HOME/.kube/config).
    let client = AgentRun::from_kubeconfig("default", None)?;

    // Lazy: ensure the mitos-default-python pool exists, then create a Sandbox.
    let sandbox = client.sandbox("python")?;
    println!("{} ({:?})", sandbox.name, sandbox.phase());

    Ok(())
}
```

Inside a cluster (a pod with a mounted service account), construct it from the
in-cluster config instead. The API server, CA, and bearer token are read from
the projected service account; the token is held in memory and never logged.

```rust
use mitos::AgentRun;

# fn main() -> Result<(), mitos::MitosError> {
let client = AgentRun::in_cluster("default")?;
# Ok(())
# }
```

The explicit path never creates anything. `create` claims from a named pool with
optional env, secrets, a TTL, and a workspace binding:

```rust
use mitos::{AgentRun, CreateSandbox};

# fn run(client: &AgentRun) -> Result<(), mitos::MitosError> {
let sandbox = client.create(
    "prod-pool",
    CreateSandbox::new()
        .env("LOG_LEVEL", "info")
        .secret("OPENAI_API_KEY", "llm-creds", "api-key")
        .ttl("30m")
        .workspace("agent-home"),
)?;
# Ok(())
# }
```

### Auth modes

| Mode | Constructor | Resolves |
| --- | --- | --- |
| In-cluster | `AgentRun::in_cluster(ns)` | `KUBERNETES_SERVICE_HOST` / `_PORT`, the projected `ca.crt`, and the projected `token`. |
| Kubeconfig | `AgentRun::from_kubeconfig(ns, path)` | The current context's server, CA, and credential (a bearer token, a token file, or a client certificate / key). `path` is `None` for `$KUBECONFIG`, then `$HOME/.kube/config`. |

TLS verifies against the cluster CA (the self-signed API server certificate is
trusted through the kubeconfig or service account CA, not the public roots). A
kubeconfig that sets `insecure-skip-tls-verify: true` is honored only when it
opts in explicitly; verification is on by default. Client-certificate auth is
carried by the TLS layer; a bearer token rides in the `Authorization` header.
The per-sandbox token (from the `<name>-sandbox-token` Secret) is read into
memory for a Ready sandbox and is never logged.

### Cluster surface

| Method | Action |
| --- | --- |
| `AgentRun::in_cluster(ns)` / `AgentRun::from_kubeconfig(ns, path)` | Construct the client. |
| `AgentRun::sandbox(image)` | Lazy: get-or-create the default pool, then create a `Sandbox`. |
| `AgentRun::create(pool, CreateSandbox)` | Create a `Sandbox` from a named pool (env / secrets / ttl / workspace). |
| `AgentRun::get(name)` / `AgentRun::from_name(name)` | Reconnect to an existing sandbox. |
| `AgentRun::list(pool)` | List sandboxes, optionally filtered by pool. |
| `AgentRun::pool_status(name)` | Read a `SandboxPool` status (`PoolStatus`). |
| `AgentRun::create_workspace(name)` / `workspace(name)` / `get_workspace(name)` / `list_workspaces()` | Durable `Workspace` handles. |
| `ClusterSandbox::phase()` / `endpoint()` / `refresh()` / `terminate()` | Inspect, refresh, and tear down a sandbox. |
| `Workspace::head()` / `resumable()` | Read workspace status. |
| `mitos::default_pool_name(image)` | The default-pool slug for an image (byte-for-byte equal to the Python and TypeScript SDKs). |

`default_pool_name` lowercases the image, maps `/` and `:` to `-`, collapses any
other unsafe run to `-`, bounds the slug to 40 characters, trims leading and
trailing `-` and `.`, and prefixes `mitos-default-`. For example `python:3.12`
becomes `mitos-default-python-3.12`.

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

This crate covers both direct mode (`SandboxServer`) and cluster mode
(`AgentRun`, the Kubernetes CRD path), at parity with the Python and TypeScript
SDKs. Beyond the create / fork / exec / terminate surface above, the following
direct-mode endpoints are not part of this crate yet: the files API
(`/v1/files/*`), interactive PTY (`/v1/pty`), `run_code`, per-sandbox network
posture, `set_timeout`, `pause` / `resume`, and `get_host(port)` preview URLs. In
cluster mode the sandbox lifecycle (create / get / list / pool status / workspace
handles / terminate) is covered; the in-VM exec and file traffic flows through
the same direct-mode sandbox API.

## The Mitos SDK family

Mitos ships native clients in six languages. All of them share the same
direct-mode surface (create a template, fork, exec, terminate), so the API maps
1:1 across languages; cluster mode (driving the Kubernetes CRDs) ships in Python,
TypeScript, and Rust today and is planned for the rest, for full parity.

| Language | Install | Covers |
| --- | --- | --- |
| Python | `pip install mitos-run` | direct + cluster + async |
| TypeScript | `npm install @mitos/sdk` | direct + cluster |
| Rust | `cargo add mitos` | direct + cluster |
| Ruby | `gem install mitos` | direct |
| Go | `go get github.com/mitos-run/mitos/sdk/go` | direct |
| Java | build from source | direct |

Project home: [https://mitos.run](https://mitos.run). Source and all six SDKs:
[github.com/mitos-run/mitos](https://github.com/mitos-run/mitos).

## License

Apache-2.0.
</content>
