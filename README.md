<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/mitos-mark-white.svg">
    <source media="(prefers-color-scheme: light)" srcset="docs/assets/mitos-mark-black.svg">
    <img alt="Mitos" src="docs/assets/mitos-mark-black.svg" width="120" height="120">
  </picture>
</p>

<h1 align="center">Mitos</h1>

<p align="center">
  <b>Isolated, forkable computers for your AI agents.</b><br/>
  Millisecond microVM sandbox forking on Kubernetes: fork a running VM into parallel attempts and restore from memory in tens of milliseconds.
</p>

<p align="center">
  <a href="https://github.com/mitos-run/mitos/actions/workflows/ci.yaml"><img alt="CI" src="https://img.shields.io/github/actions/workflow/status/mitos-run/mitos/ci.yaml?branch=main&label=CI"></a>
  <a href="https://github.com/mitos-run/mitos/releases"><img alt="Release" src="https://img.shields.io/github/v/release/mitos-run/mitos?include_prereleases&label=release"></a>
  <a href="LICENSE"><img alt="License" src="https://img.shields.io/github/license/mitos-run/mitos?label=license"></a>
  <a href="https://github.com/mitos-run/mitos"><img alt="Go" src="https://img.shields.io/github/go-mod/go-version/mitos-run/mitos?label=go"></a>
  <a href="https://goreportcard.com/report/mitos.run/mitos"><img alt="Go Report Card" src="https://goreportcard.com/badge/mitos.run/mitos"></a>
  <a href="docs/"><img alt="Docs" src="https://img.shields.io/badge/docs-mitos-blue"></a>
  <a href="https://discord.gg/zddgd2pgab"><img alt="Discord" src="https://img.shields.io/discord/1518722949295771759?label=discord&logo=discord&logoColor=white&color=5865F2"></a>
</p>

<p align="center">
  <a href="#quickstart">Quickstart</a> .
  <a href="docs/">Documentation</a> .
  <a href="#features">Features</a> .
  <a href="#architecture">Architecture</a> .
  <a href="#comparison">Comparison</a> .
  <a href="CONTRIBUTING.md">Contributing</a> .
  <a href="https://discord.gg/zddgd2pgab">Community</a>
</p>

<p align="center">
  <img alt="Mitos SDK: create a microVM sandbox, run code, and fork it into isolated parallel attempts" src="docs/assets/demo.gif" width="760">
</p>

---

## Try it in a few lines

```python
import mitos

sb = mitos.create("python")              # Ready microVM sandbox (~27 ms warm-claim)
print(sb.exec("echo hello").stdout)      # hello

# Fork into independent siblings to try two approaches at once.
a, b = sb.fork(2)
a.exec("echo conservative > /workspace/plan.txt")
b.exec("echo aggressive  > /workspace/plan.txt")

sb.terminate()
```

```bash
pip install mitos-run
export MITOS_API_KEY=sk-...   # a key from https://mitos.run; no Kubernetes required
```

The base URL defaults to the hosted endpoint, so the same code runs against your own cluster by setting `MITOS_BASE_URL`.

## Why Mitos

Agent harnesses need fast, isolated environments where agents read and write files, install packages, and run untrusted code. Every existing option forces a trade: speed without ownership, isolation without forking, Kubernetes-native without warm starts, or durability locked inside someone else's cloud.

- **Live-fork a running VM.** N-way copy-on-write fork of a live microVM: daughters share the parent's memory pages until they write, so each fork lands in a warm, ready environment. Branch one agent into many parallel attempts.
- **~27 ms warm-claim activate.** Firecracker microVMs restore from a memory snapshot in the tens-of-milliseconds class: P50 ~27 ms on the bare-metal reference node, reproducible from [`bench/husk-activate-latency.sh`](bench/husk-activate-latency.sh).
- **Open source, self-hostable, Kubernetes-native.** As far as we know, the only runtime that does all three. You drive the whole lifecycle through declarative CRDs (`mitos.run`).

Two ways to run it:

- **Self-hosted (today):** any Kubernetes cluster with KVM nodes. Your data never leaves your infrastructure. Bare metal (Talos + Hetzner) is the first-class reference platform.
- **Hosted (in progress):** the same engine and API operated by us, for teams that want milliseconds without managing nodes.

