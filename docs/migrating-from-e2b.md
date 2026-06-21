# Migrating from E2B

`mitos.e2b` is a one-way migration bridge for teams leaving E2B's cloud for a
SELF-HOSTED sandbox runtime: regulated, air-gapped, or on-prem environments
where agent code, data, and credentials must not run on someone else's
infrastructure. The promise is "change one import":

```python
# before
from e2b_code_interpreter import Sandbox
# after
from mitos.e2b import Sandbox
```

The shim presents E2B's `Sandbox` surface over the standalone mitos
sandbox-server REST API (no Kubernetes required). It is an adapter over the
native `DirectSandbox` surface, not a re-implementation, and it has no
dependency on the `e2b` package. E2B's per-operation vocabulary is roughly 70%
aligned with ours, so most scripts run unchanged.

## Why self-host

- Your agents' code, data, and credentials run on infrastructure you control.
- Air-gapped and regulated deployments: no egress to a third-party cloud.
- The same snapshot-fork engine, exec, files, and code-interpreter surface you
  get from the native SDK, behind E2B's method names.

## Quickstart

Point the shim at a sandbox-server you run (for example the local mock engine,
`mitos dev up`, or a real KVM deployment):

```python
from mitos.e2b import Sandbox

sandbox = Sandbox.create("python", base_url="http://localhost:8080")

# run a shell command
result = sandbox.commands.run("echo hello")
print(result.stdout, result.exit_code)

# files
sandbox.files.write("/tmp/data.json", "{}")
print(sandbox.files.read("/tmp/data.json"))
sandbox.files.make_dir("/tmp/out")          # E2B make_dir -> mitos mkdir

# code interpreter with rich MIME results
execution = sandbox.run_code("import matplotlib; 1 + 1")
print(execution.text)
for r in execution.results:
    print(r.data.keys())                     # image/png, text/html, ...

# live TTL extension and teardown
sandbox.set_timeout(300)
sandbox.kill()
```

Auth resolves exactly like `mitos.create`: pass `api_key=` / `base_url=`
explicitly, or set `MITOS_API_KEY` / `MITOS_BASE_URL`. The standalone server is
tokenless and ignores the key; the hosted front door verifies the same header
with no code change.

## Reconnect and list

```python
sandbox = Sandbox.create("python", base_url="http://localhost:8080")
again = Sandbox.connect(sandbox.sandbox_id, base_url="http://localhost:8080")

for info in Sandbox.list(base_url="http://localhost:8080"):
    print(info.sandbox_id)
```

`connect` reattaches to a running sandbox by id; an unknown id raises the typed
`NotFoundError`.

## Support table

Every E2B method the shim exposes, what it maps to, and whether it works today
against the standalone sandbox-server.

| E2B method | mitos op | Status |
|---|---|---|
| `Sandbox.create(template, ...)` | `mitos.create` / `DirectSandbox` | Supported |
| `Sandbox.connect(id)` | `SandboxServer.list_sandboxes` lookup | Supported |
| `Sandbox.list()` | `SandboxServer.list_sandboxes` | Supported |
| `sandbox.commands.run(cmd)` | `DirectSandbox.exec` | Supported (needs a guest agent) |
| `sandbox.commands.run(cmd, background=True)` | `DirectSandbox.exec` | Supported (needs a guest agent) |
| `sandbox.files.read(path)` | `DirectSandbox.files.read` | Supported (needs a guest agent) |
| `sandbox.files.write(path, data)` | `DirectSandbox.files.write` | Supported (needs a guest agent) |
| `sandbox.files.list(path)` | `DirectSandbox.files.list` | Supported (needs a guest agent) |
| `sandbox.files.exists(path)` | `DirectSandbox.files.exists` | Supported (needs a guest agent) |
| `sandbox.files.remove(path)` | `DirectSandbox.files.remove` | Supported (needs a guest agent) |
| `sandbox.files.make_dir(path)` | `DirectSandbox.files.mkdir` (rename) | Supported (needs a guest agent) |
| `sandbox.run_code(code)` | `DirectSandbox.run_code` (rich MIME `Result`) | Supported (needs a guest agent) |
| `sandbox.set_timeout(seconds)` | `DirectSandbox.set_timeout` (issue #218) | Supported |
| `sandbox.kill()` | `DirectSandbox.terminate` | Supported |
| `sandbox.get_host(port)` | preview URLs (issue [#126](https://github.com/mitos-run/mitos/issues/126)) | Supported (needs the preview proxy deployed) |

"needs a guest agent" means the op runs end-to-end only against a real guest
over vsock (a KVM deployment, or `mitos dev up` with the engine). The bare
mock-engine sandbox-server answers the create / connect / list / kill /
set_timeout lifecycle but cannot run exec / files / run_code, which is why the
test suite proves those against a fake target and runs them end-to-end in the
KVM CI job.

## The one vocabulary rename

E2B's `files.make_dir(path)` maps to mitos `files.mkdir(path)`. The shim
exposes `make_dir` so your E2B script does not change; under the hood it calls
`mkdir`.

## get_host returns a signed preview URL

`sandbox.get_host(port)` returns a signed, expiring preview URL for a sandbox
port, served by the per-sandbox preview reverse proxy (issue
[#126](https://github.com/mitos-run/mitos/issues/126)):

```python
url = sandbox.get_host(3000)   # https://<sandbox-id>.preview.<domain>/?token=...
```

The server mints the URL (the signing secret stays server-side) and the token
expires (Daytona style), so treat the URL value as a credential. The URL
resolves once the preview proxy is deployed and routing to the sandbox; the
proxy is not part of the default install yet, so a server that does not expose
it raises a typed `AgentRunError` from the call rather than fabricating a URL
that would not resolve.

## What you gain beyond E2B parity

The underlying `DirectSandbox` (reachable via `sandbox.sandbox`) exposes mitos
superpowers E2B does not: `fork(n)` for branching / parallel runs at fork(2)
speeds, `pause` / `resume`, and an interactive `pty`. The shim keeps your E2B
script working; reach for the native handle when you want those.
