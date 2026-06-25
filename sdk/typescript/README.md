# Mitos TypeScript SDK

Mitos gives AI agents isolated, forkable sandboxes: Firecracker microVMs that
restore from snapshots and fork into parallel attempts, so an agent can branch a
warm environment instead of rebuilding it. Run it fully hosted at
[https://mitos.run](https://mitos.run) or self-hosted on your own Kubernetes
cluster.

This is the TypeScript client. It covers both modes: the direct sandbox API
(hosted or standalone, no Kubernetes) and cluster mode driving the Kubernetes
CRDs. It targets Node 18+ with native `fetch`.

## Install

```bash
npm install @mitos/sdk
```

```bash
pnpm add @mitos/sdk
yarn add @mitos/sdk
```

## Quickstart (hosted)

Get an API key from [https://mitos.run](https://mitos.run) and set it in the
environment. The base URL defaults to the hosted endpoint.

```typescript
import { SandboxServer } from "@mitos/sdk";

// MITOS_API_KEY from the environment; base URL defaults to https://mitos.run.
const server = new SandboxServer();

// Fork a sandbox from a template (a fresh, independent VM from a warm snapshot).
const sandbox = await server.fork("python");

const result = await sandbox.exec("python3 -c 'print(1 + 1)'");
console.log(result.exitCode, result.stdout.trim()); // 0  2

await sandbox.files.write("/workspace/hello.txt", "hello\n");
console.log((await sandbox.files.read("/workspace/hello.txt")).trim()); // hello

await sandbox.terminate();
```

Point at a local standalone server by setting `MITOS_BASE_URL` or passing the
URL: `new SandboxServer("http://localhost:8080")`.

## Direct mode: SandboxServer

`SandboxServer.fork(template)` forks a named template into a fresh, independent
sandbox: that is the snapshot-fork primitive that makes parallel attempts cheap.

```typescript
import { SandboxServer } from "@mitos/sdk";

const server = new SandboxServer();

const templates = await server.listTemplates();
console.log(templates.map((t) => t.id));

await server.createTemplate("python");
const sandbox = await server.fork("python");

// Streaming exec: callbacks fire per chunk; the result still carries the
// aggregate stdout/stderr.
await sandbox.exec("pytest -x", {
  onStdout: (b) => process.stdout.write(b),
});

// Stateful code interpreter (needs a base image with the kernel).
const ex = await sandbox.runCode("import math; math.sqrt(144)");
console.log(ex.text); // 12.0

await sandbox.terminate();
```

### Direct-mode surface

| Method | HTTP | Returns |
| --- | --- | --- |
| `new SandboxServer(url?, token?)` | none | `SandboxServer` |
| `server.createTemplate(id, opts?)` | `POST /v1/templates` | `Template` |
| `server.listTemplates()` | `GET /v1/templates` | `Template[]` |
| `server.fork(template, id?, opts?)` | `POST /v1/fork` | `Sandbox` |
| `server.listSandboxes()` | `GET /v1/sandboxes` | `ServerSandbox[]` |
| `sandbox.exec(cmd, opts?)` | `sandbox.v1.Sandbox/ExecStream` | `ExecResult` |
| `sandbox.execBackground(cmd, opts?)` | `sandbox.v1.Sandbox/ExecStream` | `BackgroundProcess` |
| `sandbox.runCode(code, opts?)` | `sandbox.v1.Sandbox/RunCodeStream` | `Execution` |
| `sandbox.files.read(path)` | `sandbox.v1.Sandbox/ReadFile` | `string` |
| `sandbox.files.write(path, content, opts?)` | `sandbox.v1.Sandbox/WriteFile` | `void` |
| `sandbox.files.list(path?)` | `sandbox.v1.Sandbox/List` | `FileInfo[]` |
| `sandbox.setTimeout(seconds)` | `POST /v1/set_timeout` | `number` (deadline) |
| `sandbox.pause()` / `sandbox.resume()` | `POST /v1/pause`, `/v1/resume` | `void` |
| `sandbox.terminate()` | `DELETE /v1/sandboxes/{id}` | `void` |

An interactive PTY is available via `createPty` / the `Pty` class over
`WS /v1/pty`.

## Cluster mode: AgentRun

Cluster mode drives the Kubernetes CRDs (`SandboxPool`, `Sandbox`, `Workspace`)
in API group `mitos.run/v1` and execs through the forkd sandbox API. Each
sandbox gets a per-sandbox bearer token read from a Secret; the value is never
logged and is redacted from any error message.

```typescript
import { AgentRun, KubeConfigApi } from "@mitos/sdk";

// KubeConfigApi loads ~/.kube/config by default. Pass { inCluster: true }
// inside a pod that has a service account.
const c = new AgentRun({ k8s: new KubeConfigApi(), namespace: "default" });

// Lazy default pool: ensures mitos-default-python (a SandboxPool carrying the
// image in its inline spec.template) if you have none, then starts from it.
const sb = await c.sandbox("python");
const { stdout } = await sb.exec("python -c 'print(2 + 2)'");
console.log(stdout.trim()); // 4
await sb.files.write("/workspace/notes.md", "# findings");

// Reconnect to a live sandbox by id (a durable handle across processes).
const again = await c.fromName(sb.id);

await sb.terminate();
```

Pass `{ pool: "my-pool" }` for the explicit path, which never creates a pool.
The default-pool name is computed by `defaultPoolName(image)` byte-for-byte
identically to the Python `default_pool_name`, so both SDKs target the same pool
object. Set `{ allowDefaultPool: false }` to require an explicit pool.

| Method | Effect |
| --- | --- |
| `c.sandbox("python", opts?)` | one-liner; lazy default pool, claims, waits Ready |
| `c.sandbox(undefined, { pool })` | explicit pool; never creates |
| `c.create(pool, opts?)` | creates a `Sandbox` |
| `c.fromName(name)` | reconnect by id |
| `c.list(pool?)` | lists sandboxes |

## Auth and base URL precedence

Resolution order, highest precedence first:

- Token: the `token` argument, then `MITOS_API_KEY`, then the credential file
  written by `mitos auth login` (`~/.config/mitos/credentials.json`, honoring
  `MITOS_CONFIG_DIR`), then tokenless. The credential-file fallback is read only
  on Node; in a browser bundle it is skipped silently.
- Base URL: the `url` argument, then `MITOS_BASE_URL`, then `https://mitos.run`.

The token rides on `Authorization: Bearer <token>`. The standalone server runs
tokenless and ignores it; the hosted endpoint verifies it. The token value is
never logged.

## Errors

Failures are `AgentRunError` instances, parsed from the server envelope
`{error:{code, message, cause, remediation}}`. Branch on `code`, never on the
message text.

```typescript
import { AgentRunError } from "@mitos/sdk";

try {
  await sandbox.exec("...");
} catch (err) {
  if (err instanceof AgentRunError) {
    console.error(err.code);        // e.g. "unauthorized", "not_found"
    console.error(err.remediation); // actionable hint
  }
}
```

A bearer token a misconfigured server reflects back is redacted (via `redact`)
before it becomes the error cause.

## Sandbox ids

Sandbox ids must match `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`. `fork` and
`terminate` validate the id (the explicit one or the generated `sandbox-<hex>`)
and throw `invalid_sandbox_id` before sending any request. `validSandboxId(id)`
exposes the check.

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

## Development

```bash
npm ci
npm run build   # tsc -> dist/
npm test        # vitest conformance suite
npm run lint    # tsc --noEmit + tsc --project tsconfig.examples.json
```

## License

Apache-2.0.
</content>
