# Give Claude Code a forkable computer (5 minutes)

Claude Code's built-in sandbox is per-machine and OS-level. Mitos adds a
Kubernetes-native, multi-tenant **microVM** with a fork primitive: Claude Code
runs agent-generated code in a Firecracker microVM on your own cluster, and can
fork a warm sandbox before a risky edit so a bad change is thrown away, not
rolled back.

Claude Code plugs in through the Mitos **MCP server** (`mitos-mcp`) and the Mitos
**agent skill** (`skills/mitos/SKILL.md`). This walkthrough wires both.

## Prerequisites

- Claude Code installed (`claude`).
- A Mitos sandbox backend, one of:
  - the hosted endpoint `https://mitos.run` (set `MITOS_API_KEY`), or
  - your self-hosted cluster's `sandbox-server` / gateway URL, or
  - a local standalone `sandbox-server` for trying it out:
    `go run ./cmd/sandbox-server --mock --addr :8080` (mock engine, no KVM), or a
    real-mode server on a KVM host (`--kernel ... --rootfs ... --agent-bin ...`).
- The `mitos-mcp` binary on your `PATH`:
  `go install mitos.run/mitos/cmd/mitos-mcp@latest`.

## 1. Add the Mitos MCP server to Claude Code

`mitos-mcp` is a stdio MCP server: Claude Code launches it and talks JSON-RPC
over stdin/stdout. Add it with `claude mcp add`, passing the backend URL and
token as environment variables:

```bash
claude mcp add mitos \
  --env MITOS_BASE_URL=https://mitos.run \
  --env MITOS_API_KEY=your-scoped-token \
  -- mitos-mcp
```

For a local standalone server, point it at `http://localhost:8080` and drop the
token (the standalone server is tokenless):

```bash
claude mcp add mitos --env MITOS_BASE_URL=http://localhost:8080 -- mitos-mcp
```

> `mitos-mcp` speaks stdio, not HTTP; use the command form above (`-- mitos-mcp`),
> not `--transport http`. Token resolution follows the SDK precedence: `--env
> MITOS_API_KEY`, then `~/.config/mitos/credentials.json` from `mitos auth login`,
> then tokenless.

Confirm Claude Code connected to the server:

```bash
claude mcp get mitos     # Status: Connected
claude mcp list          # mitos: mitos-mcp - Connected
```

Inside a session, `/mcp` lists the server and its tools.

## 2. The tools Claude Code gets

The server advertises the sandbox lifecycle as MCP tools:

| Tool | Arguments | Purpose |
|---|---|---|
| `sandbox_create` | `pool` | Claim a sandbox from a pool / template; returns its id |
| `sandbox_exec` | `sandbox`, `command`, `timeout_seconds?` | Run a shell command; returns `{exit_code, stdout, stderr}` |
| `sandbox_read_file` | `sandbox`, `path` | Read a file |
| `sandbox_write_file` | `sandbox`, `path`, `content` | Write a file |
| `sandbox_fork` | `sandbox`, `replicas?` | Fork a live sandbox into isolated copies (cluster / forkd) |
| `sandbox_terminate` | `sandbox` | Terminate a sandbox |

Every failure is a structured envelope, `{code, cause, remediation}`, so the
agent can branch on it instead of parsing prose.

> The `pool` you pass to `sandbox_create` must already exist: a `SandboxPool` in
> a cluster, or a template on the standalone server (`POST /v1/templates`). On the
> standalone `sandbox-server` the fork primitive is pool to sandbox, so use
> `sandbox_create` per attempt there; `sandbox_fork` forks a live sandbox on the
> cluster / forkd path.

## 3. Run agent-generated code in a microVM

Ask Claude Code to use the sandbox. A prompt like:

> Create a sandbox from the `python` pool, write a script that computes the 20th
> Fibonacci number, run it, and show me the output. Then terminate the sandbox.

drives `sandbox_create` -> `sandbox_write_file` -> `sandbox_exec` ->
`sandbox_terminate`. The code never touches your laptop or the cluster host: it
runs inside a Firecracker microVM with its own kernel.

## 4. Fork before a risky edit

The fork primitive is the reason to reach for Mitos over a local sandbox. On a
cluster, ask Claude Code to fork the warm sandbox before a destructive change:

> Fork this sandbox into 3 copies. In each, try a different refactor of
> `app/handler.py`, run the tests, and tell me which fork passed. Keep that one
> and terminate the others.

Each fork is an independent microVM restored from the same warm snapshot in
milliseconds (copy-on-write: you pay for the pages each fork dirties). A crash,
a fork bomb, or an `rm -rf` in one fork cannot touch its siblings, the host, or
the control plane, so it is safe to run code the model just wrote.

## 5. The agent skill

`skills/mitos/SKILL.md` teaches the workflow the tools enable: when to fork
versus start fresh, the best-of-N loop, the microVM isolation guarantee, and the
cost model (copy-on-write, discard losers promptly, capability budgets that bound
a runaway fan-out). Install it as an
[agent skill](https://docs.claude.com/en/docs/claude-code/skills) so Claude Code
applies the fork-vs-fresh and cost reasoning, not just the raw tool calls.

## What is verified

This path was exercised end to end against a real Firecracker microVM (a
standalone real-mode `sandbox-server` on a KVM host):

- `claude mcp add` with the stdio command form registers the server, and
  `claude mcp get` / `claude mcp list` report it **Connected** (Claude Code
  completed the MCP `initialize` handshake against `mitos-mcp`).
- Driving the advertised tools runs against the live VM: `sandbox_create` claims
  a real microVM, `sandbox_exec` returns a real `{exit_code, stdout}`,
  `sandbox_write_file` then `sandbox_read_file` round-trips file content, and
  `sandbox_terminate` reaps it.
- The agent skill's examples were corrected to match the current SDK surface
  (direct `mitos.create` for the hosted / standalone loop; the `AgentRun`
  cluster surface for durable-workspace commit).

The model-driving step (Claude Code choosing the tools from a prompt) uses your
Claude Code session. The MCP server's protocol conformance and the real-microVM
exec path are gated in CI (the MCP conformance test and the `firecracker-test`
job).
