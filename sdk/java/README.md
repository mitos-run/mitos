# mitos Java SDK

A thin, dependency-free Java client for the standalone and hosted mitos
sandbox-server REST API. It mirrors the direct-mode surface of the Python SDK
(`sdk/python/mitos/direct.py`), the TypeScript SDK
(`sdk/typescript/src/server.ts`), the Ruby SDK (`sdk/ruby`), and the Rust SDK
(`sdk/rust`): create a template, fork a sandbox, run `exec`, and terminate.

The SDK uses only the JDK standard library: `java.net.http.HttpClient` for HTTP,
`java.nio` for the credential file, and a tiny hand-rolled JSON helper for the
few flat wire shapes. There are no runtime dependencies. It targets the Java 17
language level (compiled with `--release 17`) so it is broadly usable on Java 17
and later.

## Scope

This SDK covers DIRECT mode only: the standalone `cmd/sandbox-server` and the
hosted control plane at `https://mitos.run`. The Kubernetes / cluster mode (the
controller, forkd, and the SandboxTemplate / SandboxPool / SandboxClaim /
SandboxFork CRDs) is served by the Python and TypeScript SDKs only and is NOT
part of this SDK.

## Build from source

Not yet published to Maven Central. Build the classes with plain `javac` (no
build tool required):

```bash
javac --release 17 -d out $(find sdk/java/src/main -name '*.java')
```

Then put `out` on your classpath. A Maven layout (`pom.xml` plus
`src/main/java`) is already in place so the SDK can publish to Maven Central
later without restructuring; see "Publishing" below.

## Quickstart (hosted)

The base URL defaults to the hosted endpoint `https://mitos.run`. Set your API
key in the environment; it is sent as `Authorization: Bearer <key>` and is never
logged.

```java
import run.mitos.sdk.SandboxServer;
import run.mitos.sdk.Sandbox;
import run.mitos.sdk.ExecResult;

// MITOS_API_KEY from the environment, or pass it to the constructor.
SandboxServer server = new SandboxServer();   // base URL + token from the env
server.createTemplate("python");              // build (or get) the template
Sandbox sandbox = server.fork("python");      // fork a fresh sandbox

ExecResult result = sandbox.exec("echo hello");
System.out.println(result.exitCode());        // 0
System.out.println(result.stdout());          // "hello\n"

sandbox.terminate();
```

Point at a local standalone server by setting `MITOS_BASE_URL` (or passing the
URL to the constructor):

```java
SandboxServer server = new SandboxServer("http://localhost:8080", null);
```

## Surface

| Method | HTTP | Returns | Notes |
| --- | --- | --- | --- |
| `new SandboxServer()` | none | `SandboxServer` | base URL + token resolved from args / env / credential file |
| `new SandboxServer(baseUrl, apiKey)` | none | `SandboxServer` | either argument may be `null` to fall through the precedence |
| `createTemplate(id)` | `POST /v1/templates` | `Template` | sends a fresh `Idempotency-Key` |
| `createTemplate(id, initWaitSeconds, idempotencyKey)` | `POST /v1/templates` | `Template` | explicit build wait and key |
| `listTemplates()` | `GET /v1/templates` | `List<Template>` | |
| `fork(template)` | `POST /v1/fork` | `Sandbox` | generates a `sandbox-<hex>` id, sends a fresh `Idempotency-Key` |
| `fork(template, id)` | `POST /v1/fork` | `Sandbox` | explicit id, validated before the request |
| `fork(template, id, idempotencyKey)` | `POST /v1/fork` | `Sandbox` | explicit id and key |
| `listSandboxes()` | `GET /v1/sandboxes` | `List<ServerSandbox>` | |
| `Sandbox.exec(command)` | `POST /v1/exec` | `ExecResult` | default 30s timeout |
| `Sandbox.exec(command, timeoutSeconds)` | `POST /v1/exec` | `ExecResult` | |
| `Sandbox.terminate()` | `DELETE /v1/sandboxes/{id}` | `void` | |

`Template`, `ServerSandbox`, and `ExecResult` are immutable records. Errors are
raised as `MitosException` (see below).

## Auth and base URL precedence

This SDK applies the same precedence as the Python, TypeScript, Ruby, and Rust
direct-mode SDKs.

Base URL: the constructor argument, then `MITOS_BASE_URL`, then the hosted
production endpoint `https://mitos.run`.

Bearer token: the constructor argument, then `MITOS_API_KEY`, then the CLI login
credential file written by `mitos auth login`
(`~/.config/mitos/credentials.json`, honoring `MITOS_CONFIG_DIR`, the `token`
field), then none (tokenless). A missing, unreadable, or non-JSON credential
file is NOT an error: it simply yields no token, so the SDK stays usable
tokenless against the standalone server. The token VALUE is never logged, never
placed in an error message, and is redacted from any error body before it
becomes a cause.

## Errors

Every failure raises `MitosException`, an UNCHECKED exception (a
`RuntimeException` subclass) so callers are not forced to wrap every call in a
`try`/`catch`. It carries the server error envelope
`{error:{code, message, cause, remediation}}`:

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
rejected with code `invalid_sandbox_id` BEFORE any request is sent.

## Deferred surface

Direct-mode parity with the richer Python and TypeScript clients is intentionally
not yet implemented here. The following are deferred to follow-ups:

- Kubernetes / cluster mode (controller, forkd, the CRDs): Python and
  TypeScript only.
- File operations (`files.read` / `write` / `list` / `remove` / `mkdir`).
- Interactive PTY over WebSocket.
- `run_code` against the stateful code-interpreter kernel.
- `pause` / `resume`, `set_timeout`, `get_host` preview URLs, and per-sandbox
  `Network` posture.
- Sandbox-to-sandbox `fork` (siblings) from a running handle.

## Publishing

The `pom.xml` carries the metadata Maven Central needs (`url`, `scm`,
`developers`, `licenses`, `issueManagement`) so the published page links back to
`https://mitos.run`. Publishing itself is a follow-up: it requires claiming the
`run.mitos` namespace on Maven Central (Sonatype Central) and GPG-signing the
artifacts. Until then, build from source as shown above.
