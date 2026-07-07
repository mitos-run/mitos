# mitos CLI

`mitos` is the command-line interface for snapshot-fork sandboxes. It drives
the sandbox lifecycle (create, exec, file IO, fork, terminate, list) against the
hosted mitos.run gateway OR a Kubernetes cluster, and brings a local kind dev
cluster up or down for a one-command local-dev loop.

```bash
go build -o mitos ./cmd/mitos/
```

## Command reference

```
mitos init [--api-key K] [--check]             set up the hosted CLI: validate
                                                  an api key, save it, print
                                                  the first-fork next step
mitos run <command> [--pool P] [--timeout N]   create a sandbox, run the
                                                  command, terminate, exit with
                                                  the command's exit code
mitos sandbox create [--pool P]                create a sandbox, print its id
  [--wait|--no-wait] [--timeout N]
mitos sandbox ls [-n namespace] [-A] [-o json] list sandboxes (table or JSON)
mitos sandbox exec <id> <command...>           run a command in a sandbox
mitos sandbox fork <id> [--count N]            fork a sandbox, print new ids
  [--wait|--no-wait] [--timeout N]
mitos sandbox terminate <id> [--timeout N]     destroy a sandbox
mitos ws create <name>                         create an empty workspace
mitos ws ls [-n namespace]                     list workspaces
mitos ws log <workspace>                       list revisions, newest first
mitos ws diff <workspace> <revision>           content-hash diff vs the parent
mitos ws fork <src-ws> <revision> <dst-ws>     branch a committed revision
mitos ws revert <workspace> <revision>         set the head to a past revision
mitos ws rm <name>                             delete a workspace and revisions
mitos ws bind <id> <workspace>                 bind a sandbox to a workspace
mitos dev up [--skip-cluster-create]           bring a local kind dev cluster
                                                  up with a mock control plane
mitos dev down                                 delete the local kind dev cluster
mitos doctor [-n namespace]                    run the node + install preflight
                                                  and print remediation
```

## Agent automation contract

`mitos` is built to be driven by an agent or a shell pipeline, not only a human
at a prompt. Three surfaces make it scriptable: a documented exit-code contract,
machine-readable `-o json` output on the read verbs, and uniform
`--wait`/`--timeout` control on the lifecycle verbs.

### Exit codes

Every invocation returns one of a small, stable set of exit codes. An automated
caller can branch on the code without parsing stderr; the human-facing
diagnostic on stderr carries the cause and remediation.

| Code | Name | Meaning |
|---|---|---|
| `0` | success | The command succeeded. For `run` this is the executed command's own exit code (0 on success). |
| `1` | error | A general, remediable runtime error (backend unreachable, a failed operation). The stderr diagnostic names the cause. |
| `2` | usage | A usage error: an unknown subcommand, a missing argument, a bad flag, or an unknown `-o` output format. |
| `3` | not found | The targeted sandbox or workspace does not exist. |
| `124` | timeout | A `--wait`/`--timeout` deadline elapsed before the operation completed. The value matches the coreutils `timeout` tool so it is familiar in pipelines. |

`run` is the one verb that passes through: its exit code is the executed
command's exit code, so `mitos run false` exits non-zero exactly as `false`
would. The other verbs use the table above.

### Structured output: `-o json`

The read and inspect verbs accept `-o json` (equivalently `--output json` or the
`--json` shorthand) and emit a stable JSON envelope on stdout. The default
remains the human-aligned table. An unrecognized format is a usage error (exit
`2`), never a silent fall-back to the human render, so an agent that asked for
JSON always gets JSON or a clear failure.

`mitos sandbox ls -o json`:

```json
{
  "sandboxes": [
    {
      "name": "sbx-abc123",
      "pool": "python",
      "phase": "Ready",
      "node": "node-a",
      "endpoint": "10.0.0.1:9091",
      "ageSeconds": 90
    }
  ]
}
```

`mitos ws ls -o json`:

```json
{
  "workspaces": [
    { "name": "w1", "head": "rev-2", "revisions": 2, "resumable": true }
  ]
}
```

`mitos ws log <workspace> -o json`:

```json
{
  "revisions": [
    { "name": "rev-2", "phase": "Committed", "resumable": true, "lineage": "root" }
  ]
}
```

An empty listing renders an empty array (`{"sandboxes": []}`), never `null`, so a
consumer can iterate unconditionally. `ageSeconds` is a whole-second integer so a
caller never has to parse the human `90s`/`2m`/`3h` rendering.

### Waiting and timeouts: `--wait` / `--timeout`

