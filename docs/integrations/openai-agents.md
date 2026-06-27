# Give the OpenAI Agents SDK a Mitos sandbox

The [OpenAI Agents SDK](https://openai.github.io/openai-agents-python/) (the
`openai-agents` package) lets an agent call tools you define as function tools.
Mitos ships a native adapter, `MitosSandboxTools`, that binds four sandbox tools
(`run_command`, `read_file`, `write_file`, `run_code`) to a Mitos snapshot-fork
microVM, so any Agents SDK agent can run and edit code inside a hardware-isolated
sandbox on your own infrastructure.

This is an adapter over the existing SDK ops (`exec`, `files`, `run_code`); it
needs no Kubernetes (it runs against the standalone `sandbox-server` or the
hosted control plane) and it imports `openai-agents` lazily, so the rest of your
code works whether or not the SDK is installed.

## Install

```bash
pip install "mitos-run[openai-agents]"
```

The `[openai-agents]` extra pulls in the `openai-agents` package. The base
`mitos-run` install already carries the adapter module; only the
`as_function_tools()` conversion needs the SDK.

## Quickstart

Point the adapter at a sandbox backend with `MITOS_BASE_URL` (a standalone
`sandbox-server`, for example `http://localhost:8080`, or the hosted
`https://mitos.run`) and `MITOS_API_KEY` (or run `mitos auth login` once; the
standalone server runs tokenless).

```python
from agents import Agent, Runner
from mitos.integrations.openai_agents import MitosSandboxTools

# Build a READY sandbox and bind the four tools to it (no Kubernetes).
tools = MitosSandboxTools.create("python", base_url="http://localhost:8080")

agent = Agent(
    name="coder",
    instructions=(
        "You run and edit code inside a sandbox. Use run_command for shell, "
        "run_code for snippets, and read_file / write_file for files."
    ),
    tools=tools.as_function_tools(),
)

result = Runner.run_sync(agent, "Write hello.py that prints 42, then run it.")
print(result.final_output)

tools.close()  # terminate the sandbox
```

`MitosSandboxTools` is also a context manager (`with MitosSandboxTools.create(...)
as tools:`) and exposes the underlying `DirectSandbox` as `tools.sandbox` for the
Mitos-only operations (`fork`, `pause`/`resume`, PTY, preview URLs).

The four tools map to native sandbox ops:

| Tool | Maps to | Returns |
|---|---|---|
| `run_command(command, timeout=30)` | `sandbox.exec` | `{stdout, stderr, exit_code, exec_time_ms}` |
| `read_file(path)` | `sandbox.files.read` | file contents (string) |
| `write_file(path, content)` | `sandbox.files.write` | `{path, written}` |
| `run_code(code, language="python")` | `sandbox.run_code` | flattened `Execution` (text, logs, rich results, error) |

`run_code` needs a language kernel in the sandbox image; against a minimal image
it returns a fail-closed `KernelUnavailable` until the kernel ships in the base
image. `run_command` / `read_file` / `write_file` work on any image with a shell.

## Why fork

Because each sandbox is a Firecracker microVM, you can fork a warm one into N
isolated attempts and let the agent explore in parallel (best-of-N), then keep
the winner. Reach the fork primitive through the underlying handle:

```python
tools = MitosSandboxTools.create("python", base_url="http://localhost:8080")
children = tools.sandbox.fork(4)        # 4 isolated microVMs from the warm base
# give each child its own agent / attempt, score them, keep the winner
```

See the [Mitos agent skill](../../skills/mitos/SKILL.md) for the full
fork-vs-fresh and best-of-N workflow.

## Codex: the honest path

The **Codex CLI** is closed and runs the agent on OpenAI's own containers; its
sandbox is not swappable. There is no supported way to point the Codex CLI at a
Mitos sandbox, and we do not document a workaround that does not exist.

The real path into the OpenAI ecosystem is the **OpenAI Agents SDK** shown above
(it is open and takes a Mitos sandbox as a tool). If you are using a Codex model,
drive it through the Agents SDK with these tools rather than the CLI.

## What is verified

The adapter's tool layer is exercised end to end against a real Firecracker
microVM (a standalone real-mode `sandbox-server` on a KVM host):

- `MitosSandboxTools.create(...)` builds a ready sandbox; `function_tools()` and
  `as_function_tools()` return the four tools, and `as_function_tools()`
  constructs real `agents.FunctionTool` objects when `openai-agents` is
  installed.
- Invoking the tool callables runs against the live VM: `write_file` then
  `read_file` round-trips file content, and `run_command` returns real stdout
  and a real exit code.

The model-driving step (an `Agent` actually selecting and calling these tools)
uses your own OpenAI credentials, exactly as the Agents SDK documents. The adapter
unit tests live in `sdk/python/tests/test_openai_agents_integration.py` and run in
CI; the real-microVM exec path is gated by the `firecracker-test` job.
