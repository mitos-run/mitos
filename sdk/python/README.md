# mitos Python SDK

Python client for [mitos-run/mitos](https://github.com/mitos-run/mitos):
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
else `MITOS_API_KEY`, else the CLI login credential file written by
`mitos auth login`, so one login authenticates the SDK too) and base URL
(argument, else `MITOS_BASE_URL`) and returns a `DirectSandbox` exposing `exec`,
`run_code`, `files`, `pty`, `fork`, and `terminate`. `Sandbox.create(...)` is an
alias for the same call.

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

## Integrations

Framework adapters live under `mitos.integrations`. Each maps a framework's
sandbox-backend interface onto the native SDK ops (exec, files, run_code, fork),
so you change the backend and keep your agent code. The framework is an OPTIONAL
dependency: the adapter modules import mitos always and the framework lazily, so
the base SDK installs and tests without it.

### LangChain / deepagents quickstart

LangChain and deepagents let you pick a pluggable sandbox backend (they ship
`E2BSandbox` and `DaytonaSandbox`). `MitosSandbox` is the mitos backend: change
the backend, keep your agent code.

```python
from mitos.integrations.langchain import MitosSandbox

# Standalone sandbox-server / hosted control plane, no Kubernetes:
sb = MitosSandbox.create("python", base_url="http://localhost:8080")

# Shell command -> normalized result dict (stdout, stderr, exit_code).
out = sb.execute("echo hi")          # alias: sb.run("echo hi")

# Filesystem ops.
sb.write_file("/workspace/a.txt", "hello")
print(sb.read_file("/workspace/a.txt"))
print(sb.list_files("/workspace"))

# Code execution with rich MIME results (image/png, text/html, ...).
ex = sb.run_code("import math; math.sqrt(144)")
print(ex.text, ex.results)

sb.close()                           # alias: sb.stop(); lifecycle close
```

Install the optional extra only if you use the integration:
`pip install "mitos[langchain]"`. `MitosSandbox` does not subclass any langchain
type and is fully usable without langchain installed.

Fork is a mitos superpower the LangChain sandbox-backend interface does not
expose, so it is NOT forced onto that interface. Reach branching / parallel runs
through the adapter's native `fork`, which returns sibling `MitosSandbox`
backends:

```python
children = sb.fork(2)                # two independent forked sandboxes
for c in children:
    print(c.execute("echo from-fork")["stdout"])
    c.close()
```

The wire-op mapping is factored into `mitos.integrations._mapping` so the other
adapters (OpenAI / Claude, the E2B-compat shim) reuse one translation layer.

### OpenAI Agents SDK quickstart

The OpenAI Agents SDK exposes tools as function tools. `MitosSandboxTools` binds
`run_command` / `read_file` / `write_file` / `run_code` to a mitos sandbox: give
the tools to your agent and its tool calls run inside the sandbox.

```python
from mitos.integrations.openai_agents import MitosSandboxTools

# Standalone sandbox-server / hosted control plane, no Kubernetes:
tools = MitosSandboxTools.create("python", base_url="http://localhost:8080")

# Use the thin wrappers directly:
print(tools.run_command("echo hi")["stdout"])
tools.write_file("/workspace/a.txt", "hello")
print(tools.read_file("/workspace/a.txt"))
print(tools.run_code("import math; math.sqrt(144)")["text"])

# Or hand real function tools to an Agent (needs the SDK installed):
from agents import Agent
agent = Agent(name="coder", tools=tools.as_function_tools())

tools.close()
```

Install the optional extra only if you use the integration:
`pip install "mitos[openai-agents]"`. The adapter is fully usable and testable
without `openai-agents`; only `as_function_tools()` needs it and it raises a
clear error naming the extra when absent.

### Claude Agent SDK quickstart

The Claude Agent SDK takes custom tools as an in-process MCP server.
`MitosSandboxTools` binds the same four tools to a mitos sandbox and wraps them
as an MCP server you pass to the agent.

```python
from mitos.integrations.claude_agent import MitosSandboxTools

# Standalone sandbox-server / hosted control plane, no Kubernetes:
tools = MitosSandboxTools.create("python", base_url="http://localhost:8080")

# The handlers return MCP tool results (a content list of text blocks):
res = tools.run_command({"command": "echo hi"})
print(res["content"][0]["text"])
tools.write_file({"path": "/workspace/a.txt", "content": "hello"})
print(tools.read_file({"path": "/workspace/a.txt"})["content"][0]["text"])

# Or build a real in-process MCP server (needs the SDK installed):
server = tools.as_mcp_server(name="mitos-sandbox")  # pass via mcp_servers

tools.close()
```

Install the optional extra only if you use the integration:
`pip install "mitos[claude-agent]"`. The adapter is fully usable and testable
without `claude-agent-sdk`; only `as_mcp_server()` needs it and it raises a clear
error naming the extra when absent.

### Use mitos via VibeKit

VibeKit is a provider aggregator over sandbox backends ("E2B today; Daytona,
Modal, Fly.io coming soon"). `MitosVibeKitProvider` is the mitos provider against
VibeKit's provider shape: a named provider that creates sandboxes exposing
command execution, a filesystem, and a lifecycle.

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

Install the optional extra only if you use the integration:
`pip install "mitos[vibekit]"`. The provider does not subclass any VibeKit type
and is fully usable and testable without VibeKit installed. Fork stays reachable
as a mitos-native op via `sandbox.fork(n)`, which VibeKit's provider interface
does not expose.

### Use mitos via ZenML

ZenML treats a sandbox as a pluggable stack component selected by a flavor.
`MitosSandboxComponent` is the framework-neutral mitos backend (config, flavor
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

Install the optional extra only if you use the integration:
`pip install "mitos[zenml]"`. The component backend is fully usable and testable
without ZenML; only `MitosSandboxComponent.flavor()` needs it and raises a clear
error naming the extra when absent.

### Registering mitos with VibeKit / ZenML (maintainer step)

The adapters above implement the mitos backend each aggregator expects, but
LISTING mitos as a selectable option inside VibeKit or ZenML is a contribution to
THOSE projects' own repositories and is NOT done by installing this SDK. It is a
maintainer step:

- **VibeKit:** open a PR to VibeKit adding a provider entry that constructs
  `MitosVibeKitProvider` and wires its create / command / filesystem methods to
  VibeKit's provider interface, following VibeKit's contribution process.
- **ZenML:** open a PR (or ship a plugin) that subclasses ZenML's sandbox
  stack-component base with `MitosSandboxConfig` and `FLAVOR = "mitos"`, delegates
  its hooks to `MitosSandboxComponent`, and registers the flavor through ZenML's
  flavor API, following ZenML's integration contribution process.

Until those external PRs merge, mitos is usable through the adapters directly (as
shown above); it is not yet selectable by name inside VibeKit's or ZenML's own
provider lists. See the module docstrings in `mitos/integrations/vibekit.py` and
`mitos/integrations/zenml.py` for the targeted contract each PR conforms to.

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

See the [repository README](https://github.com/mitos-run/mitos#readme)
for project status; this SDK is pre-alpha and its API may change.
