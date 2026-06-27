# Serving Workload (#460) Implementation Plan and Status

**Goal:** Let a pool template declare a long-running workload (command + HTTP ready probe) that the build starts as a persistent process, waits until it is listening, then snapshots, so a fork wakes with the app already serving on its port. This makes Run with Mitos serve any app at `<label>.mitos.run` with instant forks (capture-running, not start-after-fork).

**Root cause this addresses (established by debugging #340/#460 on a live node):**
1. The guest agent runs each exec in its own process group and kills the group on completion (`guest/agent-rs/src/service/exec.rs`), so a server started in an `init` step dies when init returns, before the snapshot.
2. `setsid` is missing from typical images, so the usual escape silently fails.
3. The fork `NotifyForked` handshake broadcasts SIGUSR2 (default-terminate), killing any served app that does not handle it.

**Architecture:** A host-trusted `StartWorkload` RPC on the guest `Control` service spawns the workload in its OWN session (setsid in a pre-exec hook), so it escapes the exec process-group kill; registers the workload's session id so the SIGUSR2 broadcast excludes it; and blocks on an HTTP ready probe before returning. The build calls it AFTER init and BEFORE pause+snapshot, so the snapshot captures a listening workload. Capture-running gives instant forks.

## Tasks and status

- [x] **Task 1: agent SIGUSR2 session exclusion** (`guest/agent-rs/src/sys/signal.rs`): `select_targets(proc, exclude_sids)` + `read_session`; `signal_userspace` takes the excluded set. Unit-tested (`excludes_pids_in_an_excluded_session`). Commit d476d8e.
- [x] **Task 2: workload supervisor** (`guest/agent-rs/src/service/workload.rs`): `WorkloadRegistry`, `spawn_detached` (setsid), `await_http_ready`. Unit-tested. Commit bcb1a9c.
- [x] **Task 3 + 4: proto + agent serves StartWorkload** (`proto/sandbox/controlv1/internal.proto` Control.StartWorkload; `proto/forkd.proto` CreateTemplateRequest.WorkloadSpec; agent `control.rs` serves it and threads the registry into NotifyForked). Commit 9ecc8d9.
- [x] **Task 5: api/v1 Workload field** (`api/v1/sandboxpool_types.go` `WorkloadSpec` + `HTTPReadyProbe` + CRD regen). Commit 49f6c11.
- [x] **Task 6: runmanifest mapping** (`internal/runmanifest/plan.go` GoldenPool maps `run.command` + `run.ready.http` to `Template.Workload`, gated on a declared ready probe). Unit-tested. Commit c04ba46.
- [x] **Task 8: controller plumbing** (`internal/controller/sandboxpool_controller.go` `forkdWorkload` maps the pool workload into the forkd CreateTemplate request). Unit-tested. Commit 3cf9e62.
- [x] **Task 7: host build step** (`internal/firecracker/template.go` `startWorkloadGRPC` calls the guest Control StartWorkload after init, before pause; threaded through `internal/fork/engine.go`, `internal/daemon/grpc_service.go`, the `ForkEngine` interface, and the mock engine). A workload that never becomes ready fails the build. Commit 34ae36c.
- [x] **Task 9 (docs):** `docs/fork-correctness.md` (SIGUSR2 row: workload-session exclusion) and `docs/threat-model.md` (StartWorkload host-trusted, not tenant-reachable).

## Verification

- Go: `api/v1`, `internal/runmanifest`, `internal/firecracker`, `internal/fork`, `internal/daemon`, `internal/controller` all build, vet, and test green (envtest for the controller).
- Rust (docker, Linux): `signal`, `workload`, and `control` unit tests pass; `clippy -D warnings` clean. The 14 failing lib tests are pre-existing privileged netlink/mount/netns/kernel-driver cases unrelated to this change.
- KVM-only (the real build + fork): the `firecracker-test` CI job. Remaining follow-ups: a KVM assertion that a forked workload pool answers its port, and a `docs/recipes/` example using `run.command` + `run.ready.http`. The Run with Mitos e2e on a real node is blocked separately on box2 rehab (cert/mTLS/CAS-GC) and the disk-robustness issues (#463/#464/#465).
