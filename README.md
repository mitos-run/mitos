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
  <a href="https://mitos.run/docs"><img alt="Docs" src="https://img.shields.io/badge/docs-mitos-blue"></a>
  <a href="https://discord.gg/zddgd2pgab"><img alt="Discord" src="https://img.shields.io/discord/1518722949295771759?label=discord&logo=discord&logoColor=white&color=5865F2"></a>
</p>

<p align="center">
  <a href="#quickstart">Quickstart</a> .
  <a href="https://mitos.run/docs">Documentation</a> .
  <a href="#features">Features</a> .
  <a href="#comparison">Comparison</a> .
  <a href="CONTRIBUTING.md">Contributing</a> .
  <a href="https://discord.gg/zddgd2pgab">Community</a>
</p>

<p align="center">
  <img alt="Mitos SDK: create a microVM sandbox, run code, and fork it into isolated parallel attempts" src="docs/assets/demo.gif" width="760">
</p>

---

## What is Mitos

Mitos gives every AI agent its own isolated computer: a hardware-isolated Firecracker microVM that runs untrusted code safely and that you can fork while it is running. A live copy-on-write fork branches one warm VM into N independent siblings in tens of milliseconds, so an agent can explore many attempts in parallel from a shared, ready state, and you pay only for the pages each sibling changes.

Run it on your own Kubernetes cluster today, where your agents' code, data, and credentials never leave your infrastructure, or on the hosted API with no nodes to manage. As far as we know, it is the only runtime that is open source, self-hostable, Kubernetes-native, and able to live-fork a running VM, all at once.

## Quickstart

### 1. Install and authenticate

```bash
pip install mitos-run
export MITOS_API_KEY=sk-...   # a key from https://mitos.run; no Kubernetes required
```

The SDK defaults to the hosted endpoint. The same code runs against your own cluster or a standalone sandbox-server by setting `MITOS_BASE_URL`. The key is resolved from the argument or `MITOS_API_KEY` and is never logged.

### 2. Create a sandbox and run code

```python
import mitos

sb = mitos.create("python")                  # Ready microVM sandbox (~27 ms warm-claim)
print(sb.exec("echo hello").stdout)          # hello

# Files and a stateful code interpreter hang off the same flat handle.
sb.files.write("/workspace/plan.txt", "draft")
print(sb.run_code("import math; math.sqrt(144)").text)   # 12.0
```

