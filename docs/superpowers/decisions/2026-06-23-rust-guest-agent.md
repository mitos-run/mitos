# Decision: Rust guest-agent spike (issue #310)

Status: recorded, conditional. Date: 2026-06-23. Hardware: mitos-bench1 (bare metal).

## Context

Issue #310 proposed a benchmark-driven spike: behind the existing vsock interface,
implement the guest agent in Rust and measure it against the Go agent, then record
a keep-Go-or-adopt-Rust decision with numbers. The thesis was that the latency-
and memory-critical paths (fork engine, guest agent, runtime data path) are where
Go's GC and goroutine overhead cost most, and that the #24 Connect-protocol
concurrency bugs (a PortForward defer-order deadlock, an Upload reader-goroutine
leak) are the class Rust's ownership model prevents.

The spike built `guest/agent-rs/`, a drop-in `/init` replacement speaking the same
vsock JSON protocol (host unchanged), implementing the bench-exercised subset
(ping, exec, exec_stream, read_file, write_file, list_dir, mkdir, remove,
configure, notify_forked). Full numbers: `bench/results/2026-06-23-rust-agent-comparison.md`.

## Pre-registered threshold (from the plan)

"Adopt only if the Rust agent shows a material per-VM RSS reduction OR a p99
exec-rt improvement beyond measurement noise; otherwise keep Go."

## Measured results

- exec round-trip p50: indistinguishable (Go 0.602 to 0.720 ms, Rust 0.595 to 0.611 ms; distributions overlap across 3 runs). p99: no improvement beyond noise; Rust's spread is tighter.
- per-VM RSS at a 256 MiB guest: 65,236 KB (Go) vs 64,916 KB (Rust), within 0.5 percent.
- fork to first exec p50: 113.2 to 114.4 ms (Go) vs 98.9 to 100.6 ms (Rust), a reproducible ~12 percent improvement across 3 runs with no overlap. NOT a pre-registered metric.
- binary size: 4.97 MB (Go) vs 637 KB (Rust), ~7.6x smaller.

## Decision

By the pre-registered threshold, KEEP GO: neither a material per-VM RSS reduction
nor a p99 exec-rt improvement beyond noise was observed. We hold to the
pre-registration rather than move the goalposts to the metric that happened to win.

HOWEVER, a non-preregistered, reproduced signal emerged that we record honestly
rather than discard: fork to first exec, which is on the product's headline claim
path, is ~12 percent faster with the Rust agent, stable across three runs. Paired
with the 7.6x smaller static binary and the memory-safety argument on a
security-sensitive PID-1 process, this is enough to justify a SCOPED FOLLOW-UP,
not an adoption today and not a rewrite.

Explicitly NOT justified by these numbers:

- Rewriting the exec/file runtime data path in Rust: no exec-rt median win.
- A Rust fork engine (`internal/fork`): the fork/restore path is identical for both templates here, so this spike says nothing in its favor; that remains a separate, unstarted question.
- Any control-plane rewrite: out of scope and unsupported (see "Control plane" below).

## Scoped follow-up (gates before any adopt call)

1. Reproduce the fork-to-first-exec win on box2 and with more samples, to rule out a single-host artifact.
2. Investigate WHY fork-to-first-exec is faster (resumed-agent first-exec handling: Go runtime/scheduler wake-up after restore vs the lean static binary). Confirm the cause before claiming it.
3. Complete protocol parity (run_code, pty, tunnel, vitals, tar/untar) so a real adopt is apples-to-apples.
4. Production fork-correctness: the spike reseed does an uncredited plain `/dev/urandom` write; a production Rust agent MUST use credited `RNDADDENTROPY` and fail closed, exactly as the Go agent does (`guest/agent/notifyforked.go`). The spike already injects the host-supplied `req.entropy` and returns false on empty entropy, matching Go's contract.
5. If adopted: dual toolchain in CI, a named human reviewer for `guest/agent-rs` (security-sensitive path), and a threat-model + fork-correctness doc update in the same PR.

## Consequences

- The spike crate stays in-tree behind its interface as the basis for the follow-up; the Go agent remains the shipping agent.
- No security surface changed by this spike (no adoption), so `docs/threat-model.md` and `docs/fork-correctness.md` are unchanged by it; a graduation PR would update both.
