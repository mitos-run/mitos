# Migrating from Daytona

`mitos.daytona` is a one-way migration bridge for teams leaving Daytona for a
SELF-HOSTED sandbox runtime. Daytona moved its runtime to a private codebase, so
the open path many teams adopted is closed. The promise here is "change one
import":

```python
# before
from daytona import Daytona, CreateSandboxFromSnapshotParams
# after
from mitos.daytona import Daytona, CreateSandboxFromSnapshotParams
```

The shim presents Daytona's client and `Sandbox` surface over the standalone
Mitos sandbox-server REST API (no Kubernetes required). It is an adapter over the
native `DirectSandbox` surface, not a re-implementation, and it has no dependency
on the `daytona` package. Most scripts run unchanged.

## Why self-host

- Your agents' code, data, and credentials run on infrastructure you control.
- The runtime is open source under Apache-2.0, a license that cannot be revoked
  on the code already released.
- The same snapshot-fork engine, exec, files, and code-interpreter surface you
  get from the native SDK, behind Daytona's method names. Plus `fork(n)`, which
  Daytona does not have.

## Quickstart

Point the client at a sandbox-server you run (for example the local mock engine,
`mitos dev up`, or a real KVM deployment):

```python
from mitos.daytona import Daytona, DaytonaConfig, CreateSandboxFromSnapshotParams

daytona = Daytona(DaytonaConfig(api_url="http://localhost:8080"))
sandbox = daytona.create(CreateSandboxFromSnapshotParams(language="python"))

# run a shell command
response = sandbox.process.exec("echo hello", cwd="/workspace")
print(response.result, response.exit_code)

# run code in the stateful kernel
run = sandbox.process.code_run("print(1 + 1)")
print(run.result)

# files
sandbox.fs.upload_file(b"{}", "/tmp/data.json")
print(sandbox.fs.download_file("/tmp/data.json"))
sandbox.fs.create_folder("/tmp/out", "0755")

# a signed preview URL for a guest port
link = sandbox.get_preview_link(3000)
print(link.url, link.token)

# stop / start (pause / resume) and delete
sandbox.stop()
sandbox.start()
daytona.delete(sandbox)
```

Auth resolves exactly like `mitos.create`: pass `api_key=` / `api_url=` on the
`DaytonaConfig`, or set `MITOS_API_KEY` / `MITOS_BASE_URL`. The standalone server
is tokenless and ignores the key; the hosted front door verifies the same header
with no code change.

## Reconnect and list

```python
sandbox = daytona.create(CreateSandboxFromSnapshotParams(language="python"))
again = daytona.get(sandbox.id)

for s in daytona.list():
    print(s.id)
```

`get` reattaches to a running sandbox by id; an unknown id raises the typed
`NotFoundError`.

## Support table

Every Daytona verb the shim exposes, what it maps to, and whether it works today
against the standalone sandbox-server.

| Daytona verb | Mitos op | Status |
|---|---|---|
| `Daytona(config)` | hold `api_key` / `api_url` | Supported |
| `daytona.create(params)` | `mitos.create` / `DirectSandbox` | Supported |
| `daytona.get(id)` | `SandboxServer.list_sandboxes` lookup | Supported |
| `daytona.list()` | `SandboxServer.list_sandboxes` | Supported |
| `daytona.delete(sandbox)` | `DirectSandbox.terminate` | Supported |
| `daytona.start(sandbox)` | `DirectSandbox.resume` | Supported |
| `daytona.stop(sandbox)` | `DirectSandbox.pause` | Supported |
| `sandbox.process.exec(cmd, cwd, env, timeout)` | `DirectSandbox.exec` | Supported (needs a guest agent) |
| `sandbox.process.code_run(code, params, timeout)` | `DirectSandbox.run_code` | Supported (needs a guest agent) |
| `sandbox.fs.upload_file(file, remote_path)` | `DirectSandbox.files.write` | Supported (needs a guest agent) |
| `sandbox.fs.download_file(remote_path[, local_path])` | `DirectSandbox.files.read_bytes` | Supported (needs a guest agent) |
| `sandbox.fs.list_files(path)` | `DirectSandbox.files.list` | Supported (needs a guest agent) |
| `sandbox.fs.create_folder(path, mode)` | `DirectSandbox.files.mkdir` | Supported (needs a guest agent) |
| `sandbox.fs.delete_file(path)` | `DirectSandbox.files.remove` | Supported (needs a guest agent) |
| `sandbox.fs.get_file_info(path)` | `DirectSandbox.files.list` lookup | Supported (needs a guest agent) |
| `sandbox.get_preview_link(port)` | preview URLs | Supported (needs the preview proxy deployed) |

"needs a guest agent" means the op runs end-to-end only against a real guest over
vsock (a KVM deployment, or `mitos dev up` with the engine). The bare mock-engine
sandbox-server answers the create / get / list / delete / start / stop lifecycle
but cannot run process / fs, which is why the test suite proves those against a
fake target and runs them end-to-end in the KVM CI job.

## The vocabulary renames

- `fs.upload_file` / `fs.download_file` map onto `files.write` / `files.read`.
- `fs.create_folder` maps onto `files.mkdir`; `fs.delete_file` onto `files.remove`.
- `start` / `stop` map onto the native `resume` / `pause`.

`DirectSandbox.exec` is shell based and takes no `cwd` / `env` argument, so
`process.exec(cmd, cwd=..., env=...)` folds those into the command
(`export K=V; cd DIR; CMD`) rather than dropping them. `env_vars` and `labels` on
create are accepted for signature parity and not applied today (no server slot
yet).

## get_preview_link returns a signed preview URL

`sandbox.get_preview_link(port)` returns a `PortPreviewUrl` with `.url` and
`.token`, served by the per-sandbox preview reverse proxy. The server mints the
URL (the signing secret stays server-side) and the token expires, so treat the
URL value as a credential. A server that does not expose the preview proxy raises
a typed `AgentRunError` from the call rather than fabricating a URL that would not
resolve.

## What you gain beyond Daytona parity

The underlying `DirectSandbox` (reachable via `sandbox.sandbox`, also surfaced as
`sandbox.fork(n)`) exposes a Mitos superpower Daytona does not: `fork(n)` for
branching a warm, mid-task machine into many copies at fork(2) speeds, plus
`pause` / `resume` and an interactive `pty`. The shim keeps your Daytona script
working; reach for the fork when you want best-of-N, evals, or tree search from
one shared starting state.
