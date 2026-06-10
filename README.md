# sandbox

Snapshot-fork sandboxes for AI agents on Kubernetes — Firecracker microVMs,
CoW memory forking, declarative CRDs. Self-hosted on any cluster with KVM.

**Project status: early development, pre-alpha.** This README distinguishes,
for every feature, between what is implemented and tested today and what is
design. Nothing here is production-ready, no performance claim is made that
is not backed by a measurement you can reproduce, and **no external security
review has happened** — do not run untrusted code with this in production yet.
See [ROADMAP.md](ROADMAP.md) and [docs/threat-model.md](docs/threat-model.md).

## What works today (tested in CI)

| Capability | Where | Verified by |
|---|---|---|
| Firecracker VM boot → pause → snapshot → restore in a fresh VMM process (CoW via `mmap(MAP_PRIVATE)`) | `internal/firecracker`, `internal/fork` | `kvm-test` workflow on KVM runners |
| Guest agent as PID 1: exec + file ops over vsock | `guest/agent`, `internal/vsock` | end-to-end CI test (boot real VM, exec via vsock) |
| CRDs (`SandboxTemplate`, `SandboxPool`, `SandboxClaim`, `SandboxFork`) and controllers | `api/v1alpha1`, `internal/controller` | envtest suite |
| Standalone REST server (no k8s) with mock or KVM engine | `cmd/sandbox-server` | Python SDK direct-mode tests |
| Python SDK | `sdk/python` | unit tests |
| Mock fork engine for development without KVM (kind, macOS) | `internal/fork/mock.go` | unit + kind e2e |

## What is design, not yet implementation

Honest gaps as of this commit — tracked in [ROADMAP.md](ROADMAP.md):

- **controller ↔ forkd wiring** — being implemented now
  (`docs/superpowers/plans/2026-06-10-control-plane-wiring.md`). Until it
  lands, applying a `SandboxClaim` does not produce a VM.
- **Volume fork policies** (`Fresh`/`Share`/`Clone`/`Snapshot`) — the API
  and handlers exist, but they do not yet provision or attach anything to VMs.
- **Secret injection into the guest** — claim-time resolution exists;
  delivery into the VM does not yet.
- **Guest networking and egress allowlists** — restored VMs currently have
  no network device. Exec and file I/O run over vsock. Egress policy
  enforcement (host-side nftables/eBPF, controlled DNS) is designed but not
  built.
- **Fork correctness after restore** — RNG reseeding, clock resync, network
  identity, and live-fork secret hygiene are specified with tests in
  [docs/fork-correctness.md](docs/fork-correctness.md), none implemented yet.
- **TypeScript SDK, Helm chart, benchmark suite** — do not exist yet.

## Performance

We make no latency claim that is not reproducible from this repository.

- The mechanism is the right one for sub-10ms forks: Firecracker snapshot
  restore with lazily-faulted, CoW-shared snapshot memory. The CI workflow
  measures the restore API call on shared GitHub runners (typically tens of
  ms there, including process start).
- **Targets, not yet measured:** claim→first-successful-exec P50 < 50ms on
  bare metal; fork→first-exec P50 < 10ms. The number that matters is
  end-to-end time to a responsive guest — not the KVM restore syscall — and
  that is what `bench/` will measure when it lands (roadmap §4), published
  with hardware and methodology in `BENCHMARKS.md`.
- Per-fork memory: CoW makes the T=0 dirty-page footprint small, but T=0 is
  not a density-planning number. We will publish unique-memory-over-lifetime
  under realistic workloads alongside it.

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│  k8s Control Plane                                          │
│                                                             │
│  SandboxTemplate ──► SandboxPool ──► SandboxClaim           │
│        │                  │               │                 │
│        │            (manages snapshot     (fork from        │
│        │             lifecycle)            snapshot)        │
│        ▼                  │               │                 │
│  SandboxFork              │               │                 │
│  (fork running sandbox)   │               │                 │
│                           ▼               ▼                 │
│  ┌───────────────────────────────────────────────────────┐ │
│  │  controller (Deployment)                              │ │
│  │  reconciles CRDs, picks nodes, calls forkd over gRPC  │ │
│  └───────────────────────────────────────────────────────┘ │
└──────────────┬──────────────────────────────────────────────┘
               │ gRPC
┌──────────────▼──────────────────────────────────────────────┐
│  KVM-capable nodes                                          │
│  ┌───────────────────────────────────────────────────────┐  │
│  │  forkd (DaemonSet)                                    │  │
│  │  - builds template snapshots (boot → init → snapshot) │  │
│  │  - forks: new Firecracker process restores the        │  │
│  │    snapshot; memory is CoW-shared across forks        │  │
│  │  - serves exec/files over HTTP, bridged to the guest  │  │
│  │    agent via vsock                                    │  │
│  │  ┌─────┐ ┌─────┐ ┌─────┐ ┌─────┐                      │  │
│  │  │ VM  │ │ VM  │ │ VM  │ │ VM  │  ← one KVM microVM   │  │
│  │  └─────┘ └─────┘ └─────┘ └─────┘    per sandbox       │  │
│  └───────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────┘
```

Sandboxes are **not pods**. Pod-scoped Kubernetes mechanisms — NetworkPolicy,
pod resource quotas, pod security admission — do not apply to sandbox VMs.
Where we provide an equivalent (our own egress enforcement, our own capacity
accounting), it is documented as ours. See the
[threat model](docs/threat-model.md) for exactly what isolation you get.

## Quick start

### Local development (no KVM required)

```bash
# Standalone server with the mock engine
go run ./cmd/sandbox-server --mock --addr :8080

