---
name: mitos-sandboxes
description: Use when an agent needs isolated, forkable compute, running untrusted or model-written code safely, or exploring several attempts in parallel (best-of-N) and keeping the winner. Mitos boots Firecracker microVMs and forks a running VM via copy-on-write snapshots, so a fan-out is cheap and each attempt is hardware-isolated. Pairs with the mitos-mcp server (the tools) and the Python/TypeScript SDKs.
---

# Using Mitos snapshot-fork sandboxes

Mitos gives an agent isolated, forkable computers. Each sandbox is a Firecracker
microVM. Forking copies a running VM with copy-on-write, so spawning N attempts
from a warm base is fast and you pay only for the memory each attempt dirties.

This skill teaches the workflow. To actually drive sandboxes, use the
`mitos-mcp` MCP server (tools) or an SDK (see Surfaces). Do not parse error
prose: every failure is a structured envelope, branch on `code` and follow
`remediation` (see `llms.txt`).

## Connecting

The hosted production Mitos is at `https://mitos.run`. The simplest path is one
login: run `mitos auth login --token <session>` once and it writes a shared
credential file (`~/.config/mitos/credentials.json`, honoring `MITOS_CONFIG_DIR`).
The Python and TypeScript SDKs, the `mitos` CLI, and `mitos-mcp` all pick up the
bearer token from that file, so one login authenticates every agent-facing
surface. The token resolution precedence is: an explicit argument (the SDK
`api_key` / the `--token` flag), then `MITOS_API_KEY`, then the credential file,
then none (the standalone `sandbox-server` runs tokenless). The credential
file's token is sent as the `Authorization: Bearer` value and the hosted gateway
decides its validity; if your deployment requires an API key minted with
`mitos auth keys create`, set that as `MITOS_API_KEY` or pass it explicitly.

All three SDK/mcp surfaces DEFAULT to the hosted endpoint, so the examples below
need no base URL. To target a self-hosted cluster or a local standalone
`sandbox-server`, set `MITOS_BASE_URL` (for example `http://localhost:8080`); it
overrides the hosted default. The `mitos` CLI drives a Kubernetes cluster and
resolves its connection from your kubeconfig.

## When to fork vs. start fresh

- Start fresh (`create`) when there is no shared setup to inherit: a clean
  environment for one task.
- Fork (`fork`) when several attempts should share the same warm, set-up state:
  dependencies installed, repo cloned, a server running. The fork inherits that
  state in milliseconds instead of rebuilding it N times.

## The core loop: best-of-N

Warm one base, fan out, run attempts in parallel, keep the winner, discard the rest.

```python
import mitos

# Warm one base sandbox (hosted endpoint or a standalone sandbox-server;
# resolves MITOS_BASE_URL and the credential file).
base = mitos.create("python")

# Fan out 8 independent attempts. Each child is a full microVM restored from
# the base snapshot, isolated from its siblings.
children = base.fork(8)

# Run one attempt per child IN PARALLEL (threads/async), then score them.
# Each child is isolated: a crash or rm -rf in one cannot touch the others.
results = run_attempts_in_parallel(children)   # your scoring
winner = pick_best(results)

# Discard the losers. You only paid for the pages each one dirtied.
for c in children:
    if c is not winner:
        c.terminate()
```

The same shape works over MCP: `sandbox_create` -> `sandbox_fork` ->
`sandbox_exec`/`sandbox_read_file` per child -> `sandbox_terminate`.

## Lineage and workspaces (Kubernetes cluster mode)

A Workspace is the durable, forkable filesystem behind sandboxes: bind a base to
one so the best-of-N winner is committed instead of discarded. Workspaces are a
Kubernetes cluster feature, driven by the `AgentRun` surface (not the direct
`mitos.create` one):

```python
import mitos

run = mitos.AgentRun(namespace="default")           # k8s mode

# Bind the base to a durable workspace by name.
base = run.create("python", workspace="refactor-task")
children = base.fork(8, timeout=180)                 # seconds to activate

results = run_attempts_in_parallel(children)
winner = pick_best(results)

# Commit the winner: terminating a workspace-bound sandbox dehydrates
# /workspace into a new committed revision. checkpoint=True also snapshots VM
# memory for a resumable head. It returns the workspace name.
winner.terminate(checkpoint=True)
for c in children:
    if c is not winner:
        c.terminate()
```

Each commit is a revision; revisions form a lineage you can inspect
(`run.workspace("refactor-task").log()`) and resume from. Ephemeral, throwaway
exploration needs no workspace; use the direct `mitos.create` loop above.

## Cost-awareness

- Copy-on-write: a fork shares the base's pages until it writes; you pay for
  what you dirty, not a full copy. A wide fan-out of mostly-reading attempts is
  cheap; attempts that rewrite large files cost more.
- Discard losers promptly (`terminate`) to release their unique pages.
- Spend caps: a sandbox can carry a capability budget (max forks, CPU seconds,
  and so on). A self-initiated fork is admitted only while the budget has room;
  over-budget requests fail with a `BudgetExhausted` remediation. A fork's
  budget is never wider than its parent's remaining, so a runaway fan-out is
  bounded by design.

## Isolation guarantees (running untrusted or model-written code)

Each sandbox is a Firecracker microVM with its own kernel, not a shared-kernel
container. Model-written or untrusted code runs inside that VM: a crash, a fork
bomb, or a destructive command is contained to the one sandbox and cannot reach
the host, the control plane, or sibling sandboxes. This is the property that
makes it safe to execute code an agent just generated. Sandboxes are not pods;
pod-scoped Kubernetes controls do not govern the workload inside the VM.

## What a sandbox may NOT do to others

A sandbox acts on ITSELF: it forks itself, execs in itself, terminates itself.
It cannot delete or mutate other sandboxes, change pools, or administer
workspaces. `exit()` terminates only the caller. Rely on this when reasoning
about blast radius.

## Surfaces

- MCP server `mitos-mcp`: `sandbox_create`, `sandbox_exec`, `sandbox_read_file`,
  `sandbox_write_file`, `sandbox_fork`, `sandbox_terminate`, `workspace_create`,
  `workspace_list`. See `docs/mcp.md`.
- Python SDK (`sdk/python`) and TypeScript SDK (`sdk/typescript`): `create`,
  `exec`, `run_code`, `fork`, `terminate`; k8s mode and the standalone
  sandbox-server (no Kubernetes) mode.
- CLI `mitos` and the standalone `sandbox-server`. See `docs/cli.md`.
- Error contract for agents: `llms.txt`.