> Two engine paths exist. The **husk pod-native path is the default**: each VM runs in its own unprivileged pod, and the source husk pod snapshots its running VM so N child pods restore it via CoW. The **raw-forkd path** runs forks in forkd's in-process engine. Everything below runs on the husk default unless explicitly marked `engine path`.

## Quickstart

### Python

One line gives you a Ready sandbox. The SDK resolves the API key (argument, else `MITOS_API_KEY`) and base URL (argument, else `MITOS_BASE_URL`, else the hosted `https://mitos.run`); the key is never logged. Full reference: [docs/quickstart.md](docs/quickstart.md).

```python
import mitos

sb = mitos.create("python")                      # Ready sandbox handle

# Files, stateful code, and fork all work on the flat handle.
sb.files.write("/workspace/plan.txt", "draft")
print(sb.files.read("/workspace/plan.txt"))      # draft

ex = sb.run_code("import math; math.sqrt(144)")
print(ex.text)                                   # 12.0

# Fork into independent siblings to try two approaches at once.
fork_a, fork_b = sb.fork(2)
fork_a.exec("echo conservative > /workspace/a.txt")
fork_b.exec("echo aggressive  > /workspace/b.txt")

sb.terminate()
```

The async client mirrors the same surface: `await mitos.aio.create("python")` returns an `AsyncDirectSandbox` with the same `exec` / `run_code` / `files` / `create_pty` / `fork` / `terminate` over `httpx.AsyncClient`.

### On a cluster (operators)

Run the operator yourself and the two-tier `AgentRun` path drives the CRDs directly:

```python
from mitos import AgentRun

c = AgentRun()                                   # kubeconfig or in-cluster; autodetected
sb = c.sandbox("python", ready=True)             # claims a warm sandbox, waits Ready
print(sb.exec("python -c 'print(40 + 2)'").stdout)   # 42

fork_a, fork_b = sb.fork(2)                       # fork against shared warmed state
sb.terminate()
```

`c.sandbox("python")` lazily creates a default pool if you have none; pass `pool="my-pool"` to use an existing one. Errors raise `AgentRunError(code, cause, remediation)`. `AsyncAgentRun` mirrors the hot paths and adds `create_pty()` over WebSocket.

### Languages and modes

Every SDK speaks the same sandbox-server REST API in **direct mode** (standalone or hosted), and every SDK now also has **cluster mode** (an `AgentRun` that drives the `mitos.run/v1` CRDs through the Kubernetes API). The default-pool naming is byte-for-byte identical across all six.

| Language | Install | Direct mode | Cluster mode | SDK docs |
|---|---|---|---|---|
| Python | `pip install mitos-run` | yes (sync + async) | yes (`AgentRun`) | [sdk/python](sdk/python) |
| TypeScript | `npm i @mitos/sdk` | yes | yes (`AgentRun`) | [sdk/typescript](sdk/typescript/README.md) |
| Go | `go get github.com/mitos-run/mitos/sdk/go` | yes (typed, `errors.Is`-friendly) | yes (`AgentRun`) | [sdk/go](sdk/go/README.md) |
| Ruby | gem (stdlib only) | yes | yes (`AgentRun`) | [sdk/ruby](sdk/ruby/README.md) |
| Rust | crate (blocking) | yes | yes (`AgentRun`) | [sdk/rust](sdk/rust/README.md) |
| Java | JDK 17 (stdlib only) | yes | yes (`AgentRun`) | [sdk/java](sdk/java/README.md) |

The Go SDK ships in its own nested module (`github.com/mitos-run/mitos/sdk/go`), so importing it never pulls the controller into your build.

### CLI

```bash
go install mitos.run/mitos/cmd/mitos@latest      # works today (needs a Go toolchain)

mitos sandbox create --pool dev-default
mitos run echo hello --pool dev-default
mitos sandbox ls
```

