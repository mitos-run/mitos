# Rust guest agent: opt-in rootfs selector and production cutover strategy

This document describes the opt-in mechanism for baking the Rust guest agent
(`guest/agent-rs`) as the VM's `/init`, the honest status of that agent relative
to the production data path, the prerequisite workstream that must land before a
real production default-flip, and the soak/canary/rollback procedure.

## What the Rust agent is and is not today

The Rust agent (`guest/agent-rs`) is fully implemented, conformance-validated,
and benchmark-validated over the gRPC contract. It serves **only** the gRPC
protocol on vsock port 53 (`AgentGRPCPort`). It does NOT serve the legacy JSON
protocol on vsock port 52 (`AgentPort`).

The Go agent (`guest/agent`) serves **both** protocols: JSON on port 52 and gRPC
on port 53. It is the production default and will remain so until the prerequisite
workstream described below completes.

## What "baking the Rust agent as /init" actually enables

Baking the Rust agent as `/init` (via `AGENT_IMPL=rust`, described below) makes
the gRPC surface fully functional:

- The agent conformance harness (`guest/agent-rs/tests/conformance.rs`) passes.
- The bench tool (`cmd/bench`) can exercise exec latency and throughput via
  `--exec-transport grpc` (gRPC `Control.Ping` over `AgentGRPCPort`).
- Fork-correctness: the Rust agent's `NotifyForkedHandler` runs the same
  credited `RNDADDENTROPY` reseed, `CLOCK_REALTIME` step, `SIGUSR2` signal,
  per-fork network reconfiguration, and per-fork volume mount as the Go agent.

What it does NOT enable today is the **production fork/exec/file data path**,
because that path speaks the legacy JSON protocol on port 52. The following
host-side callers must be migrated before a Rust-only rootfs can function in
production:

| File | Role | Protocol used |
|---|---|---|
| `internal/fork/uffd_engine.go` | fork-readiness check, first-exec after restore | JSON on AgentPort (52) |
| `internal/daemon/sandbox_api.go` | forkd exec and file API | JSON on AgentPort (52) |
| `internal/firecracker/template.go` | template create (agent readiness ping) | JSON on AgentPort (52) |
| `internal/husk/stub.go` | husk pool activate, NotifyForked handshake | JSON on AgentPort (52) |
| `cmd/test-agent/main.go` | KVM smoke test agent client | JSON on AgentPort (52) |
| `cmd/bench/main.go` | bench default mode (json transport) | JSON on AgentPort (52) |
| `cmd/vol-smoke/main.go` | volume smoke test | JSON on AgentPort (52) |
| `cmd/net-fork-smoke/main.go` | network fork smoke test | JSON on AgentPort (52) |
| `cmd/mem-smoke/main.go` | memory smoke test | JSON on AgentPort (52) |
| `cmd/ws-smoke/main.go` | workspace smoke test | JSON on AgentPort (52) |
| `cmd/pull-smoke/main.go` | pull smoke test | JSON on AgentPort (52) |
| `cmd/tmpl-smoke/main.go` | template smoke test | JSON on AgentPort (52) |

Until all of those callers are migrated from the JSON protocol (port 52) to the
gRPC protocol (port 53), a rootfs baked with `AGENT_IMPL=rust` will produce a VM
whose `/init` does not answer on port 52, causing the template build to hang at
the readiness ping, and all production exec/file/fork operations to fail.

**Do not flip the default** (i.e., do not change `guest/rootfs/build.sh` default
from `go` to `rust`) until the prerequisite workstream below is complete.

## Prerequisite workstream: JSON->gRPC host-caller migration (SP1.5)

The hard gate for a real production default-flip is a dedicated migration
workstream, referred to here as **SP1.5**, that rewrites every host-side guest
caller listed above from the legacy JSON protocol (vsock port 52) to the gRPC
contract (vsock port 53). When SP1.5 is complete:

- All host callers speak gRPC; the JSON protocol listener on port 52 is only
  served by the Go agent for backward compatibility during transition.
- The Rust agent is a true drop-in: baking it as `/init` produces a VM that
  passes all smoke tests, the KVM fork-correctness suite, and the production
  data path.
- The default can be changed from `go` to `rust` in a single PR.

SP1.5 is tracked as a follow-up to the SP1 gRPC feature branch. No part of this
document or the SP1 branch changes the default; the Rust agent is opt-in only.

## Opt-in rootfs selector (AGENT_IMPL)

`guest/rootfs/build.sh` supports a per-bake agent selector via the `AGENT_IMPL`
environment variable:

```
AGENT_IMPL=go   (default, also the behavior when unset)
AGENT_IMPL=rust (opt-in, gRPC-only, for bench and conformance)
```

### Go agent (default)

```bash
# Default: Go agent, no env var needed.
./guest/rootfs/build.sh /path/to/rootfs.ext4

# Explicit:
AGENT_IMPL=go ./guest/rootfs/build.sh /path/to/rootfs.ext4
```

