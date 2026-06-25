# Mitos Java SDK

Mitos gives AI agents isolated, forkable sandboxes: Firecracker microVMs that
restore from snapshots and fork into parallel attempts, so an agent can branch a
warm environment instead of rebuilding it. Run it fully hosted at
[https://mitos.run](https://mitos.run) or self-hosted on your own Kubernetes
cluster.

This is the Java client for both modes Mitos runs in:

- **Direct mode** (`SandboxServer`): talk to the standalone or hosted
  sandbox-server REST API. Create a template, fork a sandbox, run `exec`, and
  terminate.
- **Cluster mode** (`AgentRun`): drive the `mitos.run/v1` Kubernetes CRDs
  (`SandboxPool`, `Sandbox`, `Workspace`) directly, the operator path. This is
  the same surface the Python and TypeScript SDKs expose.

It uses only the JDK standard library, so it has no runtime dependencies:
`java.net.http.HttpClient` for HTTP, `javax.net.ssl` for the cluster CA and
optional client-certificate mutual TLS, `java.util.Base64` for the
service-account and Secret payloads, and a small hand-rolled JSON helper (plus a
minimal kubeconfig YAML reader) for the wire shapes. It targets the Java 17
language level and is usable on Java 17 and later.

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

## Cluster mode (AgentRun)

When you run Mitos on your own Kubernetes cluster, `AgentRun` is the operator
path: it reconciles the `mitos.run/v1` CRDs directly, so a `Sandbox` is born from
a `SandboxPool` and the controller drives it to Ready. This is the Java port of
the Python `AgentRun`, with the same surface and the same
`mitos-default-<image-slug>` lazy-pool naming, byte-for-byte.

It speaks the Kubernetes REST API on the JDK standard library alone, with no
`fabric8` and no official client: `HttpClient` for REST and `SSLContext` for the
cluster CA (and client-certificate mutual TLS for kind/minikube). Direct mode
above is untouched and pulls in none of it.

### The one-liner

```java
import run.mitos.sdk.AgentRun;
import run.mitos.sdk.ClusterSandbox;
import run.mitos.sdk.SandboxParams;

// In a pod: load the in-cluster service-account mount.
AgentRun agent = AgentRun.inCluster("agents");        // namespace "agents"

// Lazy default pool: ensures a SandboxPool named mitos-default-python-3.12
// exists (creating it with an inline template if absent), then starts a Sandbox.
ClusterSandbox sb = agent.sandbox("python:3.12").waitUntilReady();

System.out.println(sb.phase());     // READY
System.out.println(sb.endpoint());  // 10.0.0.7:9091
String token = sb.token();          // per-sandbox bearer token, never logged

sb.terminate();
```

From outside the cluster, load a kubeconfig instead:

```java
// null path falls back to $KUBECONFIG, then ~/.kube/config.
AgentRun agent = AgentRun.fromKubeconfig(null, "agents");
```

### The full surface

```java
// Explicit pool, plus env/secrets/ttl/workspace via the params builder.
ClusterSandbox sb = agent.create(SandboxParams.builder()
        .pool("warm-python")
        .name("worker-1")
        .env("MODE", "fast")
        .secret("API_KEY", "my-creds", "key")  // (env var) -> Secret name + key
        .ttl("30m")
        .workspace("project-x")
        .build());

// Reconnect across processes by name.
ClusterSandbox again = agent.fromName("worker-1");

// List, optionally filtered by pool (null lists all).
for (ClusterSandbox s : agent.list("warm-python")) {
    System.out.println(s.name() + " " + s.phase());
}

// Pool status.
var status = agent.poolStatus("warm-python");
System.out.println(status.readySnapshots() + "/" + status.desired());

// Durable workspaces (git-shaped).
var ws = agent.createWorkspace("project-x");
System.out.println(ws.head());      // current head revision
ws.log().forEach(r -> System.out.println(r.name() + " " + r.phase()));
```

### Auth modes

| Mode | Construct with | Auth | TLS |
| --- | --- | --- | --- |
| In-cluster | `AgentRun.inCluster([namespace])` | the projected service-account token (`/var/run/secrets/.../token`) | the mounted cluster CA (`.../ca.crt`) |
| Kubeconfig | `AgentRun.fromKubeconfig(path[, namespace])` | the current context's bearer token, or its client cert/key | the cluster `certificate-authority(-data)`, else system roots |

The kubeconfig reader parses a common subset: the current context's cluster
(`server`, inline `certificate-authority-data` or a `certificate-authority`
file) and user (a bearer `token`, or `client-certificate-data` +
`client-key-data` for mutual-TLS clusters). It does not support exec credential
plugins or `auth-provider` blocks; inside a cluster use `inCluster()`. The
service-account and per-sandbox token values are held in memory only and are
never logged.

### Default-pool naming

`AgentRun.defaultPoolName(image)` is the deterministic slug behind the lazy
`sandbox(image)` path, identical to the Python and TypeScript SDKs: lowercase,
`/` and `:` become `-`, any other unsafe character collapses to `-`, the slug is
bounded to 40 characters, leading/trailing `-` and `.` are stripped, and the
result is prefixed with `mitos-default-`. For example `python:3.12` maps to
`mitos-default-python-3.12` and `ghcr.io/Acme/Foo-Bar:latest` to
`mitos-default-ghcr.io-acme-foo-bar-latest`.

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
| `Sandbox.exec(command)` | `POST /sandbox.v1.Sandbox/ExecStream` | `ExecResult` |
| `Sandbox.exec(command, timeoutSeconds)` | `POST /sandbox.v1.Sandbox/ExecStream` | `ExecResult` |
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

This SDK covers both direct mode (the create / fork / exec / terminate surface
above) and cluster mode (`AgentRun` driving the `mitos.run/v1` CRDs:
`SandboxPool`, `Sandbox`, `Workspace`). The cluster surface ports the Python
`AgentRun`: `sandbox` / `create` / `get` / `fromName` / `list`, `poolStatus`,
`waitUntilReady` / `info` / `terminate`, and the workspace verbs
(`createWorkspace` / `workspace` / `getWorkspace` / `listWorkspaces`,
`head` / `resumable` / `log`).

Beyond those, the following are not part of this SDK yet: direct-mode file
operations (`files.read` / `write` / `list` / `remove` / `mkdir`), interactive
PTY over WebSocket, `run_code` against the code-interpreter kernel,
`pause` / `resume`, `set_timeout`, `get_host` preview URLs, per-sandbox
`Network` posture, and sandbox-to-sandbox `fork` from a running handle. The
cluster `ClusterSandbox` resolves its endpoint and per-sandbox token so a caller
can drive the sandbox HTTP API directly.

## The Mitos SDK family

Mitos ships native clients in six languages. All of them share the same
direct-mode surface (create a template, fork, exec, terminate), so the API maps
1:1 across languages; cluster mode (driving the Kubernetes CRDs) ships in Python,
TypeScript, and Java, for full parity (#296).

| Language | Install | Covers |
| --- | --- | --- |
| Python | `pip install mitos-run` | direct + cluster + async |
| TypeScript | `npm install @mitos/sdk` | direct + cluster |
| Java | build from source | direct + cluster |
| Ruby | `gem install mitos` | direct |
| Rust | `cargo add mitos` | direct |
| Go | `go get github.com/mitos-run/mitos/sdk/go` | direct |

Project home: [https://mitos.run](https://mitos.run). Source and all six SDKs:
[github.com/mitos-run/mitos](https://github.com/mitos-run/mitos).

## License

Apache-2.0.
</content>