`mitos dev up` brings up a one-command local control plane on a mock engine. An MCP server (`mitos-mcp`) exposes sandboxes as MCP tools for any MCP-speaking agent, and an [Agent Skill](skills/mitos/SKILL.md) teaches skill-aware agents the workflow (fork vs. fresh, best-of-N, isolation, cost). The full install matrix (script, Homebrew, deb/rpm, scoop/winget, checksums) is in [docs/install.md](docs/install.md); packaging beyond `go install` lands with releases.

### Beyond exec

```python
# Streaming exec: callbacks fire per chunk; the ExecResult still carries the aggregate.
sb.exec("pip install rich", on_stdout=lambda b: print(b.decode(), end=""))

# Stateful code interpreter: state persists across run_code calls for the sandbox lifetime.
ex = sb.run_code("import pandas as pd; pd.DataFrame({'x':[1,2,3]}).describe()")
print(ex.text)            # the REPL's last value, rendered

# Detach a long-running process and keep working.
sb.exec_background("python train.py > /workspace/train.log 2>&1")
```

Blocking `exec` works on the husk default. Streaming exec (`/v1/exec/stream`) and the interactive PTY (`/v1/pty`) run on the engine path and are being brought to the husk default. `run_code` returns a fail-closed `KernelUnavailable` until the kernel ships in the husk base image.

### Integrations

Drop a Mitos sandbox into the coding agent or agent framework you already use. Each adapter is a thin shim over the same native ops (`exec`, `run_code`, `files`, `fork`), with no hard dependency on the framework package. The [integrations hub](docs/integrations/README.md) indexes every path.

| Surface | How | Doc |
|---|---|---|
| Claude Code | MCP server + agent skill | [docs/integrations/claude-code.md](docs/integrations/claude-code.md) |
| opencode | MCP server or harness-in-sandbox | [docs/integrations/opencode.md](docs/integrations/opencode.md) |
| OpenAI Agents SDK | `from mitos.integrations.openai_agents import MitosSandboxTools` | [docs/integrations/openai-agents.md](docs/integrations/openai-agents.md) |
| LangChain / deepagents | `from mitos.integrations.langchain import MitosSandbox` | [sdk/python/README.md](sdk/python/README.md) |
| Claude Agent SDK | `from mitos.integrations.claude_agent import MitosSandboxTools` | [sdk/python/README.md](sdk/python/README.md) |
| Vercel AI SDK / Pydantic AI / AutoGen / LlamaIndex | standard MCP server | [docs/integrations/mcp-frameworks.md](docs/integrations/mcp-frameworks.md) |
| VibeKit / ZenML | `from mitos.integrations.vibekit import MitosVibeKitProvider` | [sdk/python/README.md](sdk/python/README.md) |
| E2B (migration) | `from mitos.e2b import Sandbox` | [docs/migrating-from-e2b.md](docs/migrating-from-e2b.md) |

The Codex CLI is closed and its sandbox is not swappable; the supported path into the OpenAI ecosystem is the OpenAI Agents SDK above. The E2B shim is a "change one import" bridge for self-hosted, regulated, or air-gapped teams leaving E2B's cloud: it presents E2B's `Sandbox` surface over the standalone sandbox-server. `get_host(port)` returns a signed, expiring preview URL once the per-sandbox preview proxy is deployed.

### On a cluster

```bash
kubectl apply -k deploy/
```

The self-contained kustomize base installs the CRDs, the controller (husk mode), the forkd DaemonSet, the `/dev/kvm` device plugin, and the PKI bootstrap, and applies on a real KVM node with no manual patches. Nodes need `/dev/kvm` and the label `mitos.run/kvm=true`. The Helm chart is published at `https://mitos.run/charts` and listed on Artifact Hub: `helm repo add mitos https://mitos.run/charts`. See [deploy/charts/mitos](deploy/charts/mitos/README.md) for the install command and values.

```yaml
apiVersion: mitos.run/v1
kind: SandboxPool
metadata:
  name: python-agent-pool
spec:
  template:
    image: python:3.12-slim
    init: ["pip install numpy pandas requests"]
    resources: { cpu: "1", memory: "512Mi" }
    volumes:
      - { name: workspace, size: 5Gi, forkPolicy: Snapshot }
  warm: { min: 10 }
---
apiVersion: mitos.run/v1
kind: Sandbox
metadata:
  name: parallel-attempt
spec:
  source:
    fromSandbox: { name: agent-session-1 }
  replicas: 3
  secretInheritance: inherit   # forks duplicate memory; opt in knowingly
```