The lifecycle verbs that start an asynchronous operation accept a uniform
`--wait`/`--no-wait` and `--timeout` pair:

- `mitos sandbox create` and `mitos sandbox fork` (and the `mitos fork` alias)
  wait for the new sandboxes to become `Ready` by default. Pass `--no-wait` (or
  `--wait=false`) to return as soon as the object is created without polling for
  readiness. On the cluster backend the created sandbox name is assigned
  client-side, so `--no-wait` on `create` returns the id immediately; the hosted
  gateway returns as soon as the sandbox is provisioned, so `--no-wait` is a
  no-op there.
- `--timeout N` bounds the wait to `N` seconds. When the deadline elapses the
  command exits `124` (timeout) rather than blocking indefinitely. `--timeout 0`
  (the default) uses the backend's own bound.
- `mitos sandbox terminate` accepts `--timeout N` to bound the delete call.
  Terminate is asynchronous (the controller reaps the object); waiting until the
  sandbox is fully reaped is a named follow-up, so `--wait` is not offered on
  terminate yet.

`mitos run`'s existing `--timeout` is the in-sandbox exec deadline (the executed
command's timeout), distinct from the lifecycle wait above.

### First-run setup: `mitos init`

`mitos init` takes you from key-in-hand to a verified working setup in one
command. It validates an API key against the gateway (by listing sandboxes, the
cheapest authenticated call), saves the endpoint and key to
`~/.config/mitos/config.json` (mode `0600`, honoring `MITOS_CONFIG_DIR`), and
prints the first next step:

```bash
mitos init --api-key sk-...          # or set MITOS_API_KEY and run: mitos init
# init: key sk-abc12...wxyz verified against https://api.mitos.run
# saved to ~/.config/mitos/config.json; the CLI reads it whenever
# --api-key and MITOS_API_KEY are unset
#
# You are ready. Create a sandbox and fork it:
#
#   mitos sandbox create --pool python   # create a sandbox, prints its id
#   mitos fork <id> --count 2            # fork it into 2 live siblings
```

Run without a key on an interactive terminal, `mitos init` says where to get
one (https://mitos.run/keys, or your deployment's console for a custom
`--server`) and prompts you to paste it; the paste is read without echo. On a
non-interactive stdin it prints the same guidance and exits with a usage error.

`mitos init --check` re-validates the SAVED config and reports clearly; it is
the hosted counterpart of the `mitos doctor` cluster preflight. It exits `0`
when the saved key is valid, `1` when the gateway rejects it (with remediation:
mint a new key and re-run `mitos init`), and `2` when no config is saved yet.

The key value is never logged and never echoed back in full; every message
shows at most a mask (prefix + last 4). After `mitos init`, every hosted verb
resolves credentials with the precedence: `--api-key`/`--server` flags, then
`MITOS_API_KEY`/`MITOS_BASE_URL`, then the config file. Cluster mode is
selected only when none of the three provide an API key, so remove the config
file (or keep using a kubeconfig-scoped shell) if you switch between modes.

### Authentication: `mitos auth login`

`mitos auth login --token <session>` writes a credential profile to
`~/.config/mitos/credentials.json` (mode `0600`, honoring `MITOS_CONFIG_DIR`).
The profile holds the session token, the resolved email, and the default org.

One login authenticates every agent-facing surface. The Python and TypeScript
SDKs and `mitos-mcp` resolve the bearer token with this precedence:

1. an explicit argument (the SDK `api_key`, the `mitos-mcp --token` flag);
2. the `MITOS_API_KEY` environment variable;
3. the `token` field of `~/.config/mitos/credentials.json` (honoring
   `MITOS_CONFIG_DIR`);
4. none, in which case the surface runs tokenless (the standalone
   `sandbox-server` accepts that).

The credential file's token is sent verbatim as the `Authorization: Bearer`
value; the hosted gateway decides whether it is valid. If your deployment
requires a scoped API key minted with `mitos auth keys create` rather than the
raw login session, set that key as `MITOS_API_KEY` or pass it explicitly; it
overrides the file. The token VALUE is never logged or echoed in an error.

### Preflight: `mitos doctor`

`mitos doctor` runs an install/node preflight and prints a report
with an actionable, LLM-legible remediation per failing check. It is meant to run
on a KVM worker node, or as an in-cluster Job, and exits non-zero if any check
fails so it composes in an install pipeline.

Checks:

- `kvm-device`: `/dev/kvm` is present and a usable character device.
- `kernel-module-nf_tables` / `-vhost_vsock` / `-tun`: the required kernel
  modules are loaded.
