# Mitos Python SDK

Mitos gives AI agents isolated, forkable sandboxes: Firecracker microVMs that
restore from snapshots and fork into parallel attempts, so an agent can branch a
warm environment instead of rebuilding it. Run it fully hosted at
[https://mitos.run](https://mitos.run) or self-hosted on your own Kubernetes
cluster.

This is the Python client. It covers both modes: the direct sandbox API (hosted
or standalone, no Kubernetes) and cluster mode driving the Kubernetes CRDs, plus
an async client and framework adapters.

## Install

```bash
pip install mitos-run
```

The PyPI distribution is named `mitos-run`; the import package is `mitos`. You
`pip install mitos-run` and `import mitos`. Optional extras keep the same import
name, for example `pip install "mitos-run[k8s]"` for cluster mode.

## Quickstart (hosted)

Get an API key from [https://mitos.run](https://mitos.run) and set it in the
environment. The base URL defaults to the hosted endpoint, so create, exec, and
terminate work with no further configuration.

```python
import mitos

# MITOS_API_KEY from the environment; base URL defaults to https://mitos.run.
sb = mitos.create("python")
print(sb.exec("echo hello").stdout)              # hello
sb.terminate()
```

Fork is the differentiator: branch a warm sandbox into independent siblings and
run parallel attempts against the same starting state.

```python
import mitos

sb = mitos.create("python")
sb.files.write("/workspace/plan.txt", "draft")

fork_a, fork_b = sb.fork(2)                       # two independent siblings
fork_a.exec("echo a > /workspace/a.txt")
fork_b.exec("echo b > /workspace/b.txt")          # b does not see a's write

ex = sb.run_code("import math; math.sqrt(144)")
print(ex.text)                                    # 12.0

sb.terminate()
```

This hero is the checked example `examples/quickstart.py`: CI byte-compiles and
import-checks it against the real SDK, so this snippet cannot drift.

`mitos.create(image, api_key=..., base_url=...)` resolves auth (see precedence
below), gets-or-creates the template for `image`, forks it, and returns a READY
`DirectSandbox` exposing `exec`, `run_code`, `files`, `pty`, `fork`,
`set_timeout`, `pause`, `resume`, `get_host`, and `terminate`.
`Sandbox.create(...)` is an alias for the same call.

### Async direct path

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

## Direct-mode surface

`mitos.create` / `Sandbox.create` returns a `DirectSandbox`.

The runtime calls (exec, run_code, files) ride the Connect `sandbox.v1.Sandbox`
service at `/sandbox.v1.Sandbox/<Method>`; the control-plane calls
(templates, fork, sandboxes) and the interactive PTY keep their `/v1/*`
transports. The sandbox is addressed by id via the `X-Sandbox-Id` header.

| Method | Wire | Returns |
| --- | --- | --- |
| `mitos.create(image, ...)` | `POST /v1/templates`, `POST /v1/fork` | `DirectSandbox` |
| `sandbox.exec(command, timeout=30)` | `ExecStream` (server-stream) | `ExecResult` |
| `sandbox.run_code(code, language="python", ...)` | `RunCodeStream` (server-stream) | `Execution` |
| `sandbox.files.read(path)` / `read_bytes(path)` | `ReadFile` (server-stream) | `str` / `bytes` |
| `sandbox.files.write(path, content, mode=0o644)` | `WriteFile` (client-stream) | `None` |
| `sandbox.files.list(path="/")` | `List` (unary) | `list[FileInfo]` |
| `sandbox.files.exists(path)` | `List` (unary) | `bool` |
| `sandbox.files.remove(path)` | `Remove` (unary) | `None` |
| `sandbox.files.mkdir(path)` | `Mkdir` (unary) | `None` |
| `sandbox.pty(on_data, cols=80, rows=24)` | `WS /v1/pty` | `PtyHandle` |
| `sandbox.set_timeout(timeout_seconds)` | `POST /v1/set_timeout` | `int` (deadline) |
| `sandbox.pause()` / `sandbox.resume()` | `POST /v1/pause`, `/v1/resume` | `None` |
| `sandbox.get_host(port=80)` | `POST /v1/preview` | `str` (signed URL) |
| `sandbox.fork(n=1)` | `POST /v1/fork` | `list[DirectSandbox]` |
| `sandbox.terminate()` | `DELETE /v1/sandboxes/{id}` | `None` |

The lower-level `SandboxServer` exposes the same endpoints when you want to drive
templates and forks explicitly:

```python
from mitos.direct import SandboxServer

server = SandboxServer("http://localhost:8080")
server.create_template("python")
sandbox = server.fork("python")
print(sandbox.exec("python -c 'print(1 + 1)'").stdout)
sandbox.terminate()
```

## Cluster mode: AgentRun

Cluster mode drives the Kubernetes CRDs (`SandboxPool`, `Sandbox`, `Workspace`)
in API group `mitos.run/v1` and execs through the forkd sandbox API. It is for
operators who run the Mitos cluster themselves.

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
`mitos-default-python` (a `SandboxPool` carrying the image in its inline
`spec.template`), creating it if absent. It is
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

### Async cluster client

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
sandbox(image)). Pool and workspace administration are sync-only.

## Auth and base URL precedence

Resolution order, highest precedence first:

- API key: the `api_key` argument, then `MITOS_API_KEY`, then the bearer token in
  the credential file written by `mitos auth login`
  (`~/.config/mitos/credentials.json`, honoring `MITOS_CONFIG_DIR`), then
  tokenless.
- Base URL: the `base_url` argument, then `MITOS_BASE_URL`, then the hosted
  default `https://mitos.run`.

The key rides on `Authorization: Bearer <key>` on every request. A standalone
`sandbox-server` runs tokenless and ignores it; the hosted endpoint verifies it.
The key value is never logged and never placed in an error message. A missing
base URL raises a typed `AgentRunError(code="missing_base_url")` whose
remediation names the argument and the env var, never a key value.

## Errors

Failures raise `AgentRunError`, parsed from the server envelope
`{error:{code, message, cause, remediation}}`. Branch on `code`, never on the
message text.

```python
from mitos import AgentRunError

try:
    sb.files.read("/does/not/exist")
except AgentRunError as e:
    print(e.code)         # e.g. file_failed, not_found
    print(e.remediation)  # an actionable next step
```

Any bearer token a misconfigured server reflects into a body is redacted before
it becomes the error cause.

## Sandbox ids

Sandbox ids must match `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`. The SDK validates the
id (the explicit one or the generated `sandbox-<hex>`) and raises
`invalid_sandbox_id` before sending any request.

## Templates as code

Author a custom environment from code with the fluent `Template` builder. It
emits a pool template spec (the inline `spec.template` of a `SandboxPool`); no
server or KVM is needed to build the spec.

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
steps are reused. See the [docs](https://mitos.run) for the CLI
(`mitos template build` / `push`) and the cache semantics.

## Fork-native subagents

When a multi-agent harness runs INSIDE a mitos sandbox, spawning a subagent IS
forking the current warm sandbox: the child is a copy-on-write snapshot of the
live parent, so it starts warm with the parent's state, with no cold boot and no
external orchestrator round-trip. `mitos.subagent` is the small hook that does
this, with a graceful no-op fallback when the code is not on mitos.

```python
import mitos.subagent as sub

result = sub.spawn_subagent(3, label="researcher")
if result.on_mitos:
    for child in result.children:
        print("warm subagent:", child.sandbox_id)   # forked from this sandbox
else:
    print("off mitos:", result.reason)              # fall back to your normal spawn
```

- Detection signal: the in-guest self-service socket advertised at
  `$MITOS_SOCKET` (see `mitos.guest`). The host sets it at claim time only inside
  a mitos sandbox, so its presence is the honest "am I running inside mitos"
  check. `sub.is_on_mitos()` is that pure check; `sub.current_sandbox()` returns
  the sandbox's own identity (or `None` off mitos).
- On mitos: `spawn_subagent(n, label=...)` routes the spawn through the
  budget-gated self-fork on that socket (`mitos.guest.fork`), no network egress
  and no API credentials, and returns a `SubagentResult` with `on_mitos=True` and
  a `SubagentHandle` per warm child.
- Off mitos: it returns `on_mitos=False` with an explanatory `reason`, does NOT
  call fork, and never raises. The fallback path is first-class so a harness
  using this hook does not break off-platform.

Honest scope: the warm fork needs a running mitos sandbox context. The
budget-gated self-fork is wired progressively by the guest agent; where it is not
yet enabled the socket returns its remediation as a `mitos.guest.GuestError`,
which is a real on-mitos error rather than the off-mitos fallback. The handles
carry the child sandbox names; driving a child from another process needs
control-plane credentials, which is a documented follow-up. See
`examples/fork_native_subagent.py`.

## Integrations

Framework adapters live under `mitos.integrations`. Each maps a framework's
sandbox-backend interface onto the native SDK ops (exec, files, run_code, fork),
so you change the backend and keep your agent code. The framework is an OPTIONAL
dependency: the adapter modules import mitos always and the framework lazily, so
the base SDK installs and tests without it.

### LangChain / deepagents

LangChain and deepagents let you pick a pluggable sandbox backend (they ship
`E2BSandbox` and `DaytonaSandbox`). `MitosSandbox` is the Mitos backend: change
the backend, keep your agent code.

```python
from mitos.integrations.langchain import MitosSandbox

sb = MitosSandbox.create("python", base_url="http://localhost:8080")

out = sb.execute("echo hi")          # alias: sb.run("echo hi")
sb.write_file("/workspace/a.txt", "hello")
print(sb.read_file("/workspace/a.txt"))
print(sb.list_files("/workspace"))

ex = sb.run_code("import math; math.sqrt(144)")
print(ex.text, ex.results)

children = sb.fork(2)                # fork stays reachable as a native op
for c in children:
    c.close()

sb.close()                           # alias: sb.stop()
```

Install the extra only if you use the integration:
`pip install "mitos-run[langchain]"`. `MitosSandbox` does not subclass any
langchain type and is fully usable without langchain installed. Fork is a Mitos
op the LangChain backend interface does not expose, so it is reached through the
adapter's native `fork`, which returns sibling `MitosSandbox` backends.

For LangChain's **deepagents**, which takes a pluggable `backend=` (the same slot
as `E2BSandbox` / `DaytonaSandbox`), wrap a Mitos sandbox with
`as_deepagents_backend(...)`. It returns a real `deepagents` `BaseSandbox`
subclass whose `execute()` returns an `ExecuteResponse`, so the agent's shell and
file tools run in a Mitos sandbox, swap providers by swapping this one object:

```python
from deepagents import create_deep_agent
from langchain_anthropic import ChatAnthropic
from mitos.integrations.langchain import MitosSandbox, as_deepagents_backend

backend = as_deepagents_backend(MitosSandbox.create("python", base_url="http://localhost:8080"))

agent = create_deep_agent(
    model=ChatAnthropic(model="claude-sonnet-4-6"),
    system_prompt="You are a Python coding assistant with sandbox access.",
    backend=backend,
)
```

`as_deepagents_backend` imports `deepagents` lazily, so the base SDK keeps no hard
dependency on it; the `[langchain]` extra pulls it in.

### OpenAI Agents SDK

`MitosSandboxTools` binds `run_command` / `read_file` / `write_file` /
`run_code` to a Mitos sandbox: give the tools to your agent and its tool calls
run inside the sandbox.

```python
from mitos.integrations.openai_agents import MitosSandboxTools

tools = MitosSandboxTools.create("python", base_url="http://localhost:8080")

print(tools.run_command("echo hi")["stdout"])
tools.write_file("/workspace/a.txt", "hello")
print(tools.read_file("/workspace/a.txt"))
print(tools.run_code("import math; math.sqrt(144)")["text"])

from agents import Agent
agent = Agent(name="coder", tools=tools.as_function_tools())

tools.close()
```

Install the extra only if you use the integration:
`pip install "mitos-run[openai-agents]"`. The adapter is fully usable and
testable without `openai-agents`; only `as_function_tools()` needs it and it
raises a clear error naming the extra when absent.

### Claude Agent SDK

The Claude Agent SDK takes custom tools as an in-process MCP server.
`MitosSandboxTools` binds the same four tools to a Mitos sandbox and wraps them
as an MCP server you pass to the agent.

```python
from mitos.integrations.claude_agent import MitosSandboxTools

tools = MitosSandboxTools.create("python", base_url="http://localhost:8080")

res = tools.run_command({"command": "echo hi"})
print(res["content"][0]["text"])
tools.write_file({"path": "/workspace/a.txt", "content": "hello"})
print(tools.read_file({"path": "/workspace/a.txt"})["content"][0]["text"])

server = tools.as_mcp_server(name="mitos-sandbox")  # pass via mcp_servers

tools.close()
```

Install the extra only if you use the integration:
`pip install "mitos-run[claude-agent]"`. The adapter is fully usable and
testable without `claude-agent-sdk`; only `as_mcp_server()` needs it and it
raises a clear error naming the extra when absent.

### VibeKit

`MitosVibeKitProvider` is the Mitos provider against VibeKit's provider shape: a
named provider that creates sandboxes exposing command execution, a filesystem,
and a lifecycle.

```python
from mitos.integrations.vibekit import MitosVibeKitProvider

provider = MitosVibeKitProvider(base_url="http://localhost:8080")  # name == "mitos"
sandbox = provider.create("python")

out = sandbox.run_command("echo hi")     # {stdout, stderr, exit_code, exec_time_ms}
sandbox.write_file("/workspace/a.txt", "hello")
print(sandbox.read_file("/workspace/a.txt"))
ex = sandbox.run_code("1 + 1")           # rich Execution with MIME results
sandbox.kill()                            # alias: sandbox.close()
```

Install the extra only if you use the integration:
`pip install "mitos-run[vibekit]"`. The provider does not subclass any VibeKit
type and is fully usable without VibeKit installed. Fork stays reachable as a
mitos-native op via `sandbox.fork(n)`.

### ZenML

`MitosSandboxComponent` is the framework-neutral Mitos backend (config, flavor
name, and the provision / run_command / files / run_code / deprovision logic the
flavor wraps).

```python
from mitos.integrations.zenml import MitosSandboxComponent, MitosSandboxConfig

comp = MitosSandboxComponent(
    MitosSandboxConfig(template="python", base_url="http://localhost:8080")
)  # FLAVOR == "mitos"
comp.provision()
out = comp.run_command("echo hi")        # {stdout, stderr, exit_code, exec_time_ms}
comp.write_file("/workspace/a.txt", "hello")
ex = comp.run_code("1 + 1")              # rich Execution; comp.run_code_dict(...) for JSON
comp.deprovision()
```

Install the extra only if you use the integration:
`pip install "mitos-run[zenml]"`. The component backend is fully usable and
testable without ZenML; only `MitosSandboxComponent.flavor()` needs it and
raises a clear error naming the extra when absent.

Listing Mitos as a selectable provider inside VibeKit or ZenML is a contribution
to those projects' own repositories, not something installing this SDK does. The
adapters above implement the backend each aggregator expects; you use them
directly today, and the module docstrings spell out the contract each upstream
contribution would conform to.

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

## License

Apache-2.0.
</content>
</invoke>
