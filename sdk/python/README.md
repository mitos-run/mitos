# mitos Python SDK

Python client for [paperclipinc/mitos](https://github.com/paperclipinc/mitos):
snapshot-fork sandboxes for AI agents on Kubernetes.

Two modes:

- `mitos.create` (flat one-liner): API key plus base URL, returns a Ready
  sandbox handle against the hosted control plane or a standalone
  `sandbox-server`. No Kubernetes required. The canonical entry point.
- `mitos.AgentRun` / `Sandbox`: drives the Kubernetes CRDs
  (`SandboxClaim`, `SandboxFork`, `SandboxPool`, `SandboxTemplate`) and execs
  through the forkd sandbox API. For operators who run the mitos cluster.

## The flat one-liner

```python
import mitos

# MITOS_API_KEY and MITOS_BASE_URL from the environment (explicit args override).
sb = mitos.create("python")
print(sb.exec("echo hello").stdout)              # hello
sb.terminate()
```

`mitos.create(image, api_key=..., base_url=...)` resolves the API key (argument,
else `MITOS_API_KEY`) and base URL (argument, else `MITOS_BASE_URL`) and returns a
`DirectSandbox` exposing `exec`, `run_code`, `files`, `pty`, `fork`, and
`terminate`. `Sandbox.create(...)` is an alias for the same call.

```python
import mitos

sb = mitos.create("python", api_key="sk-...", base_url="http://localhost:8080")

sb.files.write("/workspace/plan.txt", "draft")
print(sb.files.read("/workspace/plan.txt"))      # draft

ex = sb.run_code("import math; math.sqrt(144)")
print(ex.text)                                   # 12.0

fork_a, fork_b = sb.fork(2)                       # independent sibling sandboxes
fork_a.exec("echo a > /workspace/a.txt")
fork_b.exec("echo b > /workspace/b.txt")

sb.terminate()
```

Auth: the API key rides on `Authorization: Bearer <key>` on every request. The
standalone `sandbox-server` runs tokenless and ignores it; the hosted control
plane verifies the same header server-side ([#210], not yet built) without an SDK
change. The key value is never logged and never placed in an error message. A
missing base URL raises a typed `AgentRunError(code="missing_base_url")` whose
remediation names the argument and the env var but no key value.

[#210]: https://github.com/mitos-run/mitos/issues/210

### Async flat path

```python
import asyncio
import mitos.aio

async def main():
    sb = await mitos.aio.create("python")        # AsyncDirectSandbox
    print((await sb.exec("echo hi")).stdout)
    await sb.files.write("/workspace/a.txt", "x")
    print(await sb.files.read("/workspace/a.txt"))
    ex = await sb.run_code("1 + 1")
    print(ex.text)
    children = await sb.fork(2)
    for c in children:
        await c.terminate()
    await sb.terminate()

asyncio.run(main())
```

## Cluster mode: the one-liner

```python
from mitos import AgentRun

c = AgentRun()                                   # kubeconfig or in-cluster; autodetected

sb = c.sandbox("python", ready=True)             # lazy default pool, waits Ready
print(sb.exec("python -c 'print(2 + 2)'").stdout)  # 4
sb.files.write("/workspace/notes.md", "# findings")
print(sb.files.read("/workspace/notes.md"))      # "# findings"
sb.terminate()
```

`c.sandbox("python")` ensures a deterministic default pool
`mitos-default-python` (a `SandboxTemplate` carrying the image plus a
`SandboxPool` that references it), creating both if absent. It is
admin-disableable with `AgentRun(allow_default_pool=False)`, which makes the
image path raise instead of creating anything.

### Explicit pool (never creates anything)

```python
sb = c.sandbox(pool="python-agent-pool")
```

### Fork a running sandbox

```python
forks = sb.fork(3)                               # 3 copies of the warmed state
for f in forks:
    print(f.exec("echo from-fork").stdout)
```

### Reconnect by name (durable handle across processes)

```python
sb = c.sandbox("python", name="agent-session-1", ready=True)
# ... later, in a different process:
sb = c.from_name("agent-session-1")
print(sb.exec("cat /workspace/notes.md").stdout)
```

### Readiness

```python
sb = c.sandbox("python").wait_until_ready()      # chainable; raises on Failed/timeout
```

### Streaming exec

```python
# Callbacks fire per chunk (bytes) as output arrives; the returned ExecResult
# still carries the full aggregate.
sb.exec("pytest -x", on_stdout=lambda b: print(b.decode(), end=""))

# A long-running background command with a handle.
bg = sb.exec_background("npm run dev")
# ... do other work ...
bg.kill()
```

### Structured errors

```python
from mitos import AgentRunError

try:
    sb.exec("false")
    sb.files.read("/does/not/exist")
except AgentRunError as e:
    print(e.code)         # e.g. file_failed, not_found
    print(e.remediation)  # an actionable next step
```

`AgentRunError` is parsed from the server envelope
`{error:{code, message, cause, remediation}}`. Any bearer token a misconfigured
server reflects into a body is redacted before it becomes the error cause.

### Async client (hot paths)

```python
import asyncio
from mitos import AsyncAgentRun

async def main():
    c = AsyncAgentRun()
    sb = await c.sandbox("python", ready=True)
    print((await sb.exec("echo async-hello")).stdout)
    await sb.files.write("/workspace/a.txt", "x")
    print(await sb.files.read("/workspace/a.txt"))
    forks = await sb.fork(2)
    for f in forks:
        await f.terminate()
    await sb.terminate()

asyncio.run(main())
```

`AsyncAgentRun` / `AsyncSandbox` cover the hot paths (exec blocking and
streaming, files, fork, terminate, wait_until_ready, from_name,
sandbox(image)). Pool and workspace administration are sync-only. If your build
includes the code interpreter (#102), `sb.run_code(...)` returns an `Execution`;
the async client does not yet wrap `run_code`.

## Direct mode (no Kubernetes)

```python
from mitos.direct import SandboxServer

server = SandboxServer("http://localhost:8080")
server.create_template("python")
sandbox = server.fork("python")
print(sandbox.exec("print(1 + 1)").stdout)
sandbox.terminate()
```

## Templates as code

Author a custom environment from code with the fluent `Template` builder, in the
shape E2B and Daytona use. It emits a `SandboxTemplate` spec; no server or KVM is
needed to build the spec.

```python
from mitos import Template

spec = (
    Template()
    .from_image("python:3.12")
    .copy("app/", "/app")
    .run("pip install -r requirements.txt")
    .set_start("python app.py")
    .to_spec()
)

# Or wrap it as a full object to apply to a cluster:
obj = Template().from_image("node:24").run("npm ci").to_template("web")
```

The ordered steps (copy / run / env / workdir) map onto the CRD
`spec.buildSteps` and feed a content-addressed, chained build cache so unchanged
steps are reused. See docs/templates.md for the CLI (`mitos template build` /
`push`) and the cache semantics.

## What is proven where

The cluster examples (lazy default-pool creation, fork, from_name reconnect,
readiness, async hot paths) run against the real Firecracker engine in the KVM
CI job. The structured-error parsing and the wire shapes are unit-tested with
no cluster. No latency or throughput number is claimed in this README.

## Development

```bash
pip install -e ".[dev]"
pytest tests/ -v
```

See the [repository README](https://github.com/paperclipinc/mitos#readme)
for project status; this SDK is pre-alpha and its API may change.