## Features

The husk pod-native path is the default. A few capabilities run today only on the raw-forkd `engine path` and are marked, with a link to the tracking issue.

### Speed

| Capability | What you get | Docs |
|---|---|---|
| Warm-claim activate | P50 ~27 ms on the bare-metal reference node (snapshot load + fork-correctness handshake + guest-ready); ~6-16 ms snapshot restore; ~3 MiB marginal memory per fork via CoW page sharing | [BENCHMARKS.md](BENCHMARKS.md) |
| Pre-snapshotted pools | OCI images flattened to ext4 rootfs and warmed with your `init` steps before snapshotting, so there is no cold start on claim | [docs/templates.md](docs/templates.md) |
| CoW memory sharing | You pay for unique pages across forks, not for copies | [docs/metering.md](docs/metering.md) |
| Content-addressed distribution | Forks pull only the missing sha256 chunks from a holder over mTLS; rebuilds ship deltas under a version-compatibility contract | [docs/snapshot-distribution.md](docs/snapshot-distribution.md) |

### Isolation

| Capability | What you get | Docs |
|---|---|---|
| Hardware isolation per session | A dedicated kernel per sandbox (KVM/Firecracker); on the husk default each VM runs in its own unprivileged, PSA-restricted pod, which is the per-VM boundary | [docs/threat-model.md](docs/threat-model.md) |
| No silent secret inheritance | Live forks of secret-holding sandboxes are rejected unless explicitly opted in; credentials are injected at claim time over vsock, never baked into snapshots | [docs/threat-model.md](docs/threat-model.md) |
| Default-deny egress | An in-pod nftables default-deny filter in the pod's own netns (CNI-independent), with an unconditional cloud-metadata (169.254.169.254) block and a per-template allowlist by IP:port and by name through an in-pod DNS proxy. Verified end to end on a real KVM cluster; the guest cannot influence enforcement | [docs/networking.md](docs/networking.md) |
| Encryption at rest | Per-scope LUKS2 containers with crypto-shredding and KMS envelope wrapping (behind `--enable-encryption`, fail-closed); HSM-backed keys and per-workspace scope are follow-ups | [docs/encryption.md](docs/encryption.md) |

### Agent DX

| Capability | What you get | Docs |
|---|---|---|
| Blocking exec | Correct stdout and exit code over the sandbox API | [docs/cli.md](docs/cli.md) |
| Streaming exec and PTY | Incremental stdout/stderr, background processes, and a token-gated interactive WebSocket terminal (`engine path`) | [docs/cli.md](docs/cli.md) |
| Code interpreter | `run_code` with a stateful kernel and rich multi-MIME results, in both SDKs and the MCP server; fail-closed `KernelUnavailable` until the kernel ships in the husk base image | [docs/mcp.md](docs/mcp.md) |
| LLM-legible errors | Every failure carries `{code, cause, remediation}`, parsed by the SDKs into a structured `AgentRunError` | [docs/api/errors.md](docs/api/errors.md) |

### Kubernetes-native

| Capability | What you get | Docs |
|---|---|---|
| Declarative CRDs | `SandboxPool`, `Sandbox` (poolRef/fromSandbox/fromRevision source), `Workspace`/`WorkspaceRevision` in `mitos.run/v1` with volume topology and fork behavior | [docs/templates.md](docs/templates.md) |
| Pod-native execution | Each per-sandbox VM runs in an unprivileged pod (`/dev/kvm` from a device plugin, not `privileged`), so CPU/memory requests are scheduler truth and PSA governs the pod | [docs/threat-model.md](docs/threat-model.md) |
| Capacity-aware scheduling | CoW bin-packing onto warm holders, a CoW-aware overcommit budget, a `MaxSandboxes` host-DoS ceiling with atomic slot reservation, and typed `NoCapacity` backpressure instead of OOMing a node | [docs/scheduling.md](docs/scheduling.md) |
| Demand-driven autoscaling | `SandboxPool.spec.autoscale` scales the dormant husk-pod count to `clamp(inUse + targetSpare, minWarm, maxWarm)` with an anti-thrash cooldown; a fixed pool is just `minWarm == replicas` | [docs/scheduling.md](docs/scheduling.md) |
| Failure and GC semantics | Claim TTLs, orphan-VM sweeps, controller-restart reconciliation, forkd crash reaping via an on-disk journal, node-loss handling, and saturation backpressure, all CI-proven | [docs/failure-gc.md](docs/failure-gc.md) |