- `guest-kernel`: the guest kernel image is staged where forkd boots from
  (default `/var/lib/mitos/vmlinux`).
- `pki-secrets`: the controller minted `mitos-ca`, `mitos-forkd-tls`, and
  `mitos-controller-tls`.
- `image-pull-secret`: a pull secret is present (a WARN, not a fail: public
  images still pull, only private registries need it).
- `psa-privileged`: the install/pool namespace carries
  `pod-security.kubernetes.io/enforce=privileged`.

The cluster checks (PKI, pull secret, PSA) read object PRESENCE only, never a
Secret's contents, so a report can never leak a secret value. The node checks
are meaningful on a Linux KVM node; on a workstation they honestly report
"absent". Without a reachable kubeconfig the cluster checks are skipped and the
node checks still run. See `docs/platforms/host-prerequisites.md` for the
host/kernel checklist `doctor` enforces.

### Workspace verbs (git-shaped)

A `Workspace` is durable, forkable agent state independent of any single
sandbox. Its revisions form a DAG, and the verbs are git-shaped:

- `mitos ws create <name>` creates an empty `Workspace`.
- `mitos ws ls` lists workspaces with their head revision, revision count, and
  whether the head is resumable (paired with a memory snapshot).
- `mitos ws log <workspace>` lists the workspace's revisions newest first, each
  with its phase, resumable flag, and lineage (`fromClaim:<n>`,
  `fromWorkspaceRevision:<n>`, or `root`).
- `mitos ws diff <workspace> <revision>` prints the path-level content-hash diff
  recorded for a revision (captured when a sandbox terminates with a
  `{diff: true}` output).
- `mitos ws fork <src-ws> <revision> <dst-ws>` branches a committed revision
  into an existing destination workspace. A fork is a content-addressed branch:
  the new revision shares the parent's content manifest, so no bytes are copied.
  Forking an uncommitted revision is refused with an LLM-legible error.
- `mitos ws revert <workspace> <revision>` sets a workspace head back to a past
  revision by creating a new tip that shares that revision's content. Revisions
  are immutable, so a revert is a new tip, never a history rewrite.
- `mitos ws rm <name>` deletes a workspace; its revisions are garbage-collected
  by owner reference.
- `mitos ws bind <sandbox-id> <workspace>` binds a running sandbox to a
  workspace. A sandbox binds one workspace for its lifetime; re-binding to a
  different workspace is refused.

Global flags `--namespace`/`-n` and `--pool` may appear before the subcommand.
`mitos run` exits with the executed command's exit code so it chains in shell
pipelines.

## Backends

### Hosted backend (MITOS_API_KEY)

Set `MITOS_API_KEY` to route all sandbox verbs to the hosted mitos.run gateway.
No kubeconfig, no Kubernetes, no nodes to manage.

```bash
export MITOS_API_KEY=sk-...   # from https://mitos.run/keys
export MITOS_BASE_URL=https://api.mitos.run   # the default; omit it entirely

# Quickstart: create a sandbox, exec, fork, list, terminate.
mitos sandbox create --pool python
# prints: sbx-abc123

mitos sandbox exec sbx-abc123 "echo hello"
# prints: hello

mitos fork sbx-abc123 --count 3
# prints three new sandbox ids

mitos sandbox ls
# lists all live sandboxes under your api key

mitos sandbox terminate sbx-abc123
```

Alternatively, pass flags inline instead of using environment variables:

```bash
mitos --api-key sk-... --server https://api.mitos.run sandbox create --pool python
```

Flag precedence (highest wins): `--api-key` flag, then `MITOS_API_KEY` env var,
then the `api_key` saved by `mitos init` in `~/.config/mitos/config.json`.
URL precedence: `--server` flag, then `MITOS_BASE_URL` env var, then the saved
`endpoint`, then `https://api.mitos.run`.

The api key value is NEVER logged or placed in any error message.

**What works in hosted mode:**

| Verb | Hosted | Notes |
|---|---|---|
| `init [--check]` | yes | validate + save the key, verify the saved config |
| `sandbox create --pool <template>` | yes | `--pool` names a template, not a pool |
| `sandbox exec <id> <cmd>` | yes | Connect RPC via gateway |
| `sandbox ls` | yes | GET /v1/sandboxes |
| `sandbox fork <id> --count N` | yes | re-forks from the sandbox template |
| `sandbox terminate <id>` | yes | DELETE /v1/sandboxes/{id} |
| `run <cmd> --pool <template>` | yes | create + exec + terminate |
| `ws *` (workspace verbs) | no | cluster-only, requires Kubernetes CRDs |
| `template build/push` | no | cluster-only, requires a KVM node |
| `dev up/down` | no | local kind cluster; no api key needed |
| `doctor` | no | cluster node preflight |
| `auth login/keys` | yes | always talks to the hosted account service |

