# Mitos Java SDK

Mitos gives AI agents isolated, forkable sandboxes: Firecracker microVMs that
restore from snapshots and fork into parallel attempts, so an agent can branch a
warm environment instead of rebuilding it. Run it fully hosted at
[https://mitos.run](https://mitos.run) or self-hosted on your own Kubernetes
cluster.

This is the Java client for the direct sandbox API: create a template, fork a
sandbox, run `exec`, and terminate. It uses only the JDK standard library
(`java.net.http.HttpClient` for HTTP, `java.nio` for the credential file, and a
small hand-rolled JSON helper for the flat wire shapes), so it has no runtime
dependencies. It targets the Java 17 language level and is usable on Java 17 and
later.

## Install

The SDK is a single zero-dependency package built with plain `javac`, no build
tool required:

```bash
javac --release 17 -d out $(find sdk/java/src/main -name '*.java')
```

Put `out` on your classpath and you are ready to go. A Maven layout (`pom.xml`
plus `src/main/java`) ships with the SDK; the published Maven coordinate will be
`run.mitos:mitos-sdk` once it lands on Maven Central.

## Quickstart (hosted)

Get an API key from [https://mitos.run](https://mitos.run) and set it in the
environment. The base URL defaults to the hosted endpoint. The key is sent as
`Authorization: Bearer <key>` and is never logged.

```java
import run.mitos.sdk.SandboxServer;
import run.mitos.sdk.Sandbox;
import run.mitos.sdk.ExecResult;

// MITOS_API_KEY from the environment, or pass it to the constructor.
SandboxServer server = new SandboxServer();   // base URL + token from the env
server.createTemplate("python");              // build (or get) the template
Sandbox sandbox = server.fork("python");      // fork a fresh, independent sandbox

ExecResult result = sandbox.exec("echo hello");
System.out.println(result.exitCode());        // 0
System.out.println(result.stdout());          // "hello\n"

sandbox.terminate();
```

`fork` is the snapshot-fork primitive: each call forks a warm template into a
fresh, independent sandbox, so parallel attempts start from the same state.

Point at a local standalone server by setting `MITOS_BASE_URL` or passing the URL
to the constructor:

```java
SandboxServer server = new SandboxServer("http://localhost:8080", null);
```

## Surface

| Method | HTTP | Returns |
| --- | --- | --- |
| `new SandboxServer()` | none | `SandboxServer` |
| `new SandboxServer(baseUrl, apiKey)` | none | `SandboxServer` |
| `createTemplate(id)` | `POST /v1/templates` | `Template` |
| `createTemplate(id, initWaitSeconds, idempotencyKey)` | `POST /v1/templates` | `Template` |
| `listTemplates()` | `GET /v1/templates` | `List<Template>` |
| `fork(template)` | `POST /v1/fork` | `Sandbox` |
| `fork(template, id)` | `POST /v1/fork` | `Sandbox` |
| `fork(template, id, idempotencyKey)` | `POST /v1/fork` | `Sandbox` |
| `listSandboxes()` | `GET /v1/sandboxes` | `List<ServerSandbox>` |
| `Sandbox.exec(command)` | `POST /v1/exec` | `ExecResult` |
| `Sandbox.exec(command, timeoutSeconds)` | `POST /v1/exec` | `ExecResult` |
| `Sandbox.terminate()` | `DELETE /v1/sandboxes/{id}` | `void` |

Either constructor argument may be `null` to fall through the precedence below.
Creating calls send a fresh `Idempotency-Key`, `fork` generates a
`sandbox-<hex>` id when none is given, and `exec` defaults to a 30 second
timeout. `Template`, `ServerSandbox`, and `ExecResult` are immutable records.

## Auth and base URL precedence

Resolution order, highest precedence first:

- Bearer token: the constructor argument, then `MITOS_API_KEY`, then the
  credential file written by `mitos auth login`
  (`~/.config/mitos/credentials.json`, honoring `MITOS_CONFIG_DIR`, the `token`
  field), then tokenless.
- Base URL: the constructor argument, then `MITOS_BASE_URL`, then
  `https://mitos.run`.

A missing, unreadable, or non-JSON credential file is never an error: resolution
falls through to tokenless. The token is sent as `Authorization: Bearer <key>`;
the standalone server ignores it, the hosted endpoint verifies it. The token
value is never logged, never placed in an error message, and is redacted from any
error body before it becomes a cause.

## Errors

Every failure raises `MitosException`, an unchecked exception (a
`RuntimeException` subclass) so callers are not forced to wrap every call. It
carries the server error envelope `{error:{code, message, cause, remediation}}`.

```java
try {
    server.fork("python", "sb-1");
} catch (MitosException e) {
    e.getCode();         // stable machine-readable code, for example "not_found"
    e.getStatus();       // HTTP status, or 0 when raised before any request
    e.getCauseDetail();  // the underlying detail, redacted of any token
    e.getRemediation();  // a short actionable hint
}
```

Branch on `getCode()`, never on the message text. An invalid sandbox id is
rejected with code `invalid_sandbox_id` before any request is sent.

## Sandbox ids

Sandbox ids must match `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$` (the same allowlist
every Mitos SDK enforces). `fork` and `terminate` validate the id (the explicit
one or the generated `sandbox-<hex>`) and raise `invalid_sandbox_id` before
sending any request.

## Scope

This SDK is direct-mode only today. Cluster mode (driving the Kubernetes CRDs)
ships in the Python and TypeScript SDKs and is planned for this SDK too, for full
parity. Beyond the create / fork / exec /
terminate surface above, the following are not part of this SDK: file operations
(`files.read` / `write` / `list` / `remove` / `mkdir`), interactive PTY over
WebSocket, `run_code` against the code-interpreter kernel, `pause` / `resume`,
`set_timeout`, `get_host` preview URLs, per-sandbox `Network` posture, and
sandbox-to-sandbox `fork` from a running handle.

## The Mitos SDK family

Mitos ships native clients in six languages. All of them share the same
direct-mode surface (create a template, fork, exec, terminate), so the API maps
1:1 across languages; cluster mode (driving the Kubernetes CRDs) ships in Python
and TypeScript today and is planned for the rest, for full parity.

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