### Durable state

| Capability | What you get | Docs |
|---|---|---|
| Durable forkable workspaces | `Workspace`/`WorkspaceRevision` CRDs: durable, versioned, forkable agent state independent of any sandbox. `/workspace` hydrates on start and a committed revision dehydrates on terminate over the content-addressed store. Verified create -> commit -> fork on a real KVM cluster | [docs/workspaces.md](docs/workspaces.md) |
| Outputs and diff | `spec.lifetime.onTerminate.outputs` narrows the dehydrate to listed subtrees; `{diff: true}` records a content-hash diff against the parent head | [docs/workspaces.md](docs/workspaces.md) |
| Git rendezvous | A `{git}` output pushes per-attempt branches to a rendezvous remote (the engine pushes; a human or CI merges). Best-effort on husk today | [docs/workspaces.md](docs/workspaces.md) |
| Dev-environment URL | `mitos workspace serve <ws> --pool P` warm-claims a forked sandbox bound to the workspace and returns a ready `https://<label>.<expose-domain>/` URL; each forked session gets its own URL | [docs/recipes/dev-environment.md](docs/recipes/dev-environment.md) |

### Operable

| Capability | What you get | Docs |
|---|---|---|
| Metrics and tracing | Node and controller Prometheus metrics, a per-claim OpenTelemetry trace (`--otlp-endpoint`), and a toggleable structured audit log (`--audit-log`) recording command/path and byte counts, never content or secrets | [docs/observability.md](docs/observability.md) |
| CoW-aware metering | The shared template page set is counted once, not once per fork, so billing and scheduling reflect the honest physical footprint | [docs/metering.md](docs/metering.md) |
| Operator tooling | `kubectl mitos` plugin (`ls` / `ps`) and the operational `GET /v1/metering` report | [docs/observability.md](docs/observability.md) |
| Bare metal first-class | Talos + Hetzner is the reference platform | [docs/platforms/talos-hetzner.md](docs/platforms/talos-hetzner.md) |

## Architecture

```mermaid
flowchart TB
  subgraph SDKs["SDKs and surfaces"]
    PY["Python SDK"]
    TS["TypeScript SDK / @mitos/sdk"]
    OTH["Go / Ruby / Rust / Java (direct)"]
    CLI["mitos CLI / mitos-mcp"]
  end

  subgraph CP["Kubernetes control plane"]
    CRD["SandboxPool -> Sandbox / Workspace (mitos.run/v1)"]
    CTRL["controller (Deployment): reconciles CRDs, picks nodes, calls forkd over gRPC"]
    CRD --> CTRL
  end

  subgraph NODE["KVM-capable node"]
    FORKD["forkd (DaemonSet): builds snapshots, forks via CoW restore, bridges exec/files to the guest over vsock"]
    subgraph PODS["husk pods (DEFAULT): one unprivileged pod per VM"]
      VM1["VM + guest agent (PID 1)"]
      VM2["VM + guest agent (PID 1)"]
      VM3["VM + guest agent (PID 1)"]
    end
    FORKD --> PODS
  end

  SDKs -->|HTTP /v1| FORKD
  CTRL -->|gRPC| FORKD
```

- **Claim path:** the controller selects a node and calls forkd `Fork` over gRPC; the claim status endpoint is forkd's HTTP API on that node.
- **Exec path:** SDK -> forkd HTTP API -> vsock -> guest agent (PID 1 inside the VM).

Sandboxes are not pods. Pod-scoped Kubernetes mechanisms (NetworkPolicy, ResourceQuota, PSA) govern the husk pod, not the workload inside the microVM; where we provide an equivalent, it is documented as ours. The sandbox is the VM, not the husk pod.

## Local development (no KVM required)