Fork semantics in hosted mode: `mitos fork <id> --count N` calls `POST /v1/fork`
N times with `{"template": <id>}`. The gateway resolves the sandbox to its
original template and forks from that snapshot. The source sandbox keeps running
and each child is an independent sibling forked from the same template snapshot.
This matches `mcp.HTTPBackend.Fork()`. Note the hosted API also serves a TRUE
live fork at `POST /v1/sandboxes/<id>/fork` (the child inherits the running
source's current memory and disk; see `docs/saas/accounts-gateway.md`), which
the Python SDK `DirectSandbox.fork()` drives; moving the CLI onto that route is
a follow-up.

### Cluster backend (kubeconfig)

For every `run` and `sandbox` verb, `mitos` resolves a Kubernetes connection
from the standard kubeconfig (`KUBECONFIG`, `--kubeconfig`, or in-cluster). It
then:

- `sandbox create` creates a `Sandbox` with `spec.source.poolRef` referencing the
  pool and waits for it to reach the `Ready` phase, then prints the sandbox name
  as the sandbox id.
- `sandbox exec` / file IO reads the per-sandbox bearer token from the sandbox's
  Secret at request time and calls the sandbox's HTTP API. The token value
  is held in memory only for the request and is never logged; it is redacted from
  any error string.
- `sandbox fork` creates a `Sandbox` with `spec.source.fromSandbox` and waits for
  the requested number of forks to be `Ready`.
- `sandbox ls` lists `Sandbox`es (a namespace with `-n`, all namespaces with
  `-A`, or the backend default otherwise).
- `sandbox terminate` deletes the `Sandbox`, which the controller reaps.

This is the same `Sandbox` path the controller and forkd implement; the CLI
is a thin client over the CRDs plus the token-scoped HTTP exec.

### Dev mock-mode local cluster

`mitos dev up` brings up a local kind cluster running a MOCK control plane so
the full claim path completes without KVM:

1. `kind create cluster` (tolerating an already-existing cluster; skipped with
   `--skip-cluster-create` to target a cluster you already stood up).
2. `kubectl apply -f deploy/crds/` (the CRDs).
3. `kubectl apply -k deploy/dev/` (the dev overlay).

The dev overlay (`deploy/dev/`) runs:

- the **controller** with `--mock --disable-pki-bootstrap`, so it dials forkd over
  insecure gRPC (no control plane CA or TLS Secrets).
- a **forkd** DaemonSet with `--mock` and no TLS flags, using the no-KVM mock fork
  engine. It mounts no `/dev/kvm` and carries no `mitos.run/kvm` nodeSelector,
  so it schedules on the plain kind node.
- a default `SandboxPool` named `dev-default` with `spec.template` inline in the
  `default` namespace.

The controller discovers the mock forkd by its `app.kubernetes.io/component:
forkd` pod label, builds the `dev-default` pool snapshot over insecure gRPC, and a
claim forks via the mock engine and reaches `Ready`:

```bash
mitos dev up
mitos sandbox create --pool dev-default   # prints the sandbox id, Ready
mitos sandbox ls
mitos sandbox terminate <id>
mitos dev down
```

The dev manifests reference the `mitos-controller:ci` and `mitos-forkd:ci`
image tags with `imagePullPolicy: IfNotPresent`. Build and load them before
`mitos dev up` (CI does this automatically):

```bash
docker build -f Dockerfile.controller -t mitos-controller:ci .
docker build -f Dockerfile.forkd -t mitos-forkd:ci .
kind load docker-image mitos-controller:ci --name mitos-dev
kind load docker-image mitos-forkd:ci --name mitos-dev
```

## Mock-engine limitation

The dev cluster uses the mock fork engine, which has NO guest VM. A claim
reconciles to `Ready` and the control-plane dispatch works, but a real in-VM
`exec` is not exercised on the dev cluster. To run real sandboxes locally you need
a node with `/dev/kvm` and the production manifests (`deploy/controller/` +
`deploy/daemon/`) with the `mitos.run/kvm=true` node label.

## What is proven

PROVEN in CI:

- command dispatch for `run` and every `sandbox` verb;
- the cluster `Sandbox` claim path with token-scoped exec;
- `mitos dev up` orchestration (CRDs + mock controller + mock forkd + pool);
- `sandbox ls` over the control plane;
- on the dev mock cluster on kind: `sandbox create` reaches `Ready`, `sandbox ls`
  shows it, and `sandbox terminate` removes it.

The mock-engine exec limitation above is the one gap: real in-VM `exec` is proven
by the KVM CI of the API, not by the kind dev smoke.

## kubectl-mitos operator plugin

`kubectl-mitos` is a separate kubectl plugin for the OPERATOR persona: a
cluster admin who inspects and operates the sandbox objects already in the
cluster. Installed as `kubectl-mitos` on `PATH`, it is invoked as
`kubectl mitos <verb>` and reads the cluster connection from the standard
kubeconfig resolution.

```bash
go build -o /usr/local/bin/kubectl-mitos ./cmd/kubectl-mitos/
```

```
kubectl mitos ls   [-n ns] [-A]            list Sandboxes
kubectl mitos ps   [name] [-n ns] [-A]     list fork Sandboxes (or one sandbox's forks)
kubectl mitos tree [--pool P] [-n ns] [-A] render the fork/lineage DAG
kubectl mitos top  [-n ns] [-A]            per-sandbox CoW-aware metering
kubectl mitos logs <sandbox> [-n ns]       husk stub pod console for a claim
kubectl mitos exec <sandbox> [-n ns] -- cmd run a command in a sandbox
```

### tree

`tree` walks the lineage DAG: each `Sandbox` with `spec.source.poolRef` is a root,
and a `Sandbox` with `spec.source.fromSandbox` nests under whatever sandbox its
`spec.source.fromSandbox` names (a pool-ref sandbox OR another fork sandbox,
so a multi-level fork chain nests). Siblings sort by name; an orphan fork sandbox
whose source is out of scope is surfaced as its own root rather than dropped.
`--pool <name>` scopes to one pool via a transitive walk over the source refs.

### top

`top` shows per-sandbox CoW-aware metering pulled from each node's forkd
`GET /v1/metering` endpoint (operational data on the same access class as
`/metrics` and `/healthz`). The columns are HONEST about what they mean:

- `UNIQUE-MEM` is the marginal unique (private-dirty) memory a fork actually
  adds. It is NOT `memory.current`.
- `SHARED-MEM` is the shared-once template attribution: the page set every fork
  of a template maps copy-on-write, counted once per template at the node level
  (see `internal/metering`).
- `UNIQUE-DISK` is the backing storage the sandbox alone owns.

A sandbox with no metering datum (no endpoint, an unreachable forkd, or no
matching row) shows a dash in every metered cell, never a zero and never a
fabricated value.

### logs

`logs <sandbox>` prints the husk stub pod console for the claim (the
`mitos.run/husk` pod labeled `mitos.run/claim=<claim>`) via the Kubernetes
pod-logs API, then a one-line guest-console note. On a mock or no-VMM control
plane (kind) there is no husk pod or no live guest, so the stub console is
reported absent and the guest console states it needs a running sandbox: the
guest serial/vsock console streams only from a live VMM, not from this
read-only operator path.

### exec

`exec <sandbox> -- <cmd>` runs a command in the sandbox over the forkd HTTP
sandbox API, authenticating with the per-sandbox bearer token read from the
claim's `<claim>-sandbox-token` Secret: the SAME gate the SDK uses, never
bypassing auth. The token value is held only for the request, never logged, and
redacted from any error string. A claim that is not `Ready` (or has no endpoint,
or no token Secret) yields a clear, actionable error rather than a hang. The
in-sandbox command's exit code becomes the plugin's exit code so it chains in
shell pipelines.

On kind the mock engine has no guest VM, so `exec`/`top`/`logs` of a REAL running
sandbox are the KVM/bare-metal tail; the kind-e2e smoke proves `ls`/`ps`/`tree`
at the object level. `cp` and `port-forward` for operators are not yet available.

## Follow-ups

- workspace verbs (`mitos ws log|diff|revert|branch`) pending Workspace;
- `mitos pool create|refresh` beyond what `dev up` needs;
- streaming exec / PTY (`exec_stream`) pending the Connect protocol;
- a `curl | sh` installer and `get.mitos.run` distribution;
- `mitos init` browser/device-flow login (no pasted key) and optional MCP
  server / editor configuration;
- shell completions and a code-interpreter-compatible API shim;
- the agent-automation verbs deferred from the first `-o json` increment:
  `mitos cp` (host <-> sandbox file copy), `mitos logs` (stream a sandbox or
  claim console), a raw `mitos api` passthrough, and reading secrets from stdin
  so a key never lands in shell history or `argv`.
