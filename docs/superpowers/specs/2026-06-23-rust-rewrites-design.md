# Rust rewrites: design spec

Status: approved design, pending implementation plans. Date: 2026-06-23. Relates to: #310 (spike), #24 (Connect/gRPC runtime protocol).

## Context

The #310 spike measured a Go vs Rust guest agent on bare metal (box1, Intel i7-6700, Firecracker v1.15.0). Findings, reproduced across 3 runs:

- fork to first exec: Rust ~12 percent faster (p50 98.9 to 100.6 ms vs 113.2 to 114.4 ms), no overlap.
- exec round trip: no median win (distributions overlap); Rust tail tighter.
- static binary: Rust 637 KB vs Go 4.97 MB (~7.6x smaller), for the hand-rolled JSON-lines spike agent.
- per-VM RSS at a 256 MiB guest: within 0.5 percent.

Two assumptions fixed by the maintainer for this work:

1. The gRPC-over-vsock runtime protocol migration (`feat/connect-runtime-protocol`, #24) is COMPLETE and merged before this work starts. The guest boundary is therefore the `sandbox.v1` gRPC proto, not the legacy JSON-lines loop. The #310 spike agent (JSON-lines) is throwaway; the real agent targets the proto.
2. Maximize valuable Rust. "Valuable" is the gate: rewrite where it makes the service measurably better for the user with no regression, and nowhere it would not.

The governing constraint is Pareto improvement: every rewrite must be better on at least one user-visible axis (latency, footprint, safety) and worse on none. Anything that cannot clear that bar is out of scope.

## Goals

- A production Rust guest agent at full `sandbox.v1` parity that is Pareto-better than the Go agent: faster on fork to first exec, smaller, memory-safe, with no regression on any RPC, any latency metric, fork-correctness, or security.
- A measurement-first decision on the fork-engine hot path (UFFD plus restore), mirroring #310's discipline, before any engine rewrite is committed.
- Rust code a professional Rust engineer would respect: latest stable, edition 2024, memory safety as architecture, no panics on the request path, deny-level lints, audited minimal dependencies.

## Non-goals

- Rewriting the control plane (controller, CRD reconcilers, client-go / controller-runtime), forkd's Kubernetes-facing gRPC, or the sandbox-server HTTP layer. The #310 data shows these are off the latency-critical path (claim latency is dominated by the ~100 ms Firecracker restore, not Go reconcile code), and a rewrite would trade a mature Go ecosystem for no user benefit. These stay Go. Including them would violate "no regression."
- Committing to a fork-engine rewrite before SP2 produces numbers.

## The value gate (per-component decision)

| Component | Decision | Rationale |
| --- | --- | --- |
| Guest agent (`guest/agent`) | IN: full Rust | Reproduced ~12 percent fork-exec win, far smaller binary, PID-1 memory safety on a security-sensitive path. The one component the data justifies. |
| Fork-engine memory hot path (`internal/fork/uffd*`, restore) | MEASURE-FIRST | Tight per-page-fault systems code in Rust's wheelhouse, but #310 never measured it (both templates ran the same Go engine). Spike, then rewrite only proven-hot pieces behind the engine interface. |
| Fork engine orchestration (`internal/fork/engine.go`), firecracker client | DEFERRED to SP2 outcome | Restore is largely Firecracker/kernel bound, so latency headroom is unknown. Only revisited if SP2 shows broad engine headroom. |
| Control plane, forkd K8s gRPC, sandbox-server HTTP | OUT: stays Go | Off the hot path; Go ecosystem is load-bearing; rewrite is negative ROI and a regression risk. |

This yields two sub-projects, each with its own implementation plan:

- SP1: Rust guest agent (full `sandbox.v1` gRPC parity). Do first; fully data-justified.
- SP2: fork-engine Rust hot-path measurement spike. Do next; rewrite gated on its numbers.

A possible SP3 (a Rust "forkcore" replacing the engine behind its interface) is conjured only if SP2's numbers justify it, with its own spec.

---

## SP1: Rust guest agent

### Parity surface (the no-regression definition)

The Rust agent must serve every RPC the Go guest agent serves over `sandbox.v1` gRPC, with byte-identical proto behavior:

15 runtime RPCs: Exec, ReadFile, WriteFile, List, Stat, Mkdir, Remove, Archive, Upload, Watch, Processes, Signal, PortForward, Vitals, RunCode.

Self-service RPCs (Fork, Checkpoint, ExtendLifetime, Budget) are host/forkd/controller side, NOT served by the guest, so they are out of agent scope.

5 fork-correctness actions on the notify-forked path (the security-critical seam):

1. Credited CRNG reseed with the host-supplied entropy via the `RNDADDENTROPY` ioctl, FAIL CLOSED (return false so the host reaps a fork that could not be credibly reseeded). The #310 spike used an uncredited plain write; that is a regression and is explicitly corrected here.
2. CLOCK_REALTIME step when drift past the snapshot exceeds the 500 ms threshold.
3. SIGUSR2 to userspace processes so language runtimes reseed their PRNGs.
4. Per-fork network reconfiguration of eth0 (ip addr, default route) from the delivered network identity.
5. Per-fork volume mounts from the delivered mount table.

The substantial ports are RunCode (manage the ipykernel subprocess and speak the kernel protocol; the Go `kernel.go` is the reference), PortForward (bidirectional TCP-over-vsock), Watch (filesystem events), Archive/Upload (tar streaming), and the network/volume fork path. Total Go behavior to port: about 3,200 non-test LOC.

### Architecture

- Crate `guest/agent-rs/` (evolves the #310 spike crate). Binary is PID-1 `/init`.
- Stack: `tonic` (gRPC) on `tokio` over `tokio-vsock`. The `sandbox.v1` proto is compiled with `tonic-build` from the SAME `proto/sandbox/v1/sandbox.proto` the Go side uses, so the contract cannot drift.
- Module boundaries (each one purpose, testable in isolation):
  - `service/` one module per RPC group (exec, files, watch, portforward, runcode, vitals, archive), each implementing its slice of the tonic service trait.
  - `fork/` the notify-forked correctness actions (reseed, clock, signal, network, volumes).
  - `kernel/` the RunCode ipykernel bridge.
  - `sys/` the ONLY module with `unsafe`: AF_VSOCK, the libc fork-correctness syscalls (RNDADDENTROPY, clock_settime, sethostname, mount), wrapped in safe APIs.
  - `init/` PID-1 mount/hostname bring-up.
- Honest tradeoff: a gRPC service needs an async runtime, so the binary grows beyond the spike's 637 KB hand-rolled figure (still far below Go's 4.97 MB). The real size is re-measured as a gate, not assumed.

### Rust engineering standard

- Latest stable toolchain pinned via `rust-toolchain.toml` (box1 is on 1.96.0, 2026-05), edition 2024, locked `Cargo.lock`, static musl build.
- `#![deny(unsafe_code)]` crate-wide; `unsafe` confined to `sys/` which locally allows it, sets `unsafe_op_in_unsafe_fn`, and carries a `// SAFETY:` justification on every unsafe block behind a safe wrapper. The protocol and handler code is unsafe-free.
- No panics on the request path: `clippy::unwrap_used`, `clippy::expect_used`, `clippy::panic`, `clippy::indexing_slicing` denied on production code. Typed errors via `thiserror`; fork-correctness fails closed, never aborts. PID-1 must not crash.
- RAII/`Drop` for fds, child processes, and mounts so the goroutine/channel/fd leak class found in the Go #24 review is structurally impossible. Ownership-first concurrency: channels and ownership transfer over `Arc<Mutex<>>`; bounded channels; structured task lifetimes.
- CI gates, deny not warn: `cargo fmt --check`, `clippy::all` plus a curated `clippy::pedantic` set denied, `cargo deny` (licenses plus RUSTSEC advisories), `cargo audit`, and `miri` over the `sys` module's testable unsafe. `#![warn(missing_docs)]` on public items.
- Observability via `tracing`; a hard, test-enforced rule that secret values, entropy bytes, argv, and file contents are never logged, only keys and counts.

### No-regression gates (the spine)

1. Contract gate: both agents serve identical `sandbox.v1` from the same proto. A new agent-conformance harness runs the same RPC suite against the Go and Rust agents; cutover requires the Rust agent green on every RPC with byte-identical proto behavior.
2. Performance gate: the bare-metal bench on box1 AND box2 must show the fork-exec win holds and NO metric regresses (exec-rt, fork-exec, RunCode latency, file read/write throughput, per-VM RSS, binary size). Numbers reproducible from `bench/`, per operating principle 1.
3. Fork-correctness gate: the existing fork-correctness CI suite must pass against the Rust agent (credited reseed, clock, signal, network, volumes). CLAUDE.md already requires this green before tenant traffic.
4. Security gate: named human reviewer for `guest/agent-rs`; threat-model and `docs/fork-correctness.md` updated in the graduation PR.

### Cutover strategy

Opt-in, reversible, no big-bang. The rootfs bake already makes `/init` swappable per template (`hack/rust-agent-rootfs.md`):

1. Land the Rust agent behind a per-pool/template selector choosing which `/init` binary is baked.
2. Soak in CI conformance plus a canary pool on box2's k3s cluster.
3. Flip the default only after all four gates are green across a soak window.
4. Rollback is reverting the template selection; the Go agent remains buildable and shippable throughout.

### Testing

- Unit tests per service module (real behavior, not mocks).
- Integration tests over a real vsock/unix transport driving the tonic service.
- Property tests (`proptest`) for protocol round-trips where valuable.
- `miri` over `sys/` unsafe.
- The agent-conformance harness (gate 1) is the cross-agent regression net.

### Risks

- RunCode kernel bridge is the largest single port (stateful ipykernel subprocess); mitigated by treating it as its own module with the Go `kernel.go` as the line-by-line reference and a dedicated conformance leg.
- Async runtime grows the binary; mitigated by measuring the real size as a gate and keeping dependencies minimal.
- Network/volume fork path is security-sensitive; mitigated by the fork-correctness gate and named human review.

---

## SP2: fork-engine measurement spike

### Scope

A measurement-only prototype, behind the `internal/fork` engine interface, of the two pieces with plausible Rust headroom:

- the UFFD page-fault handler (`internal/fork/uffd*`): per-page-fault servicing in a tight loop.
- the snapshot restore path.

### Method

Mirror #310: build the Rust prototype behind the engine seam, benchmark on box1 and box2 against the Go engine (page-fault service latency, fork to first exec contribution, restore wall time), archive JSON under `bench/`, and write a recorded decision (keep Go, or rewrite a named piece, with the numbers).

### Gate

No engine rewrite is planned or scheduled until SP2 produces numbers. Restore is largely Firecracker/kernel bound, so the headroom is genuinely unknown; the spike exists to find out. SP3 (a Rust forkcore) is only written if SP2 shows broad engine headroom.

---

## Sequencing

1. SP1 guest agent: build to full parity, pass all four gates, opt-in cutover, then default. This delivers the data-justified win.
2. SP2 engine spike: run in parallel or after SP1; produces a decision, not necessarily a rewrite.
3. SP3 (conditional): only if SP2 justifies it.

## Security and docs obligations (every graduation PR)

- Tests in the same PR; docs updated in the same PR; threat-model delta if the security surface moved; a benchmark run if the hot path was touched.
- Named human reviewer for `guest/agent-rs` and any future engine Rust code.
- The fork-correctness suite and failure/GC semantics green in CI before any of this ships to production tenants.