One command brings up a local kind cluster on a mock control plane, then the `mitos` CLI drives the full claim path:

```bash
go build -o mitos ./cmd/mitos/
docker build -f Dockerfile.controller -t mitos-controller:ci .
docker build -f Dockerfile.forkd -t mitos-forkd:ci .
kind create cluster --name mitos-dev --config hack/kind-config.yaml
kind load docker-image mitos-controller:ci --name mitos-dev
kind load docker-image mitos-forkd:ci --name mitos-dev

./mitos dev up --skip-cluster-create
./mitos sandbox create --pool dev-default   # reaches Ready on the mock engine
./mitos run echo hello --pool dev-default
./mitos dev down
```

The mock engine reconciles claims to `Ready` and exercises control-plane dispatch, but a real in-VM `exec` needs a node with `/dev/kvm`. For the no-cluster REST loop, run `go run ./cmd/sandbox-server --mock --addr :8080` and point the Python SDK at it. See [docs/cli.md](docs/cli.md).

## Comparison

A head-to-head numbers table belongs here only when our harness can regenerate it against the actual competitors on the same hardware, with scripts in this repo. That harness is [#15](https://github.com/mitos-run/mitos/issues/15). The figures below are **other vendors' published numbers, for different operations, on different hardware, with different methodology**: they are not measured by us and are not a head-to-head claim.

| Runtime | Published figure (theirs, not ours) | Operation they describe |
|---|---|---|
| Mitos (ours, measured) | ~27 ms P50 | warm-claim activate on the bare-metal reference node |
| E2B | ~150 ms | sandbox create |
| Daytona | sub-90 ms | create from snapshot |
| Modal | sub-second | sandbox create |
| CodeSandbox SDK | ~863 ms / ~495 ms | live fork / memory-resume |
| Fly Machines | < 1 s | machine start |

What is comparable and real today is the qualitative pareto map: the combination of open source, self-hostable, k8s-native, and live snapshot fork is the axis where Mitos is alone.

| | Mitos | E2B | Modal | Daytona | Morph | Cloudflare | Box | Agent Sandbox | Kata/KubeVirt | raw Firecracker |
|---|---|---|---|---|---|---|---|---|---|---|
| Hardware isolation per session | KVM microVM | microVM | gVisor | container/VM | microVM | V8 isolate | VM | Kata option | KVM | KVM |
| Snapshot fork of running state | yes, core primitive | snapshot/resume | memory snapshots | no | yes (Infinibranch) | no | disk fork | no | no | DIY |
| Warm-pool millisecond claims | yes (design center) | warm pools | warm pools | workspaces | yes | instant isolates | not published | 1-3s cold | seconds | DIY |
| Durable forkable workspaces | Workspace CRD | no | volumes | workspaces | yes, proprietary | yes (disk) | no | PVCs | PVCs | no |
| Kubernetes-native API | CRDs | SaaS API | SaaS API | SaaS/OSS | SaaS API | SaaS API | agent-native CLI | CRDs | CRDs | no |
| Self-hostable | yes, any KVM cluster | partial OSS | no | OSS core | no | no | no | yes | yes | yes |
| Hosted option | planned (same engine) | yes | yes | yes | yes | yes | yes (only) | no | no | no |
| Your data stays on your infra | yes (self-hosted) | no | no | partial | no | no | no | yes | yes | yes |
| Open source | Apache 2.0 | partial | no | partial | no | no | no | Apache 2.0 | Apache 2.0 | Apache 2.0 |

SaaS runtimes (E2B, Modal, Daytona, Cloudflare) are fast, but your agents' code, data, and credentials run on someone else's infrastructure with no self-host path at equivalent capability. Morph built the right state model (branch/restore) as a proprietary cloud; our Workspace primitive targets the same semantics, open source, at fork(2) speeds. Agent Sandbox (k8s-sigs) is winning the Kubernetes API standard without a snapshot-fork engine, which is why we ship a conformance facade (`cmd/facade`) to be its fastest backend rather than fight it ([docs/facade-conformance.md](docs/facade-conformance.md)). Kata, KubeVirt, and raw Firecracker give you the isolation primitive and leave the pool, fork, distribution, and agent-API layers as your problem.

If an alternative beats us on an axis you care about and we have no roadmap line that closes it, that is a bug in our strategy: open an issue.

## Project status

Early development, pre-1.0 (latest release `v0.3.0`). Do not run untrusted code in production yet: there has been no external security review and some isolation controls remain open (see the [threat model](docs/threat-model.md) for the exact per-boundary status). The control plane is real end to end, proven in CI against mock engines and real Firecracker VMs, and exercised on a single-node Talos KVM cluster.

**Verified on a real KVM cluster (husk default):** warm-claim activate, blocking exec, `run_code` failing closed with `KernelUnavailable`, self-heal / re-pend, pool warming plus demand autoscaling, live sandbox fork (the source husk pod snapshots its VM and N child pods restore it via CoW, each an independent Ready child), durable forkable workspaces (create -> commit -> fork), and pod egress isolation (default-deny, cloud-metadata block, per-template allowlist), all proven inside a restored VM with no node prerequisite.

**Tracked tails not yet on the husk default:** streaming exec and the interactive PTY; live-VM memory snapshot hooks for resumable workspace heads (`--workspace-memory-snapshots`, fail-loud); S3/encryption live store-selection; the husk `{git}` workspace push; and multi-node N>1 (designed, single-node-verified).

[ROADMAP.md](ROADMAP.md) is the single source for what is done, in progress, and gated. The operating rule: this repository never describes a system that does not exist.

## Documentation

Per-topic docs live in [`docs/`](docs/). Start with the [quickstart](docs/quickstart.md), then:

| Topic | Doc |
|---|---|
| Templates and OCI image to rootfs build | [docs/templates.md](docs/templates.md) |
| Volume fork policies | [docs/volumes.md](docs/volumes.md) |
| Snapshot format, distribution | [docs/snapshot-format.md](docs/snapshot-format.md), [docs/snapshot-distribution.md](docs/snapshot-distribution.md) |
| Guest networking and egress | [docs/networking.md](docs/networking.md) |
| Encryption at rest, secrets | [docs/encryption.md](docs/encryption.md), [docs/secrets.md](docs/secrets.md) |
| Metering, scheduling, density | [docs/metering.md](docs/metering.md), [docs/scheduling.md](docs/scheduling.md) |
| Observability, failure and GC | [docs/observability.md](docs/observability.md), [docs/failure-gc.md](docs/failure-gc.md) |
| Fork-engine correctness | [docs/fork-correctness.md](docs/fork-correctness.md) |
| Durable workspaces | [docs/workspaces.md](docs/workspaces.md) |
| Threat model | [docs/threat-model.md](docs/threat-model.md) |
| `mitos` CLI, MCP server, Agent Skill | [docs/cli.md](docs/cli.md), [docs/mcp.md](docs/mcp.md), [skills/mitos/SKILL.md](skills/mitos/SKILL.md) |
| Guest port forwarding | [docs/ports.md](docs/ports.md) |
| Recipe: host an agent harness over HTTP | [docs/recipes/agent-harness.md](docs/recipes/agent-harness.md) |
| Recipe: headless Chromium / CDP for browser-automation agents | [docs/recipes/browser-automation.md](docs/recipes/browser-automation.md) |
| Migrating from E2B | [docs/migrating-from-e2b.md](docs/migrating-from-e2b.md) |
| Talos + Hetzner reference platform | [docs/platforms/talos-hetzner.md](docs/platforms/talos-hetzner.md) |
| Target API surface (v2 spec) | [docs/api/v2-spec.md](docs/api/v2-spec.md) |
| Benchmark methodology | [BENCHMARKS.md](BENCHMARKS.md) |

## Contributing

Contributions welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) and [CLAUDE.md](CLAUDE.md) for conventions, and the [issues page](https://github.com/mitos-run/mitos/issues) for work tracked against [ROADMAP.md](ROADMAP.md).

## Security

The threat model with per-boundary status lives in [docs/threat-model.md](docs/threat-model.md); no external security review has happened yet, and the document says exactly what is open. To report a vulnerability, see [SECURITY.md](SECURITY.md).

## License

[Apache 2.0](LICENSE).