Bakes `guest/agent` as `/init`. The Go agent serves JSON on port 52 and gRPC on
port 53. This is the production path.

### Rust agent (opt-in)

```bash
AGENT_IMPL=rust ./guest/rootfs/build.sh /path/to/rootfs.ext4
```

Bakes `guest/agent-rs` as `/init`. Requires:

- A Rust toolchain with the `x86_64-unknown-linux-musl` target installed.
- The `cargo` binary on `$PATH`.
- Linux (the musl static build is Linux-only; the vsock feature requires Linux).

The build invocation used inside the script:

```bash
cargo build --release --target x86_64-unknown-linux-musl --features vsock
```

The resulting binary is a static musl binary with no dynamic dependencies.

### Reversibility

The selector affects only `/init` inside the rootfs image. Re-baking with
`AGENT_IMPL=go` (or with `AGENT_IMPL` unset) replaces `/init` with the Go agent.
No other rootfs content changes. The selector is fully reversible; no VM state
is modified.

To restore the Go agent on an existing template, rebuild the rootfs:

```bash
# Re-bake the Go agent (restores production default):
./guest/rootfs/build.sh /path/to/rootfs.ext4
```

Then rebuild the Firecracker template snapshot from the new rootfs via the
normal template build flow.

## Soak and canary procedure (for use AFTER SP1.5 lands)

This procedure is for a future state where SP1.5 is complete and the Rust agent
is a candidate for production default. Do not run this against production before
SP1.5 is complete.

### Gate 1: agent conformance harness

The conformance harness (`guest/agent-rs/tests/conformance.rs`) exercises all 15
runtime RPCs (Exec, ReadFile, WriteFile, List, Stat, Mkdir, Remove, Archive,
Upload, Watch, Processes, Signal, PortForward, Vitals, RunCode) against the Rust
agent over a Unix socket. It must pass cleanly before proceeding.

```bash
cd guest/agent-rs
cargo test --test conformance
```

### Gate 2: bench gate on box1 + box2

Run the bench harness against a Rust-agent rootfs on the bare-metal KVM boxes
(box1 and box2). The gRPC exec round-trip latency must meet the target recorded
in `bench/` results (see `bench/` for the current baseline).

```bash
# On box1 or box2 (KVM-capable):
AGENT_IMPL=rust ./guest/rootfs/build.sh /tmp/bench-rootfs-rust.ext4
# Build template snapshot from the new rootfs, then:
cmd/bench --exec-transport grpc ...
```

### Gate 3: fork-correctness CI suite green

The KVM fork-correctness job (`kvm-test.yaml`, the `firecracker-test` job) must
pass against a Rust-agent rootfs. This job asserts distinct `/dev/urandom`,
distinct kernel UUID, distinct TLS client random, and each fork wall-clock within
2s of the runner, all of which depend on the Rust agent's `NotifyForkedHandler`
running the credited CRNG reseed and clock step correctly.

### Gate 4: threat model and fork-correctness docs current

`docs/threat-model.md` and `docs/fork-correctness.md` must reflect the Rust
agent path and be reviewed by the named human reviewer before merge. This is
done in the SP1 branch (see the sections added by this commit).

### Canary pool on box2's k3s cluster

After gates 1-4 are green, deploy a single SandboxPool with a Rust-agent rootfs
on box2's k3s cluster and soak for at least 48 hours under real workload. Monitor
for any fork-readiness failures, exec errors, or JSON-protocol fallback attempts
(which would indicate a missed SP1.5 migration).

### Default-flip PR

Once the canary soak is clean, a single PR changes the default in
`guest/rootfs/build.sh` from `go` to `rust`. That PR must:

- Update this document to mark the default-flip as done.
- Update `docs/threat-model.md` to note the Go agent JSON listener as a
  compatibility shim (if retained) or removed.
- Update `docs/api/runtime-protocol.md` to note that the Rust binary now serves
  the protocol by default.
- Carry a benchmark result from `bench/` showing the Rust agent meets or exceeds
  the Go agent on the hot exec path.
- Have a named human reviewer approval (see the reviewer policy below).

### Rollback

Rollback is re-baking the rootfs with `AGENT_IMPL=go` and rebuilding the
Firecracker template snapshot. No other change is needed. The Go agent remains
in the codebase and is never removed.

## Security-sensitive reviewer policy

The Rust agent codebase (`guest/agent-rs`) requires a named human reviewer before
any PR touching the following paths is merged to main:

- `guest/agent-rs/src/sys/` (all files: entropy, clock, signal, mount, netlink,
  vsock, pty)
- `guest/agent-rs/src/fork/` (all files: reseed, clock, signal, network, volumes)
- `guest/agent-rs/src/init/mod.rs`
- `guest/agent-rs/src/main.rs`

This mirrors the existing policy for `guest/agent` (the Go agent), stated in
`CLAUDE.md` and `docs/threat-model.md`. The rootfs build script
(`guest/rootfs/build.sh`) is not in this set, but any change to the Rust build
invocation (target, features, profile) must be reviewed alongside the agent code.