# Python SDK against it
cd sdk/python && pip install -e .
python -c "
from agent_run.direct import SandboxServer
s = SandboxServer('http://localhost:8080')
s.create_template('python')
sb = s.fork('python')
print(sb.fork_time_ms, 'ms (mock)')
"
```

### On a cluster (current state)

```bash
kubectl apply -f deploy/crds/
kubectl apply -f deploy/controller/
kubectl apply -f deploy/daemon/
```

This installs CRDs, controller, and forkd. Until roadmap §0 lands, claims do
not yet produce VMs end-to-end — track progress in [ROADMAP.md](ROADMAP.md).
There is no Helm chart yet.

### Real VMs on a KVM host

The `kvm-test` CI workflow (`.github/workflows/kvm-test.yaml`) is a complete,
runnable recipe: install Firecracker v1.15.0, fetch a kernel/rootfs, build the
guest agent into a rootfs as `/init`, boot, snapshot, restore, and exec
through vsock.

## API

The CRD surface (`agentrun.dev/v1alpha1`, may break between releases):

```yaml
apiVersion: agentrun.dev/v1alpha1
kind: SandboxTemplate
metadata:
  name: python-agent
spec:
  image: python:3.12-slim
  init: ["pip install numpy pandas requests"]
  resources: { cpu: "1", memory: "512Mi" }
  volumes:
    - name: workspace
      size: 5Gi
      forkPolicy: Snapshot     # design — not yet enforced
  networkPolicy:               # design — not yet enforced
    egress: deny
---
apiVersion: agentrun.dev/v1alpha1
kind: SandboxPool
metadata:
  name: python-agent-pool
spec:
  templateRef: { name: python-agent }
  replicas: 10
---
apiVersion: agentrun.dev/v1alpha1
kind: SandboxClaim
metadata:
  name: agent-session-1
spec:
  poolRef: { name: python-agent-pool }
  secrets:
    - name: openai-key        # resolved at claim time; in-guest delivery
      secretRef:              # is roadmap §0 — see threat model §6
        name: agent-secrets
        key: OPENAI_API_KEY
---
apiVersion: agentrun.dev/v1alpha1
kind: SandboxFork
metadata:
  name: parallel-attempt
spec:
  sourceRef: { name: agent-session-1 }
  replicas: 3
```

### Fork policies (design)

| Policy | Intended behavior | Status |
|--------|-------------------|--------|
| `Fresh` | New empty volume | API only |
| `Share` | Re-mount same backing store read-only | API only |
| `Snapshot` | CoW snapshot (btrfs/reflink) | API only |
| `Clone` | Full copy via CSI VolumeSnapshot | API only |

### Python SDK

```bash
cd sdk/python && pip install -e .   # not yet published to PyPI
```

Two modes: `agent_run.direct.SandboxServer` (standalone server, works today)
and `agent_run.Sandbox`/`AgentRun` (Kubernetes CRDs; functional once roadmap
§0 lands). A TypeScript SDK is planned but does not exist.

## Monitoring

forkd exports Prometheus metrics at `/metrics` (HTTP port, default `:9091`):

| Metric | Type | Description |
|--------|------|-------------|
| `agentrun_fork_duration_seconds` | histogram | Fork latency as measured by forkd |
| `agentrun_active_sandboxes` | gauge | Running sandboxes on this node |
| `agentrun_memory_shared_bytes` | gauge | CoW-shared memory across forks |
| `agentrun_memory_unique_bytes` | gauge | Per-fork unique memory at fork time (T=0 only — lifetime tracking is roadmap §1) |

## Node requirements

Nodes need `/dev/kvm` and the label `agentrun.dev/kvm=true`. Bare metal is a
first-class target (Talos + Hetzner reference platform is roadmap §5). Nested
virtualization on EKS/GKE/AKS works for development; we will publish the
nested-virt performance penalty rather than pretend it doesn't exist.

## Comparison

A numbers table belongs here only when `bench/` can regenerate it on the same
hardware against the actual competitors (roadmap §4). Until then, the honest
qualitative positioning:

- **E2B / Modal / Daytona (SaaS):** mature, fast, great DX — but your agents'
  code and data run on their infrastructure. We are self-hosted only.
- **Agent Sandbox (k8s-sigs):** k8s-native CRDs with Kata/gVisor isolation;
  no snapshot-fork primitive. We are building exactly that primitive, and an
  adapter for their CRDs is under consideration (roadmap §7).
- **Raw Firecracker:** the engine we build on; you'd be writing the pool,
  fork, distribution, and k8s layers yourself.

## Security

Read [docs/threat-model.md](docs/threat-model.md) before trusting this with
anything. Current honest summary: KVM/Firecracker isolation per sandbox is
real; the jailer, mTLS between components, API authentication, snapshot
integrity verification, and fork-correctness guarantees (RNG, clock, secrets)
are **not implemented yet**. No external security review has been performed.

## Contributing

`make build test` runs everything that works without KVM. `make
test-controller` runs the envtest suite. The KVM path runs in CI on every PR
touching the engine. Every PR that moves the security surface must update the
threat model in the same PR; every README claim must stay reproducible.

## License

Apache 2.0