Full reference: [mitos.run/docs/quickstart](https://mitos.run/docs/quickstart).

### 3. Fork into parallel attempts

```python
# N-way copy-on-write fork of the live VM: each sibling lands warm and independent.
a, b = sb.fork(2)
a.exec("echo conservative > /workspace/plan.txt")
b.exec("echo aggressive  > /workspace/plan.txt")

sb.terminate()
```

The async client mirrors the same surface: `await mitos.aio.create("python")` returns an `AsyncDirectSandbox` with the same `exec` / `run_code` / `files` / `create_pty` / `fork` / `terminate`.

Blocking `exec` and `run_code` work on the husk default. Streaming exec (`sb.exec(..., on_stdout=...)`), background processes (`sb.exec_background(...)`), and the interactive PTY (`sb.create_pty()`) run on the engine path today and are being brought to the husk default; `run_code` returns a fail-closed `KernelUnavailable` until the kernel ships in the husk base image.

### Run it your way

Same engine, same API, more on-ramps. Depth is one click into [the docs](https://mitos.run/docs).

**Every language, two modes.** Each SDK speaks the same sandbox-server REST API in **direct mode** (standalone or hosted), and each also has **cluster mode** (an `AgentRun` that drives the `mitos.run/v1` CRDs through the Kubernetes API). Default-pool naming is byte-for-byte identical across all six.

| Language | Install | Direct | Cluster | SDK docs |
|---|---|---|---|---|
| Python | `pip install mitos-run` | sync + async | `AgentRun` | [sdk/python](sdk/python) |
| TypeScript | `npm i @mitos/sdk` | yes | `AgentRun` | [sdk/typescript](sdk/typescript/README.md) |
| Go | `go get github.com/mitos-run/mitos/sdk/go` | typed, `errors.Is`-friendly | `AgentRun` | [sdk/go](sdk/go/README.md) |
| Ruby | gem (stdlib only) | yes | `AgentRun` | [sdk/ruby](sdk/ruby/README.md) |
| Rust | crate (blocking) | yes | `AgentRun` | [sdk/rust](sdk/rust/README.md) |
| Java | JDK 17 (stdlib only) | yes | `AgentRun` | [sdk/java](sdk/java/README.md) |

The Go SDK ships in its own nested module (`github.com/mitos-run/mitos/sdk/go`), so importing it never pulls the controller into your build.

**On a cluster you run.** The two-tier `AgentRun` path drives the CRDs directly:

```python
from mitos import AgentRun

c = AgentRun()                                   # kubeconfig or in-cluster; autodetected
sb = c.sandbox("python", ready=True)             # claims a warm sandbox, waits Ready
print(sb.exec("python -c 'print(40 + 2)'").stdout)   # 42

fork_a, fork_b = sb.fork(2)                       # fork against shared warmed state
sb.terminate()
```

`c.sandbox("python")` lazily creates a default pool if you have none; pass `pool="my-pool"` to use an existing one. Errors raise `AgentRunError(code, cause, remediation)`. `AsyncAgentRun` mirrors the hot paths and adds `create_pty()` over WebSocket.

**CLI and MCP.**

The `mitos` CLI works against the hosted gateway (no cluster needed) or your own
Kubernetes cluster:

```bash
go install mitos.run/mitos/cmd/mitos@latest      # requires a Go toolchain

# Hosted mode: set MITOS_API_KEY, no kubeconfig required.
export MITOS_API_KEY=sk-...
mitos sandbox create --pool python               # create from the python template
mitos sandbox exec <id> "python3 -c 'print(42)'"
mitos fork <id> --count 2                        # fork into 2 independent siblings
mitos sandbox ls
mitos sandbox terminate <id>

# Cluster mode (kubeconfig): target your own Kubernetes nodes.
mitos sandbox create --pool dev-default
mitos run echo hello --pool dev-default
```

`mitos dev up` brings up a one-command local control plane on a mock engine for
cluster-mode development. An MCP server (`mitos-mcp`) exposes sandboxes as MCP
tools for any MCP-speaking agent, and an [Agent Skill](skills/mitos/SKILL.md)
teaches skill-aware agents the workflow. The full install matrix (script,
Homebrew, deb/rpm, scoop/winget, checksums) is in
[mitos.run/docs/install](https://mitos.run/docs/install).

**Drop into the agent you already use.** Each adapter is a thin shim over the same native ops (`exec`, `run_code`, `files`, `fork`), with no hard dependency on the framework package: Claude Code and opencode (MCP server + agent skill), the OpenAI Agents SDK, the Claude Agent SDK, LangChain / deepagents, Vercel AI SDK / Pydantic AI / AutoGen / LlamaIndex (standard MCP), and a "change one import" [E2B migration shim](https://mitos.run/docs/migrating-from-e2b) for teams leaving E2B's cloud. The [integrations hub](docs/integrations/README.md) indexes every path.

**Install the operator.**

```bash
kubectl apply -k deploy/
```

The self-contained kustomize base installs the CRDs, the controller (husk mode), the forkd DaemonSet, the `/dev/kvm` device plugin, and the PKI bootstrap, and applies on a real KVM node with no manual patches. Nodes need `/dev/kvm` and the label `mitos.run/kvm=true`. The Helm chart is published at `https://mitos.run/charts` (`helm repo add mitos https://mitos.run/charts`); see [deploy/charts/mitos](deploy/charts/mitos/README.md). Then declare a warm pool, and fork from it with a `Sandbox` whose `source.fromSandbox` points at a live session ([templates](docs/templates.md)):

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
```

## Why Mitos

Agent harnesses need fast, isolated environments where agents read and write files, install packages, and run untrusted code. Every existing option forces a trade: speed without ownership, isolation without forking, Kubernetes-native without warm starts, or durability locked inside someone else's cloud.

- **Live-fork a running VM.** N-way copy-on-write fork of a live microVM: daughters share the parent's memory pages until they write, so each fork lands in a warm, ready environment. Branch one agent into many parallel attempts.
- **~27 ms warm-claim activate.** Firecracker microVMs restore from a memory snapshot in the tens-of-milliseconds class: P50 ~27 ms on the bare-metal reference node, reproducible from [`bench/husk-activate-latency.sh`](bench/husk-activate-latency.sh).
- **Open source, self-hostable, Kubernetes-native.** As far as we know, the only runtime that does all three. You drive the whole lifecycle through declarative CRDs (`mitos.run`).

Two ways to run it:

- **Self-hosted (today):** any Kubernetes cluster with KVM nodes. Your data never leaves your infrastructure. Bare metal (Talos + Hetzner) is the first-class reference platform.
- **Hosted (in progress):** the same engine and API operated by us, for teams that want milliseconds without managing nodes.

> Two engine paths exist. The **husk pod-native path is the default**: each VM runs in its own unprivileged pod, and the source husk pod snapshots its running VM so N child pods restore it via CoW. The **raw-forkd path** runs forks in forkd's in-process engine. Everything here runs on the husk default unless explicitly marked `engine path`.

Sandboxes are not pods. Pod-scoped Kubernetes mechanisms (NetworkPolicy, ResourceQuota, PSA) govern the husk pod, not the workload inside the microVM; the sandbox is the VM, not the husk pod, and where we provide an equivalent it is documented as ours. The full claim and exec data paths and the component diagram are at [mitos.run/docs/architecture](https://mitos.run/docs/architecture).

## Features

The husk pod-native path is the default. A few capabilities run today only on the raw-forkd `engine path` and are marked, with a link to the tracking issue.

### Speed

| Capability | What you get | Docs |
|---|---|---|
| Warm-claim activate | P50 ~27 ms on the bare-metal reference node (snapshot load + fork-correctness handshake + guest-ready); ~6-16 ms snapshot restore; ~3 MiB marginal memory per fork via CoW page sharing | [BENCHMARKS.md](BENCHMARKS.md) |
| Pre-snapshotted pools | OCI images flattened to ext4 rootfs and warmed with your `init` steps before snapshotting, so there is no cold start on claim | [docs/templates.md](docs/templates.md) |
| CoW memory sharing | You pay for unique pages across forks, not for copies | [mitos.run/docs/metering](https://mitos.run/docs/metering) |
| Content-addressed distribution | Forks pull only the missing sha256 chunks from a holder over mTLS; rebuilds ship deltas under a version-compatibility contract | [docs/snapshot-distribution.md](docs/snapshot-distribution.md) |

### Isolation

| Capability | What you get | Docs |
|---|---|---|
| Hardware isolation per session | A dedicated kernel per sandbox (KVM/Firecracker); on the husk default each VM runs in its own unprivileged, PSA-restricted pod, which is the per-VM boundary | [mitos.run/docs/threat-model](https://mitos.run/docs/threat-model) |
| No silent secret inheritance | Live forks of secret-holding sandboxes are rejected unless explicitly opted in; credentials are injected at claim time over vsock, never baked into snapshots | [mitos.run/docs/threat-model](https://mitos.run/docs/threat-model) |
| Default-deny egress | An in-pod nftables default-deny filter in the pod's own netns (CNI-independent), with an unconditional cloud-metadata (169.254.169.254) block and a per-template allowlist by IP:port and by name through an in-pod DNS proxy. Verified end to end on a real KVM cluster; the guest cannot influence enforcement | [mitos.run/docs/networking](https://mitos.run/docs/networking) |
| Encryption at rest | Per-scope LUKS2 containers with crypto-shredding and KMS envelope wrapping (behind `--enable-encryption`, fail-closed); HSM-backed keys and per-workspace scope are follow-ups | [docs/encryption.md](docs/encryption.md) |

### Agent DX

| Capability | What you get | Docs |
|---|---|---|
| Blocking exec | Correct stdout and exit code over the sandbox API | [mitos.run/docs/cli](https://mitos.run/docs/cli) |
| Streaming exec and PTY | Incremental stdout/stderr, background processes, and a token-gated interactive WebSocket terminal (`engine path`) | [mitos.run/docs/cli](https://mitos.run/docs/cli) |
| Code interpreter | `run_code` with a stateful kernel and rich multi-MIME results, in every SDK and the MCP server; fail-closed `KernelUnavailable` until the kernel ships in the husk base image | [mitos.run/docs/mcp](https://mitos.run/docs/mcp) |
| LLM-legible errors | Every failure carries `{code, cause, remediation}`, parsed by the SDKs into a structured `AgentRunError` | [docs/api/errors.md](docs/api/errors.md) |

### Kubernetes-native

| Capability | What you get | Docs |
|---|---|---|
| Declarative CRDs | `SandboxPool`, `Sandbox` (poolRef/fromSandbox/fromRevision source), `Workspace`/`WorkspaceRevision` in `mitos.run/v1` with volume topology and fork behavior | [docs/templates.md](docs/templates.md) |
| Pod-native execution | Each per-sandbox VM runs in an unprivileged pod (`/dev/kvm` from a device plugin, not `privileged`), so CPU/memory requests are scheduler truth and PSA governs the pod | [mitos.run/docs/threat-model](https://mitos.run/docs/threat-model) |
| Capacity-aware scheduling | CoW bin-packing onto warm holders, a CoW-aware overcommit budget, a `MaxSandboxes` host-DoS ceiling with atomic slot reservation, and typed `NoCapacity` backpressure instead of OOMing a node | [docs/scheduling.md](docs/scheduling.md) |
| Demand-driven autoscaling | `SandboxPool.spec.autoscale` scales the dormant husk-pod count to `clamp(inUse + targetSpare, minWarm, maxWarm)` with an anti-thrash cooldown; a fixed pool is just `minWarm == replicas` | [docs/scheduling.md](docs/scheduling.md) |
| Failure and GC semantics | Claim TTLs, orphan-VM sweeps, controller-restart reconciliation, forkd crash reaping via an on-disk journal, node-loss handling, and saturation backpressure, all CI-proven | [docs/failure-gc.md](docs/failure-gc.md) |

### Durable state

| Capability | What you get | Docs |
|---|---|---|
| Durable forkable workspaces | `Workspace`/`WorkspaceRevision` CRDs: durable, versioned, forkable agent state independent of any sandbox. `/workspace` hydrates on start and a committed revision dehydrates on terminate over the content-addressed store. Verified create -> commit -> fork on a real KVM cluster | [mitos.run/docs/workspaces](https://mitos.run/docs/workspaces) |
| Outputs and diff | `spec.lifetime.onTerminate.outputs` narrows the dehydrate to listed subtrees; `{diff: true}` records a content-hash diff against the parent head | [mitos.run/docs/workspaces](https://mitos.run/docs/workspaces) |
| Git rendezvous | A `{git}` output pushes per-attempt branches to a rendezvous remote (the engine pushes; a human or CI merges). Best-effort on husk today | [mitos.run/docs/workspaces](https://mitos.run/docs/workspaces) |
| Dev-environment URL | `mitos workspace serve <ws> --pool P` warm-claims a forked sandbox bound to the workspace and returns a ready `https://<label>.<expose-domain>/` URL; each forked session gets its own URL | [docs/recipes/dev-environment.md](docs/recipes/dev-environment.md) |

### Operable

| Capability | What you get | Docs |
|---|---|---|
| Metrics and tracing | Node and controller Prometheus metrics, a per-claim OpenTelemetry trace (`--otlp-endpoint`), and a toggleable structured audit log (`--audit-log`) recording command/path and byte counts, never content or secrets | [mitos.run/docs/observability](https://mitos.run/docs/observability) |
| CoW-aware metering | The shared template page set is counted once, not once per fork, so billing and scheduling reflect the honest physical footprint | [mitos.run/docs/metering](https://mitos.run/docs/metering) |
| Operator tooling | `kubectl mitos` plugin (`ls` / `ps`) and the operational `GET /v1/metering` report | [mitos.run/docs/observability](https://mitos.run/docs/observability) |
| Bare metal first-class | Talos + Hetzner is the reference platform | [docs/platforms/talos-hetzner.md](docs/platforms/talos-hetzner.md) |

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

Early development, pre-1.0 (latest release `v0.3.0`). Do not run untrusted code in production yet: there has been no external security review and some isolation controls remain open (see the [threat model](https://mitos.run/docs/threat-model) for the exact per-boundary status). The control plane is real end to end, proven in CI against mock engines and real Firecracker VMs, and exercised on a single-node Talos KVM cluster.

**Verified on a real KVM cluster (husk default):** warm-claim activate, blocking exec, `run_code` failing closed with `KernelUnavailable`, self-heal / re-pend, pool warming plus demand autoscaling, live sandbox fork (the source husk pod snapshots its VM and N child pods restore it via CoW, each an independent Ready child), durable forkable workspaces (create -> commit -> fork), and pod egress isolation (default-deny, cloud-metadata block, per-template allowlist).

**Tracked tails not yet on the husk default:** streaming exec and the interactive PTY; live-VM memory snapshot hooks for resumable workspace heads; S3/encryption live store-selection; the husk `{git}` workspace push; and multi-node N>1 (designed, single-node-verified).

[ROADMAP.md](ROADMAP.md) is the single source for what is done, in progress, and gated. The operating rule: this repository never describes a system that does not exist.

## Local development (no KVM required)

`mitos dev up` brings up a local kind cluster on a mock control plane and the `mitos` CLI drives the full claim path; the mock engine reconciles claims to `Ready` and exercises control-plane dispatch, but a real in-VM `exec` needs a node with `/dev/kvm`. For the no-cluster REST loop, run `go run ./cmd/sandbox-server --mock --addr :8080` and point the Python SDK at it. The full kind walkthrough is at [mitos.run/docs/cli](https://mitos.run/docs/cli).

## Documentation

Full documentation lives at **[mitos.run/docs](https://mitos.run/docs)**: quickstart, architecture, SDK and CLI reference, sandbox lifecycle, workspaces, networking, and the threat model, all rendered from this repository.

The complete long tail (templates, snapshot format and distribution, encryption and secrets, scheduling and density, failure and GC, fork-engine correctness, recipes, and the target v2 API spec) lives in [`docs/`](docs/) in this repo. Benchmark methodology is in [BENCHMARKS.md](BENCHMARKS.md).

## Contributing

Contributions welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) and [CLAUDE.md](CLAUDE.md) for conventions, and the [issues page](https://github.com/mitos-run/mitos/issues) for work tracked against [ROADMAP.md](ROADMAP.md).

## Security

The threat model with per-boundary status lives at [mitos.run/docs/threat-model](https://mitos.run/docs/threat-model); no external security review has happened yet, and the document says exactly what is open. To report a vulnerability, see [SECURITY.md](SECURITY.md).

## License

[Apache 2.0](LICENSE).
