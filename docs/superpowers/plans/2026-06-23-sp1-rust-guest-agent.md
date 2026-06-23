# SP1: Rust guest agent (full sandbox.v1 gRPC parity) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the existing JSON-lines spike crate at `guest/agent-rs/` with a production Rust guest agent that serves all 15 sandbox.v1 gRPC RPCs over tokio-vsock, implements the 5 fork-correctness actions on the notify-forked path, and acts as PID-1 init: a binary that is measurably faster on fork-to-first-exec, meaningfully smaller, and memory-safe, with zero regression on any RPC, latency metric, fork-correctness property, or security invariant relative to the Go agent at `guest/agent/`.

**Architecture:** Crate `guest/agent-rs/` (evolved from the #310 spike). The spike's JSON-lines protocol, transport.rs, protocol.rs, and handlers.rs are REPLACED entirely; init.rs and the build toolchain carry over. The new crate compiles `proto/sandbox/v1/sandbox.proto` with `tonic-build` in `build.rs` so the proto contract is a single source of truth shared with the Go side. The tonic service runs on a `tokio-vsock` listener at `vsock.AgentGRPCPort` (53, same port as the Go gRPC server in `grpc_server.go`). Module layout follows the spec's module boundaries: `service/` (one sub-module per RPC group), `fork/` (notify-forked correctness), `kernel/` (RunCode ipykernel bridge), `sys/` (all unsafe), `init/` (PID-1 bring-up). The self-service RPCs (Fork, Checkpoint, ExtendLifetime, Budget) are served as `Unimplemented` stubs, matching the Go agent's `UnimplementedSandboxServer` embed for those methods.

**Tech Stack:** Latest stable Rust (1.96+), edition 2024, pinned via `rust-toolchain.toml`. tonic 0.12, tokio 1 (full features), tokio-vsock 0.5, prost 0.13, thiserror 1, tracing + tracing-subscriber, notify 6 (inotify watcher for Watch RPC), bytes 1. Build: tonic-build in `build.rs` consuming `proto/sandbox/v1/sandbox.proto`. Static `x86_64-unknown-linux-musl` binary via `hack/build-rust-agent.sh`.

## Global Constraints

- Latest stable Rust pinned via `rust-toolchain.toml` (set `channel = "stable"`); edition 2024; committed `Cargo.lock`; static `x86_64-unknown-linux-musl` build via `hack/build-rust-agent.sh`.
- Stack: tonic + tokio + tokio-vsock; proto compiled with tonic-build from `proto/sandbox/v1/sandbox.proto` (single source of truth, no drift).
- `#![deny(unsafe_code)]` crate-wide; unsafe ONLY in `sys/` which locally `#![allow(unsafe_code)]`, sets `#![deny(unsafe_op_in_unsafe_fn)]`, and carries a `// SAFETY:` comment on every unsafe block behind a safe wrapper (AF_VSOCK, RNDADDENTROPY ioctl, clock_settime, sethostname, mount).
- No panics on the request path: `#![deny(clippy::unwrap_used, clippy::expect_used, clippy::panic, clippy::indexing_slicing)]` on production code (test code excluded via `#[cfg(test)]` scoping); typed errors via `thiserror`; fork-correctness fails closed, never aborts. PID-1 must not crash.
- RAII/Drop for fds, child processes, mounts. Ownership-first concurrency: `tokio::sync::mpsc` channels and ownership transfer over `Arc<Mutex<>>`; bounded channels; structured task lifetimes (`tokio::task::JoinSet`).
- CI gates deny not warn: `cargo fmt --check`, `clippy::all` + curated `clippy::pedantic` denied, `cargo deny` (licenses + RUSTSEC advisories), `cargo audit`, `miri` over `sys/` unsafe. `#![warn(missing_docs)]` on public items.
- `tracing` for logs; secret values, entropy bytes, argv, file contents NEVER logged (only keys/counts). A test enforces this invariant (Task 1.4).
- Punctuation: NO em dashes (U+2014) or en dashes (U+2013) anywhere; only `.` `,` `;` `:` and ASCII hyphen-minus. DCO `Signed-off-by` on every commit (`git commit -s`). Conventional commits. Stage explicit paths only.

---

## File Structure

```
guest/agent-rs/
  rust-toolchain.toml          # pins latest stable; edition 2024
  Cargo.toml                   # replaces spike manifest; adds tonic/tokio/tokio-vsock
  Cargo.lock                   # committed
  build.rs                     # tonic-build compiles proto/sandbox/v1/sandbox.proto
  deny.toml                    # cargo-deny: license + advisory config
  src/
    lib.rs                     # crate root; #![deny] attrs; re-exports service modules
    main.rs                    # PID-1 guard + tonic server startup over vsock
    error.rs                   # AgentError (thiserror); maps to tonic::Status
    env.rs                     # ConfiguredEnv: RwLock<HashMap>; merge() matching guestenv.Merge
    sys/
      mod.rs                   # #![allow(unsafe_code)]; safe wrappers around unsafe syscalls
      vsock.rs                 # AF_VSOCK socket fd wrapped as tokio-vsock or raw accept
      entropy.rs               # reseed_crng(): RNDADDENTROPY ioctl, fail-closed
      clock.rs                 # step_clock(): clock_gettime/clock_settime safe wrappers
      signal.rs                # kill() / signal_userspace() safe wrappers
      mount.rs                 # mount() / sethostname() / isMounted() safe wrappers
    init/
      mod.rs                   # init_system(): proc/sys/dev/tmp/run mounts + /workspace + hostname
    service/
      mod.rs                   # registers all groups on tonic Server; SandboxService struct
      exec.rs                  # Exec RPC: streaming exec + PTY path
      files.rs                 # ReadFile, WriteFile, List, Stat, Mkdir, Remove
      archive.rs               # Archive (DOWNLOAD only), Upload (untar)
      watch.rs                 # Watch: inotify via notify crate
      processes.rs             # Processes + Signal RPCs
      portforward.rs           # PortForward: bidirectional TCP splice
      vitals.rs                # Vitals: streaming /proc sampler
      runcode.rs               # RunCode: ipykernel bridge (delegates to kernel/)
    fork/
      mod.rs                   # handle_notify_forked(); orchestrates sub-actions
      reseed.rs                # reseed_crng() via sys/entropy; fail-closed
      clock.rs                 # step_clock() via sys/clock
      network.rs               # configure_network() via rtnetlink
      volumes.rs               # mount_volumes() via sys/mount
      signal.rs                # signal_userspace() via sys/signal
    kernel/
      mod.rs                   # KernelManager: lazy ipykernel subprocess, serialized by Mutex
      driver.rs                # JSON line protocol to kernel_driver.py
  tests/
    conformance.rs             # integration: tonic client over unix socket drives all 15 RPCs
    secret_log_audit.rs        # property: tracing output never contains secret values
```

Proto-generated code lands in `OUT_DIR/sandbox.v1.rs` (accessed via `tonic::include_proto!("sandbox.v1")`).

---

## Phase 0: Toolchain, manifest, lints, tonic-build scaffolding

### Task 0.1: Replace spike scaffolding with production toolchain and Cargo manifest

**Files:**
- Modify: `guest/agent-rs/Cargo.toml`
- Create: `guest/agent-rs/rust-toolchain.toml`
- Create: `guest/agent-rs/build.rs`
- Create: `guest/agent-rs/deny.toml`
- Modify: `guest/agent-rs/src/lib.rs`
- Modify: `guest/agent-rs/src/main.rs`

**Interfaces:**
- Consumes: `proto/sandbox/v1/sandbox.proto` (the exact file at `feat+api-v1-consolidation` worktree; the Rust worktree must have it at the same relative path or the build.rs path must be adjusted to point to it).
- Produces: a buildable crate with `tonic::include_proto!("sandbox.v1")` accessible; `cargo clippy -- -D warnings` clean; `cargo fmt --check` clean; `cargo deny check` clean.

- [ ] **Step 1: Write the failing build check.**

Create `guest/agent-rs/rust-toolchain.toml`:
```toml
[toolchain]
channel = "stable"
targets = ["x86_64-unknown-linux-musl"]
components = ["rustfmt", "clippy"]
```

Create `guest/agent-rs/build.rs`:
```rust
fn main() -> Result<(), Box<dyn std::error::Error>> {
    // Compile proto/sandbox/v1/sandbox.proto using tonic-build. The path is
    // relative to the workspace root so both the Go and Rust sides consume the
    // same file. tonic-build generates server stubs, client stubs, and prost
    // message types. Rerun when the proto changes.
    tonic_build::configure()
        .build_server(true)
        .build_client(true)
        .compile_protos(
            &["../../proto/sandbox/v1/sandbox.proto"],
            &["../../proto"],
        )?;
    println!("cargo:rerun-if-changed=../../proto/sandbox/v1/sandbox.proto");
    Ok(())
}
```

Overwrite `guest/agent-rs/Cargo.toml` (replaces the spike's edition 2021 / serde-only manifest):
```toml
[package]
name = "sandbox-agent"
version = "0.0.0"
edition = "2024"
publish = false

[lib]
name = "sandbox_agent"
path = "src/lib.rs"

[[bin]]
name = "sandbox-agent"
path = "src/main.rs"

[dependencies]
tonic          = { version = "0.12", features = ["transport"] }
tokio          = { version = "1",    features = ["full"] }
tokio-vsock    = "0.5"
prost          = "0.13"
thiserror      = "1"
tracing        = "0.1"
tracing-subscriber = { version = "0.3", features = ["env-filter"] }
notify         = "6"
bytes          = "1"
nix            = { version = "0.29", features = ["signal", "process", "fs", "mount"] }
libc           = "0.2"

[build-dependencies]
tonic-build = "0.12"

[dev-dependencies]
proptest = "1"
tokio    = { version = "1", features = ["full", "test-util"] }

[profile.release]
opt-level     = "z"
lto           = true
codegen-units = 1
panic         = "abort"
strip         = true
```

Overwrite `guest/agent-rs/src/lib.rs`:
```rust
//! Rust guest agent: full sandbox.v1 gRPC parity.
//!
//! The public API surface is intentionally minimal: only service/ and fork/ are
//! re-exported for integration tests. All unsafe code lives in sys/ and is not
//! re-exported.
#![deny(unsafe_code)]
#![deny(clippy::unwrap_used)]
#![deny(clippy::expect_used)]
#![deny(clippy::panic)]
#![deny(clippy::indexing_slicing)]
#![warn(missing_docs)]

pub mod error;
pub mod env;
pub mod init;
pub mod service;
pub mod fork;
pub mod kernel;
pub(crate) mod sys;
```

- [ ] **Step 2: Run the build to verify it fails (expected: tonic-build cannot find proto file).**

```bash
cd guest/agent-rs && cargo build 2>&1 | head -30
```

Expected: compile error from `build.rs` about the proto path, or a linker error if the path is found. The goal is to confirm the scaffold compiles and only the proto path needs adjustment.

- [ ] **Step 3: Verify the proto path is reachable from the worktree and fix build.rs if needed.**

```bash
ls ../../proto/sandbox/v1/sandbox.proto   # run from guest/agent-rs/
```

If the path differs (the Rust worktree may not have the proto file yet because it was added in `feat+api-v1-consolidation`), copy or symlink the proto into the Rust worktree:
```bash
mkdir -p proto/sandbox/v1
cp ../../.claude/worktrees/feat+api-v1-consolidation/proto/sandbox/v1/sandbox.proto \
   proto/sandbox/v1/sandbox.proto
```

Then update `build.rs` to use `proto/sandbox/v1/sandbox.proto` relative to `CARGO_MANIFEST_DIR`.

- [ ] **Step 4: Create stub modules so the crate compiles.**

Create minimal `src/error.rs`, `src/env.rs`, `src/init/mod.rs`, `src/service/mod.rs`, `src/fork/mod.rs`, `src/kernel/mod.rs`, `src/sys/mod.rs`, each containing only a `// TODO: implement` comment and one public re-export to satisfy `lib.rs`. Create `src/main.rs`:

```rust
fn main() {
    eprintln!("sandbox-agent: not yet wired");
}
```

- [ ] **Step 5: Run `cargo build` and confirm it succeeds.**

```bash
cd guest/agent-rs && cargo build 2>&1
```

Expected: PASS with warnings about unused code (not errors). The generated proto file is present in `target/`.

- [ ] **Step 6: Create `deny.toml` and run `cargo deny check`.**

```toml
[licenses]
allow = ["MIT", "Apache-2.0", "Apache-2.0 WITH LLVM-exception", "BSD-2-Clause", "BSD-3-Clause", "ISC", "Unicode-3.0"]
deny  = []

[advisories]
db-path   = "~/.cargo/advisory-db"
db-urls   = ["https://github.com/rustsec/advisory-db"]
ignore    = []
```

```bash
cd guest/agent-rs && cargo deny check licenses 2>&1 || echo "install cargo-deny first: cargo install cargo-deny"
```

- [ ] **Step 7: Commit.**

```bash
git add guest/agent-rs/rust-toolchain.toml guest/agent-rs/Cargo.toml guest/agent-rs/Cargo.lock \
        guest/agent-rs/build.rs guest/agent-rs/deny.toml \
        guest/agent-rs/src/lib.rs guest/agent-rs/src/main.rs \
        guest/agent-rs/src/error.rs guest/agent-rs/src/env.rs \
        guest/agent-rs/src/init/mod.rs guest/agent-rs/src/service/mod.rs \
        guest/agent-rs/src/fork/mod.rs guest/agent-rs/src/kernel/mod.rs \
        guest/agent-rs/src/sys/mod.rs
git commit -s -m "feat(agent-rs): replace spike scaffold with tonic/tokio/edition-2024 manifest (#310)"
```

---

## Phase 1: gRPC service skeleton over tokio-vsock

### Task 1.1: error.rs and env.rs (shared primitives)

**Files:**
- Modify: `guest/agent-rs/src/error.rs`
- Modify: `guest/agent-rs/src/env.rs`
- Test: inline `#[cfg(test)]` in `env.rs`

**Interfaces:**
- Produces:
  ```rust
  // error.rs
  #[derive(Debug, thiserror::Error)]
  pub enum AgentError {
      #[error("io: {0}")] Io(#[from] std::io::Error),
      #[error("spawn: {0}")] Spawn(String),
      #[error("path outside workspace allowlist: {0}")] PathDenied(String),
      #[error("unsupported language: {0}")] UnsupportedLanguage(String),
      #[error("kernel unavailable: {0}")] KernelUnavailable(String),
  }
  impl From<AgentError> for tonic::Status { ... }  // maps each variant to the right gRPC code

  // env.rs
  pub struct ConfiguredEnv(tokio::sync::RwLock<std::collections::HashMap<String,String>>);
  impl ConfiguredEnv {
      pub fn new() -> Self;
      // Additive merge matching guestenv.Merge from Go (guest/agent/main.go:281-296).
      // Logs count only, never values.
      pub async fn merge(&self, env: &[(String,String)], secrets: &[(String,String)]);
      // Snapshot for exec: take a consistent clone of the current map.
      pub async fn snapshot(&self) -> std::collections::HashMap<String,String>;
  }
  ```

- [ ] **Step 1: Write the failing test.**

In `src/env.rs`:
```rust
#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn merge_is_additive_and_last_wins() {
        let e = ConfiguredEnv::new();
        e.merge(&[("K".into(), "v1".into())], &[]).await;
        e.merge(&[("K".into(), "v2".into())], &[]).await;
        let snap = e.snapshot().await;
        assert_eq!(snap.get("K").map(String::as_str), Some("v2"));
    }

    #[tokio::test]
    async fn secrets_merged_same_as_env() {
        let e = ConfiguredEnv::new();
        e.merge(&[], &[("TOKEN".into(), "secret".into())]).await;
        let snap = e.snapshot().await;
        assert_eq!(snap.get("TOKEN").map(String::as_str), Some("secret"));
        // the snapshot value is present but is never logged: no tracing call
        // emits it (enforced by Task 1.4 audit test).
    }
}
```

Run: `cd guest/agent-rs && cargo test env` -> Expected: FAIL (types not defined).

- [ ] **Step 2: Implement `error.rs`.**

```rust
use tonic::{Code, Status};

#[derive(Debug, thiserror::Error)]
pub enum AgentError {
    #[error("io: {0}")]
    Io(#[from] std::io::Error),
    #[error("spawn: {0}")]
    Spawn(String),
    #[error("path outside workspace allowlist: {0}")]
    PathDenied(String),
    #[error("unsupported language: {0}")]
    UnsupportedLanguage(String),
    #[error("kernel unavailable: {0}")]
    KernelUnavailable(String),
    #[error("invalid argument: {0}")]
    InvalidArgument(String),
}

impl From<AgentError> for Status {
    fn from(e: AgentError) -> Self {
        match e {
            AgentError::PathDenied(m)         => Status::new(Code::PermissionDenied, m),
            AgentError::InvalidArgument(m)    => Status::new(Code::InvalidArgument, m),
            AgentError::UnsupportedLanguage(m)|
            AgentError::KernelUnavailable(m)  => Status::new(Code::FailedPrecondition, m),
            AgentError::Io(e)                 => Status::new(Code::Internal, e.to_string()),
            AgentError::Spawn(m)              => Status::new(Code::Internal, m),
        }
    }
}
```

- [ ] **Step 3: Implement `env.rs` (mirrors `configuredEnv`/`configuredMu` in `guest/agent/main.go:48-51` and `handleConfigure` at `main.go:280-296`).**

```rust
use std::collections::HashMap;
use tokio::sync::RwLock;
use tracing::info;

pub struct ConfiguredEnv(RwLock<HashMap<String, String>>);

impl ConfiguredEnv {
    pub fn new() -> Self {
        Self(RwLock::new(HashMap::new()))
    }

    /// Additive merge. Logs count only, never key names or values.
    /// Mirrors guest/agent/main.go handleConfigure (lines 280-296):
    /// both env and secrets are inserted into the same map; later calls
    /// overwrite existing keys but never remove them.
    pub async fn merge(&self, env: &[(String, String)], secrets: &[(String, String)]) {
        let mut guard = self.0.write().await;
        for (k, v) in env.iter().chain(secrets.iter()) {
            guard.insert(k.clone(), v.clone());
        }
        info!(configured_count = guard.len(), "env merged");
        // values are intentionally not logged
    }

    pub async fn snapshot(&self) -> HashMap<String, String> {
        self.0.read().await.clone()
    }
}
```

- [ ] **Step 4: Run test and confirm PASS.**

```bash
cd guest/agent-rs && cargo test env
```

- [ ] **Step 5: Commit.**

```bash
git add guest/agent-rs/src/error.rs guest/agent-rs/src/env.rs
git commit -s -m "feat(agent-rs): AgentError and ConfiguredEnv shared primitives (#310)"
```

---

### Task 1.2: sys/ module (all unsafe behind safe wrappers)

**Files:**
- Modify: `guest/agent-rs/src/sys/mod.rs`
- Create: `guest/agent-rs/src/sys/entropy.rs`
- Create: `guest/agent-rs/src/sys/clock.rs`
- Create: `guest/agent-rs/src/sys/signal.rs`
- Create: `guest/agent-rs/src/sys/mount.rs`
- Test: inline `#[cfg(test)]` in each file; `miri` target for entropy.rs

**Interfaces:**
- Produces:
  ```rust
  // sys/entropy.rs
  /// Credits entropy bytes to the kernel CRNG via RNDADDENTROPY ioctl.
  /// Returns true ONLY when the credited ioctl succeeds. Fail-closed: returns
  /// false on any error rather than falling back to an uncredited write.
  /// Mirrors reseedCRNGAt in guest/agent/notifyforked.go:90-119.
  pub fn reseed_crng(entropy: &[u8]) -> bool;

  // sys/clock.rs
  /// Reads CLOCK_REALTIME in nanoseconds.
  pub fn get_realtime_nanos() -> Result<i64, std::io::Error>;
  /// Steps CLOCK_REALTIME to target_nanos. Requires CAP_SYS_TIME.
  pub fn set_realtime_nanos(nanos: i64) -> Result<(), std::io::Error>;

  // sys/signal.rs
  /// Sends signal to a single pid. Validates pid > 1 and signal in 1..=64.
  pub fn kill(pid: i32, signal: i32) -> Result<(), nix::errno::Errno>;
  /// Sends SIGUSR2 to all userspace processes except pid 1 and self.
  /// Returns count of successful signals.
  /// Mirrors signalUserspace in guest/agent/notifyforked.go:299-328.
  pub fn signal_userspace() -> i32;

  // sys/mount.rs
  /// Mounts source at target with fstype. Mirrors unix.Mount in notifyforked.go:266.
  pub fn mount(source: &str, target: &str, fstype: &str, flags: u64) -> Result<(), std::io::Error>;
  /// sethostname syscall. Mirrors syscall.Sethostname in main.go:124.
  pub fn sethostname(name: &str) -> Result<(), std::io::Error>;
  /// Checks /proc/mounts for mountPath. Mirrors isMounted in notifyforked.go:283-296.
  pub fn is_mounted(mount_path: &str) -> bool;
  ```

- [ ] **Step 1: Write the failing tests for entropy.rs.**

In `sys/entropy.rs`, inside `#[cfg(test)]`:
```rust
#[test]
fn empty_entropy_returns_false() {
    // Mirror Go: reseedCRNG(nil) returns false immediately.
    // notifyforked.go:92: "if len(entropy) == 0 { return false }"
    assert!(!super::reseed_crng(&[]));
}

#[test]
#[cfg(target_os = "linux")]
fn nonzero_entropy_attempts_ioctl() {
    // On a Linux host with /dev/urandom accessible (which is always true in CI),
    // opening the device must succeed. Whether RNDADDENTROPY is credited depends
    // on CAP_SYS_ADMIN; in the Firecracker VM we always have it. In CI without
    // the capability the ioctl returns EPERM and reseed_crng returns false (fail
    // closed). The test asserts the function returns a bool without panicking,
    // not that it returns true.
    let _ = super::reseed_crng(b"test-entropy-bytes-not-real");
    // If we reach here, the function did not panic. The return value depends on
    // whether CAP_SYS_ADMIN is available; both true and false are valid here.
}
```

Run: `cd guest/agent-rs && cargo test sys` -> Expected: FAIL (module not implemented).

- [ ] **Step 2: Implement `sys/mod.rs`.**

```rust
// sys/mod.rs: the ONLY module in this crate that allows unsafe code.
// Every unsafe block has a // SAFETY: comment and is wrapped in a safe API.
#![allow(unsafe_code)]
#![deny(unsafe_op_in_unsafe_fn)]

pub mod entropy;
pub mod clock;
pub mod signal;
pub mod mount;
```

- [ ] **Step 3: Implement `sys/entropy.rs` (mirrors `reseedCRNGAt` in `guest/agent/notifyforked.go:90-119`).**

The Go reference builds a packed `rand_pool_info` struct: 4 bytes `entropy_count` (bits, little-endian i32), 4 bytes `buf_size` (bytes, little-endian i32), then the entropy bytes. It calls `unix.Syscall(unix.SYS_IOCTL, f.Fd(), uintptr(unix.RNDADDENTROPY), uintptr(unsafe.Pointer(&buf[0])))`. Replicate this exactly:

```rust
use std::os::fd::AsRawFd;

const RNDADDENTROPY: libc::c_ulong = 0x4008_5203; // same on amd64 and arm64

pub fn reseed_crng(entropy: &[u8]) -> bool {
    if entropy.is_empty() {
        return false;
    }
    reseed_crng_at(entropy, "/dev/urandom")
}

fn reseed_crng_at(entropy: &[u8], path: &str) -> bool {
    // Build rand_pool_info: [entropy_count: i32 LE][buf_size: i32 LE][entropy bytes]
    // Mirrors notifyforked.go:96-99.
    let entropy_bits = (entropy.len() * 8) as u32;
    let buf_size = entropy.len() as u32;
    let mut buf = Vec::with_capacity(8 + entropy.len());
    buf.extend_from_slice(&entropy_bits.to_le_bytes());
    buf.extend_from_slice(&buf_size.to_le_bytes());
    buf.extend_from_slice(entropy);

    let f = match std::fs::OpenOptions::new().read(true).write(true).open(path) {
        Ok(f) => f,
        Err(e) => {
            tracing::error!("open {}: {}", path, e);
            return false;
        }
    };

    // SAFETY: buf is valid for its length; the ioctl takes a pointer to a
    // packed rand_pool_info struct which we built above. The fd is open O_RDWR.
    let ret = unsafe {
        libc::ioctl(f.as_raw_fd(), RNDADDENTROPY, buf.as_ptr())
    };
    if ret == 0 {
        true
    } else {
        let errno = std::io::Error::last_os_error();
        tracing::error!(
            "RNDADDENTROPY failed ({}); reseed NOT credited, reporting failure",
            errno
        );
        // Fail closed: mirrors notifyforked.go:117-118.
        false
    }
}
```

- [ ] **Step 4: Implement `sys/clock.rs` (mirrors `stepClock` in `notifyforked.go:138-164`).**

```rust
use std::io;

pub fn get_realtime_nanos() -> io::Result<i64> {
    let mut ts = libc::timespec { tv_sec: 0, tv_nsec: 0 };
    // SAFETY: ts is a valid stack-allocated timespec; CLOCK_REALTIME is a valid id.
    let ret = unsafe { libc::clock_gettime(libc::CLOCK_REALTIME, &mut ts) };
    if ret == 0 {
        Ok(ts.tv_sec as i64 * 1_000_000_000 + ts.tv_nsec as i64)
    } else {
        Err(io::Error::last_os_error())
    }
}

pub fn set_realtime_nanos(nanos: i64) -> io::Result<()> {
    let ts = libc::timespec {
        tv_sec:  (nanos / 1_000_000_000) as libc::time_t,
        tv_nsec: (nanos % 1_000_000_000) as libc::c_long,
    };
    // SAFETY: ts is a valid timespec; CLOCK_REALTIME is the only settable clock.
    // Requires CAP_SYS_TIME; fails with EPERM if not held.
    let ret = unsafe { libc::clock_settime(libc::CLOCK_REALTIME, &ts) };
    if ret == 0 {
        Ok(())
    } else {
        Err(io::Error::last_os_error())
    }
}
```

- [ ] **Step 5: Implement `sys/signal.rs` (mirrors `signalUserspace` and `Signal` in `notifyforked.go:299-328` and `grpc_runtime.go:296-316`).**

```rust
use std::io;

/// Validates and sends a signal. Rejects pid <= 1 and signal outside 1..=64.
/// Mirrors the guard in grpc_runtime.go Signal (lines 296-315).
pub fn kill(pid: i32, signal: i32) -> io::Result<()> {
    if pid <= 1 {
        return Err(io::Error::new(io::ErrorKind::InvalidInput,
            format!("refusing to signal pid {}: pid 1 is the guest control plane", pid)));
    }
    if !(1..=64).contains(&signal) {
        return Err(io::Error::new(io::ErrorKind::InvalidInput,
            format!("signal number {} out of range 1..64", signal)));
    }
    // SAFETY: pid > 1, signal is in valid range, libc::kill is well-defined.
    let ret = unsafe { libc::kill(pid, signal) };
    if ret == 0 {
        Ok(())
    } else {
        Err(io::Error::last_os_error())
    }
}

/// Sends SIGUSR2 to every userspace process except PID 1 and self.
/// Mirrors signalUserspace in notifyforked.go:299-328: reads /proc, skips
/// pid 1 and self, kills each pid with SIGUSR2, returns success count.
pub fn signal_userspace() -> i32 {
    let self_pid = {
        // SAFETY: getpid() is always safe.
        unsafe { libc::getpid() }
    };
    let entries = match std::fs::read_dir("/proc") {
        Ok(e) => e,
        Err(_) => return 0,
    };
    let mut count = 0i32;
    for entry in entries.flatten() {
        let name = entry.file_name();
        let pid: libc::pid_t = match name.to_string_lossy().parse() {
            Ok(p) => p,
            Err(_) => continue,
        };
        if pid == 1 || pid == self_pid {
            continue;
        }
        // SAFETY: pid came from /proc (valid), SIGUSR2 is a valid signal number.
        let ret = unsafe { libc::kill(pid, libc::SIGUSR2) };
        if ret == 0 {
            count += 1;
        }
    }
    count
}
```

- [ ] **Step 6: Implement `sys/mount.rs` (mirrors `unix.Mount` in `notifyforked.go:266`, `isMounted` at `notifyforked.go:283-296`, `syscall.Sethostname` in `main.go:124`).**

```rust
use std::io;
use std::ffi::CString;

pub fn mount(source: &str, target: &str, fstype: &str, flags: u64) -> io::Result<()> {
    let source = CString::new(source).map_err(|e| io::Error::new(io::ErrorKind::InvalidInput, e))?;
    let target = CString::new(target).map_err(|e| io::Error::new(io::ErrorKind::InvalidInput, e))?;
    let fstype = CString::new(fstype).map_err(|e| io::Error::new(io::ErrorKind::InvalidInput, e))?;
    // SAFETY: all CStrings are valid; flags is a bitmask; data is null (no options).
    let ret = unsafe {
        libc::mount(source.as_ptr(), target.as_ptr(), fstype.as_ptr(), flags as libc::c_ulong, std::ptr::null())
    };
    if ret == 0 { Ok(()) } else { Err(io::Error::last_os_error()) }
}

pub fn sethostname(name: &str) -> io::Result<()> {
    let bytes = name.as_bytes();
    // SAFETY: bytes is valid UTF-8, length is its byte length.
    let ret = unsafe { libc::sethostname(bytes.as_ptr() as *const libc::c_char, bytes.len()) };
    if ret == 0 { Ok(()) } else { Err(io::Error::last_os_error()) }
}

/// Checks /proc/mounts for mountPath. Mirrors isMounted in notifyforked.go:283-296:
/// scans field 2 of each line; returns false on any read error so the caller
/// attempts the mount (a redundant mount fails loudly rather than silently skipping).
pub fn is_mounted(mount_path: &str) -> bool {
    use std::io::{BufRead, BufReader};
    let f = match std::fs::File::open("/proc/mounts") {
        Ok(f) => f,
        Err(_) => return false,
    };
    for line in BufReader::new(f).lines().map_while(Result::ok) {
        let mut fields = line.split_whitespace();
        fields.next(); // device
        if fields.next() == Some(mount_path) {
            return true;
        }
    }
    false
}
```

- [ ] **Step 7: Run all sys tests.**

```bash
cd guest/agent-rs && cargo test sys
```

Expected: PASS.

- [ ] **Step 8: Run `miri` over entropy.rs (on a Linux host with miri installed).**

```bash
cd guest/agent-rs && cargo +nightly miri test sys::entropy::empty_entropy_returns_false
```

Expected: PASS with no undefined behavior detected. The ioctl test is excluded from miri since miri cannot call real kernel syscalls.

- [ ] **Step 9: Commit.**

```bash
git add guest/agent-rs/src/sys/mod.rs guest/agent-rs/src/sys/entropy.rs \
        guest/agent-rs/src/sys/clock.rs guest/agent-rs/src/sys/signal.rs \
        guest/agent-rs/src/sys/mount.rs
git commit -s -m "feat(agent-rs): sys/ module with safe wrappers for all unsafe syscalls (#310)"
```

---

### Task 1.3: init/ module and service skeleton over tokio-vsock

**Files:**
- Modify: `guest/agent-rs/src/init/mod.rs`
- Modify: `guest/agent-rs/src/service/mod.rs`
- Modify: `guest/agent-rs/src/main.rs`
- Test: inline test in `init/mod.rs`; integration test `tests/conformance.rs` (skeleton)

**Interfaces:**
- Consumes: `sys::mount::{mount, sethostname}`, tonic-generated `sandbox_v1::sandbox_server::SandboxServer`, `tokio_vsock::VsockListener`.
- Produces:
  ```rust
  // init/mod.rs
  /// Mounts proc, sysfs, devtmpfs, tmpfs(/tmp), tmpfs(/run), creates /workspace,
  /// sets hostname "sandbox". Non-fatal: logs errors and continues.
  /// Mirrors initSystem in guest/agent/main.go:93-127.
  pub fn init_system();

  // service/mod.rs
  pub struct SandboxService {
      pub env: std::sync::Arc<crate::env::ConfiguredEnv>,
      pub kernel: std::sync::Arc<tokio::sync::Mutex<crate::kernel::KernelManager>>,
  }
  impl sandbox_v1::sandbox_server::Sandbox for SandboxService { ... }

  // main.rs: async fn main() - PID-1 guard + tonic gRPC server on vsock port 53
  ```

- [ ] **Step 1: Write the failing init test.**

In `src/init/mod.rs`:
```rust
#[cfg(test)]
mod tests {
    #[test]
    fn mount_table_order_matches_go_agent() {
        // Mirrors mount_table_matches_go_agent in the spike's init.rs.
        // initSystem in guest/agent/main.go lines 95-107 mounts in this order.
        let expected = ["/proc", "/sys", "/dev", "/tmp", "/run"];
        assert_eq!(super::MOUNT_TABLE.iter().map(|m| m.target).collect::<Vec<_>>(), expected);
    }
}
```

Run: `cargo test init` -> Expected: FAIL (MOUNT_TABLE not defined).

- [ ] **Step 2: Implement `init/mod.rs` (mirrors `initSystem` in `guest/agent/main.go:93-127`).**

```rust
use crate::sys::mount::{mount, sethostname};
use tracing::{error, info};

pub struct MountSpec {
    pub source: &'static str,
    pub target: &'static str,
    pub fstype: &'static str,
}

pub const MOUNT_TABLE: &[MountSpec] = &[
    MountSpec { source: "proc",     target: "/proc", fstype: "proc"     },
    MountSpec { source: "sysfs",    target: "/sys",  fstype: "sysfs"    },
    MountSpec { source: "devtmpfs", target: "/dev",  fstype: "devtmpfs" },
    MountSpec { source: "tmpfs",    target: "/tmp",  fstype: "tmpfs"    },
    MountSpec { source: "tmpfs",    target: "/run",  fstype: "tmpfs"    },
];

/// PID-1 init. Non-fatal: logs each failure and continues.
/// Mirrors guest/agent/main.go initSystem (lines 93-127).
pub fn init_system() {
    for m in MOUNT_TABLE {
        if let Err(e) = std::fs::create_dir_all(m.target) {
            error!("mkdir {}: {}", m.target, e);
        }
        if let Err(e) = mount(m.source, m.target, m.fstype, 0) {
            error!("mount {}: {}", m.target, e);
        }
    }
    if let Err(e) = std::fs::create_dir_all("/workspace") {
        error!("mkdir /workspace: {}", e);
    }
    if let Err(e) = sethostname("sandbox") {
        error!("sethostname: {}", e);
    }
    info!("init complete");
}
```

- [ ] **Step 3: Implement `service/mod.rs` skeleton with `UnimplementedSandboxServer` for all RPCs.**

```rust
use std::sync::Arc;
use tokio::sync::Mutex;
use tonic::{Request, Response, Status};

// The proto-generated module. tonic-build emits it into OUT_DIR.
pub mod sandbox_v1 {
    tonic::include_proto!("sandbox.v1");
}

use sandbox_v1::sandbox_server::Sandbox;

pub struct SandboxService {
    pub env: Arc<crate::env::ConfiguredEnv>,
    pub kernel: Arc<Mutex<crate::kernel::KernelManager>>,
}

#[tonic::async_trait]
impl Sandbox for SandboxService {
    // All RPCs return Unimplemented until implemented in Phase 2 tasks.
    // Each task replaces the stub with a real implementation.
    type ExecStream = tonic::codec::Streaming<sandbox_v1::ExecResponse>;
    // ... (all streaming type aliases at Unimplemented)

    async fn exec(&self, _: Request<tonic::Streaming<sandbox_v1::ExecRequest>>)
        -> Result<Response<Self::ExecStream>, Status> {
        Err(Status::unimplemented("exec: not yet implemented"))
    }
    // ... remaining 14 RPCs as Unimplemented stubs
}
```

Note: the exact streaming type aliases for tonic depend on how tonic-build generates the trait. The implementer must match the generated trait signature exactly; the `cargo build` output will show the exact types required.

- [ ] **Step 4: Implement `main.rs` with vsock listener.**

```rust
use std::sync::Arc;
use tokio::sync::Mutex;
use tonic::transport::Server;

// AgentGRPCPort = 53, matches vsock.AgentGRPCPort in internal/vsock/protocol.go.
// The Go agent serves on this port in grpc_server.go startGRPCServer().
const AGENT_GRPC_PORT: u32 = 53;

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    tracing_subscriber::fmt()
        .with_env_filter(tracing_subscriber::EnvFilter::from_default_env())
        .init();

    // SAFETY: getpid() is always safe.
    if unsafe { libc::getpid() } == 1 {
        sandbox_agent::init::init_system();
    }

    let env    = Arc::new(sandbox_agent::env::ConfiguredEnv::new());
    let kernel = Arc::new(Mutex::new(sandbox_agent::kernel::KernelManager::new()));

    let svc = sandbox_agent::service::SandboxService { env, kernel };

    tracing::info!("sandbox-agent: gRPC ready on vsock port {}", AGENT_GRPC_PORT);

    // tokio-vsock provides a VsockListener that implements tokio's Accept trait,
    // which tonic's Server::serve_with_incoming accepts.
    let listener = tokio_vsock::VsockListener::bind(
        tokio_vsock::VMADDR_CID_ANY,
        AGENT_GRPC_PORT,
    )?;

    Server::builder()
        .add_service(sandbox_agent::service::sandbox_v1::sandbox_server::SandboxServer::new(svc))
        .serve_with_incoming(listener)
        .await?;

    Ok(())
}
```

- [ ] **Step 5: Write the skeleton conformance test (one RPC - Stat on an existing path - to prove the server accepts connections).**

In `guest/agent-rs/tests/conformance.rs`:
```rust
// Integration test: spins up the tonic service on a unix domain socket and
// drives it with a generated tonic client. Unix socket is used so tests run
// on the host without vsock.

use sandbox_agent::service::sandbox_v1::{
    sandbox_client::SandboxClient, StatRequest,
};
use std::sync::Arc;
use tokio::sync::Mutex;
use tonic::transport::{Channel, Endpoint};

async fn test_client() -> SandboxClient<Channel> {
    // The test server binds on a fixed unix socket.
    let channel = Endpoint::from_static("http://[::]:0")
        .connect_with_connector(tower::service_fn(|_| {
            tokio::net::UnixStream::connect("/tmp/agent-conformance-test.sock")
        }))
        .await
        .expect("connect to test server");
    SandboxClient::new(channel)
}

#[tokio::test]
async fn stat_root_returns_dir() {
    // Start the service on a unix socket in a background task.
    let env    = Arc::new(sandbox_agent::env::ConfiguredEnv::new());
    let kernel = Arc::new(Mutex::new(sandbox_agent::kernel::KernelManager::new()));
    let svc    = sandbox_agent::service::SandboxService { env, kernel };

    let _ = std::fs::remove_file("/tmp/agent-conformance-test.sock");
    let uds    = tokio::net::UnixListener::bind("/tmp/agent-conformance-test.sock")
        .expect("bind unix socket");
    let incoming = tokio_stream::wrappers::UnixListenerStream::new(uds);

    tokio::spawn(async move {
        tonic::transport::Server::builder()
            .add_service(
                sandbox_agent::service::sandbox_v1::sandbox_server::SandboxServer::new(svc)
            )
            .serve_with_incoming(incoming)
            .await
            .ok();
    });

    tokio::time::sleep(std::time::Duration::from_millis(50)).await;

    let mut client = test_client().await;
    // Stat will return Unimplemented until Phase 2 Task 2.2 implements it.
    // This test just checks the server accepts connections and returns a valid gRPC status.
    let result = client.stat(StatRequest { path: "/".into() }).await;
    // Either Ok (if Stat is implemented) or an error status is acceptable here;
    // what must NOT happen is a connection refused or a panic.
    match result {
        Ok(_) | Err(_) => {} // connection succeeded; gRPC status is any valid code
    }
}
```

Run: `cargo test --test conformance` -> Expected: PASS (the server starts and accepts the connection).

- [ ] **Step 6: Commit.**

```bash
git add guest/agent-rs/src/init/mod.rs guest/agent-rs/src/service/mod.rs \
        guest/agent-rs/src/main.rs guest/agent-rs/tests/conformance.rs
git commit -s -m "feat(agent-rs): init system and tonic gRPC skeleton over vsock (#310)"
```

---

## Phase 2: RPC implementations

### Task 2.1: Exec and PTY (service/exec.rs)

**Files:**
- Create: `guest/agent-rs/src/service/exec.rs`
- Modify: `guest/agent-rs/src/service/mod.rs` (replace Exec stub)
- Test: `tests/conformance.rs` (add Exec test)

**Interfaces:**
- Consumes: `env::ConfiguredEnv::snapshot()`, `sandbox_v1::{ExecRequest, ExecOpen, ExecResponse, ExecExit, PtyOptions, WindowSize}`.
- Produces:
  ```rust
  /// Handles the Exec bidi stream. Mirrors sandboxServer.Exec in grpc_server.go:140-172
  /// and execPTY at grpc_server.go:182-199. Spawns /bin/sh -c <command> in its own
  /// process group (Setpgid); a context cancel kills the whole group. Non-PTY uses
  /// tokio::process::Command with piped stdout/stderr; PTY path uses openpty + slave fd.
  pub async fn exec_handler(
      env: Arc<ConfiguredEnv>,
      stream: tonic::Streaming<sandbox_v1::ExecRequest>,
      tx: tokio::sync::mpsc::Sender<Result<sandbox_v1::ExecResponse, Status>>,
  ) -> Result<(), Status>;
  ```

- [ ] **Step 1: Write the failing conformance test for Exec.**

In `tests/conformance.rs`, add:
```rust
#[tokio::test]
async fn exec_echo_returns_stdout() {
    let mut client = test_client().await;
    let open = sandbox_v1::ExecRequest {
        msg: Some(sandbox_v1::exec_request::Msg::Open(sandbox_v1::ExecOpen {
            command: "printf 'hello'".into(),
            cwd: "/tmp".into(),
            ..Default::default()
        })),
    };
    let (tx, rx) = tokio::sync::mpsc::channel(10);
    tx.send(open).await.unwrap();
    drop(tx); // no more stdin

    let stream = client.exec(tokio_stream::wrappers::ReceiverStream::new(rx)).await.unwrap().into_inner();
    use tokio_stream::StreamExt;
    let mut out = String::new();
    let mut exit_code = -1i32;
    tokio::pin!(stream);
    while let Some(msg) = stream.next().await {
        match msg.unwrap().msg.unwrap() {
            sandbox_v1::exec_response::Msg::Stdout(b) => out.push_str(&String::from_utf8_lossy(&b)),
            sandbox_v1::exec_response::Msg::Exit(e)   => { exit_code = e.exit_code; break; }
            _ => {}
        }
    }
    assert_eq!(out, "hello");
    assert_eq!(exit_code, 0);
}
```

Run: `cargo test --test conformance exec_echo` -> Expected: FAIL (Exec returns Unimplemented).

- [ ] **Step 2: Implement non-PTY exec in `service/exec.rs`.**

Mirror the behavior of `grpc_server.go Exec` (lines 140-172) and `exec_stream.go runExecStream` (lines 76-171):

1. Receive the first message; if it is not `open`, return `InvalidArgument`.
2. If `open.args` is non-empty, return `Unimplemented` (argv exec is deferred, same as grpc_server.go:149-151).
3. If `open.pty` is set, delegate to `exec_pty` (Step 4 below).
4. Spawn `/bin/sh -c open.command` via `tokio::process::Command` with:
   - `current_dir`: `open.cwd` or `"/workspace"` if empty (mirrors `exec_stream.go:88-90`).
   - `env`: merge base OS env + `ConfiguredEnv::snapshot()` + `open.env` (mirrors `exec_stream.go:100-107`). Secret values from env are NEVER logged.
   - `process_group(0)` (Setpgid equivalent, kills whole group on cancel).
   - `timeout_seconds`: `open.timeout_seconds` or 30s default (mirrors `exec_stream.go:78-83`).
5. Pump stdout and stderr as `ExecResponse::Stdout`/`ExecResponse::Stderr` chunks of 32 KiB (mirrors `streamChunkBytes = 32 << 10` in `exec_stream.go:24`).
6. On exit or timeout (exit code 124), send `ExecResponse::Exit { exit_code, exec_time_ms }` (mirrors `exec_stream.go:156-170`).
7. Context cancellation propagates via `tokio::select!` to kill the process group.

Key code for env merge (must match `guestenv.Merge` semantics - base < configured < request, last wins):
```rust
fn merge_env(
    base: impl Iterator<Item=(String,String)>,
    configured: &std::collections::HashMap<String,String>,
    request: &[sandbox_v1::EnvVar],
) -> Vec<(String,String)> {
    let mut map: std::collections::HashMap<String,String> = base.collect();
    for (k,v) in configured { map.insert(k.clone(), v.clone()); }
    for ev in request { map.insert(ev.key.clone(), ev.value.clone()); }
    map.into_iter().collect()
}
```

- [ ] **Step 3: Implement PTY exec in `service/exec.rs`.**

Mirror `grpc_server.go execPTY` (lines 182-199) and `pty.go runPTY` (lines 105-218):

1. Call `openpty()` via `nix::pty::openpty` to get a master/slave pair (replaces Go's `/dev/ptmx` open + `TIOCSPTLCK` + `TIOCGPTN` pattern from `pty.go:29-45`).
2. Apply initial window size via `nix::ioctl_write_ptr!(tiocswinsz, ...)` (mirrors `setWinsize` at `pty.go:49-59`).
3. Spawn the shell with `Setsid + Setctty` and stdin/stdout/stderr wired to the slave fd (mirrors `pty.go:142-147`; the slave fd number is 0 because it is wired to all three standard streams).
4. Launch two tasks: one reads output from the master and sends it as `ExecResponse::Stdout` chunks (PTY merges stdout/stderr, per proto comment); one reads `ExecRequest::Stdin`/`ExecRequest::Resize` from the client stream, writing stdin bytes to the master and applying window resizes via ioctl.
5. On client stream close (EOF on Recv) or context cancel, send `SIGKILL` to the process group (`killpg`), then send `ExecResponse::Exit`.

- [ ] **Step 4: Run conformance tests for Exec.**

```bash
cd guest/agent-rs && cargo test --test conformance exec
```

Expected: PASS for the non-PTY echo test. PTY is harder to test without a real terminal; add a basic PTY test that sends a simple command and waits for the exit frame.

- [ ] **Step 5: Commit.**

```bash
git add guest/agent-rs/src/service/exec.rs guest/agent-rs/src/service/mod.rs \
        guest/agent-rs/tests/conformance.rs
git commit -s -m "feat(agent-rs): Exec and PTY RPC implementation (#310)"
```

---

### Task 2.2: File RPCs (service/files.rs)

**Files:**
- Create: `guest/agent-rs/src/service/files.rs`
- Modify: `guest/agent-rs/src/service/mod.rs` (replace ReadFile/WriteFile/List/Stat/Mkdir/Remove stubs)
- Test: `tests/conformance.rs` (add file RPC tests)

**Interfaces:**
- Consumes: `sandbox_v1::{ReadFileRequest, Chunk, WriteFileRequest, WriteFileResult, ListRequest, ListResponse, FileInfo, StatRequest, MkdirRequest, MkdirResponse, RemoveRequest, RemoveResponse}`.
- Produces: implementations of ReadFile, WriteFile, List, Stat, Mkdir, Remove that mirror the Go implementations in `grpc_server.go:288-405`.

- [ ] **Step 1: Write failing conformance tests for file RPCs.**

```rust
#[tokio::test]
async fn write_then_read_file_roundtrips() {
    let mut client = test_client().await;
    let path = "/tmp/agent-rs-conformance-test.txt";

    // WriteFile: open + data chunks
    let open_msg = sandbox_v1::WriteFileRequest {
        msg: Some(sandbox_v1::write_file_request::Msg::Open(sandbox_v1::WriteFileOpen {
            path: path.into(), mode: 0o644,
        })),
    };
    let data_msg = sandbox_v1::WriteFileRequest {
        msg: Some(sandbox_v1::write_file_request::Msg::Data(b"hello world".to_vec())),
    };
    let (tx, rx) = tokio::sync::mpsc::channel(10);
    tx.send(open_msg).await.unwrap();
    tx.send(data_msg).await.unwrap();
    drop(tx);

    let result = client.write_file(tokio_stream::wrappers::ReceiverStream::new(rx)).await.unwrap().into_inner();
    assert_eq!(result.bytes_written, 11);

    // ReadFile: receive chunks until eof=true
    use tokio_stream::StreamExt;
    let mut stream = client.read_file(sandbox_v1::ReadFileRequest { path: path.into() })
        .await.unwrap().into_inner();
    let mut content = Vec::new();
    tokio::pin!(stream);
    while let Some(chunk) = stream.next().await {
        let c = chunk.unwrap();
        content.extend_from_slice(&c.data);
        if c.eof { break; }
    }
    assert_eq!(content, b"hello world");
}

#[tokio::test]
async fn stat_tmp_is_dir() {
    let mut client = test_client().await;
    let fi = client.stat(sandbox_v1::StatRequest { path: "/tmp".into() }).await.unwrap().into_inner();
    assert!(fi.is_dir);
    assert_eq!(fi.path, "/tmp");
}

#[tokio::test]
async fn mkdir_then_list_sees_entry() {
    let mut client = test_client().await;
    let dir = "/tmp/agent-rs-mkdir-test";
    client.mkdir(sandbox_v1::MkdirRequest { path: dir.into() }).await.unwrap();
    let resp = client.list(sandbox_v1::ListRequest { parent: "/tmp".into(), ..Default::default() }).await.unwrap().into_inner();
    let names: Vec<_> = resp.entries.iter().map(|e| e.name.as_str()).collect();
    assert!(names.contains(&"agent-rs-mkdir-test"));
}
```

Run: `cargo test --test conformance` -> Expected: FAIL (stubs return Unimplemented).

- [ ] **Step 2: Implement all 6 file RPCs in `service/files.rs`.**

Mirror the Go implementations in `grpc_server.go`:

**ReadFile** (mirrors `grpc_server.go:288-305`): open the file with `tokio::fs::read()`, send 32 KiB chunks as `Chunk { data, eof: false }`, then send `Chunk { data: vec![], eof: true }`. A read error becomes `Status::internal(...)` with the OS error string (no file content logged).

**WriteFile** (mirrors `grpc_server.go:311-342`): receive the first message as `open`, then accumulate `data` chunks until stream EOF. Call `tokio::fs::write(path, content)` after `create_dir_all(parent)`. Default mode `0o644` when `open.mode == 0` (mirrors `grpc_server.go:333-338`). Return `WriteFileResult { bytes_written }`. File content is NEVER logged.

**List** (mirrors `grpc_server.go:348-365`): call `tokio::fs::read_dir(parent)` and collect entries into `FileInfo` structs. Pagination (`page_size`, `page_token`) and filtering (`filter`) are not implemented in this slice (return all entries with `next_page_token: ""`, honest behavior matching the Go comment at line 344).

**Stat** (mirrors `grpc_server.go:370-386`): call `tokio::fs::symlink_metadata(path)` (lstat, no dereference). Map `NotFound` to `Status::not_found`. Fill `FileInfo { name, path, is_dir, size, mode, modified_at_unix }`.

**Mkdir** (mirrors `grpc_server.go:390-395`): `tokio::fs::create_dir_all(path)` with mode `0o755` (mirrors Go's `os.MkdirAll(path, 0o755)`).

**Remove** (mirrors `grpc_server.go:399-405`): `tokio::fs::remove_dir_all(path)` if dir, else `tokio::fs::remove_file(path)`. If the path does not exist, return `Ok(RemoveResponse {})` rather than an error (matches Go's `os.RemoveAll` which is a no-op on missing paths).

- [ ] **Step 3: Run conformance tests.**

```bash
cd guest/agent-rs && cargo test --test conformance
```

Expected: PASS for all file RPC tests.

- [ ] **Step 4: Commit.**

```bash
git add guest/agent-rs/src/service/files.rs guest/agent-rs/src/service/mod.rs \
        guest/agent-rs/tests/conformance.rs
git commit -s -m "feat(agent-rs): ReadFile/WriteFile/List/Stat/Mkdir/Remove RPC implementations (#310)"
```

---

### Task 2.3: Archive and Upload (service/archive.rs)

**Files:**
- Create: `guest/agent-rs/src/service/archive.rs`
- Modify: `guest/agent-rs/src/service/mod.rs` (replace Archive/Upload stubs)
- Test: `tests/conformance.rs`

**Interfaces:**
- Consumes: `sandbox_v1::{ArchiveRequest, Chunk, UploadRequest, UploadResult}`.
- Produces: Archive (DOWNLOAD direction only; UNTAR returns InvalidArgument) and Upload, both mirroring `grpc_server.go:413-469` and `tardir.go`.

The workspace allowlist guard (`pathAllowed` in `tardir.go:29-40`) must be replicated:
```rust
const WORKSPACE_ROOT: &str = "/workspace";

fn path_allowed(p: &str) -> bool {
    let clean = std::path::Path::new(p).components()
        .collect::<std::path::PathBuf>();
    let ws = std::path::Path::new(WORKSPACE_ROOT);
    clean == ws || clean.starts_with(ws)
}
```

The tar walk (mirrors `tardir.go:64-135`): use the `tar` crate; walk with `walkdir`; skip symlinks and non-regular entries; bound by `MAX_TAR_BYTES = 512 << 20` (512 MiB, matching `vsock.MaxTarBytes`). The untar (mirrors `tardir.go:159-193`): use `tar::Archive`; for each entry call `safe_join(dst, entry.path())` to reject absolute paths and `..` escapes; only materialize `TypeReg` and `TypeDir` (reject all other types with a `PermissionDenied` error matching `tardir.go:189`).

`safe_join` mirrors `tardir.go:217-234`:
```rust
fn safe_join(dst: &std::path::Path, name: &std::path::Path) -> Result<std::path::PathBuf, AgentError> {
    if name.is_absolute() {
        return Err(AgentError::PathDenied(format!("refusing absolute tar member {:?}", name)));
    }
    let clean = name.components()
        .filter(|c| !matches!(c, std::path::Component::CurDir))
        .collect::<std::path::PathBuf>();
    if clean.components().next() == Some(std::path::Component::ParentDir) {
        return Err(AgentError::PathDenied(format!("refusing traversing tar member {:?}", name)));
    }
    let joined = dst.join(&clean);
    if !joined.starts_with(dst) {
        return Err(AgentError::PathDenied(format!("tar member {:?} resolves outside target", name)));
    }
    Ok(joined)
}
```

- [ ] **Step 1: Write failing conformance test for Archive.**

```rust
#[tokio::test]
async fn archive_workspace_returns_tar() {
    // Create a file under /workspace (or /tmp for host tests) and confirm
    // Archive streams tar bytes ending with an eof chunk.
    // This test uses /tmp/agent-rs-archive-root as a stand-in for /workspace
    // by overriding WORKSPACE_ROOT in test mode.
    use tokio_stream::StreamExt;
    let mut client = test_client().await;
    // Archive UNTAR direction must return InvalidArgument.
    let err = client.archive(sandbox_v1::ArchiveRequest {
        direction: sandbox_v1::archive_request::Direction::Untar as i32,
        path: "/tmp".into(),
    }).await.unwrap_err();
    assert_eq!(err.code(), tonic::Code::InvalidArgument);
}
```

- [ ] **Step 2: Implement Archive and Upload.**

Archive: reject `UNTAR` direction (mirrors `grpc_server.go:415-418`). Check `path_allowed` and return `PermissionDenied` if not (mirrors `grpc_server.go:420-422`). Build tar in memory using the `tar` crate, then send 32 KiB chunks, then `Chunk { eof: true }`.

Upload: receive first message as `open` (dest path); reject if `!path_allowed(dest)`; accumulate chunk bytes; after EOF, extract using `safe_join` guard; return `UploadResult { bytes_written }`.

- [ ] **Step 3: Run conformance tests and commit.**

```bash
cd guest/agent-rs && cargo test --test conformance archive
git add guest/agent-rs/src/service/archive.rs guest/agent-rs/src/service/mod.rs \
        guest/agent-rs/tests/conformance.rs
git commit -s -m "feat(agent-rs): Archive and Upload RPC implementations (#310)"
```

---

### Task 2.4: Watch (service/watch.rs)

**Files:**
- Create: `guest/agent-rs/src/service/watch.rs`
- Modify: `guest/agent-rs/src/service/mod.rs`
- Test: `tests/conformance.rs`

**Interfaces:**
- Consumes: `sandbox_v1::{WatchRequest, FsEvent}`, `notify` crate (inotify on Linux).
- Produces: Watch RPC that streams `FsEvent` messages until client cancels.

Mirror `grpc_runtime.go Watch` (lines 52-202) exactly:
1. `path_allowed` guard with `PermissionDenied` for out-of-workspace paths (mirrors line 54-56).
2. `lstat` the path; reject non-directories with `InvalidArgument` (mirrors lines 57-66).
3. Use `notify::recommended_watcher` configured with `IN_CREATE | IN_MODIFY | IN_DELETE | IN_MOVED_FROM | IN_MOVED_TO | IN_DONT_FOLLOW` (mirrors lines 81-86).
4. Correlate `MOVED_FROM`/`MOVED_TO` pairs by cookie into `RENAME` events; an unmatched `MOVED_FROM` becomes a `DELETE` (mirrors lines 99-199 with `pendingMove` logic).
5. On context cancellation, drop the watcher and return `ctx.Err()` (mirrors lines 88-96).

The `notify` crate provides cookie-correlated rename events on Linux via inotify. Map `notify::Event` kinds to `FsEvent::Kind`:
- `EventKind::Create` -> `FsEvent_Kind::Create`
- `EventKind::Modify` -> `FsEvent_Kind::Modify`
- `EventKind::Remove` -> `FsEvent_Kind::Delete`
- `EventKind::Modify(ModifyKind::Name(RenameMode::Both))` with `paths[0]`/`paths[1]` -> `FsEvent_Kind::Rename`

- [ ] **Step 1: Write failing conformance test for Watch.**

```rust
#[tokio::test]
async fn watch_detects_file_create() {
    use tokio_stream::StreamExt;
    let dir = "/tmp/agent-rs-watch-test";
    std::fs::create_dir_all(dir).unwrap();

    let mut client = test_client().await;
    let mut stream = client.watch(sandbox_v1::WatchRequest {
        path: dir.into(), recursive: false,
    }).await.unwrap().into_inner();

    // Create a file after the watch is established.
    tokio::time::sleep(std::time::Duration::from_millis(20)).await;
    std::fs::write(format!("{}/hello.txt", dir), b"hi").unwrap();

    tokio::pin!(stream);
    let ev = tokio::time::timeout(
        std::time::Duration::from_secs(2),
        stream.next(),
    ).await.expect("no event within 2s").unwrap().unwrap();

    assert_eq!(ev.kind, sandbox_v1::fs_event::Kind::Create as i32);
    assert!(ev.path.ends_with("hello.txt"));
}
```

- [ ] **Step 2: Implement Watch in `service/watch.rs`, run test, commit.**

```bash
git add guest/agent-rs/src/service/watch.rs guest/agent-rs/src/service/mod.rs \
        guest/agent-rs/tests/conformance.rs
git commit -s -m "feat(agent-rs): Watch RPC with inotify event streaming (#310)"
```

---

### Task 2.5: Processes and Signal (service/processes.rs)

**Files:**
- Create: `guest/agent-rs/src/service/processes.rs`
- Modify: `guest/agent-rs/src/service/mod.rs`
- Test: `tests/conformance.rs`

**Interfaces:**
- Consumes: `sandbox_v1::{ProcessesRequest, ProcessList, ProcessInfo, SignalRequest, SignalResponse}`, `sys::signal::{kill, signal_userspace}`.
- Produces: Processes and Signal RPCs.

Mirror `grpc_runtime.go Processes` (lines 215-255) and `Signal` (lines 296-316):

**Processes**: read `/proc/<pid>/stat` for each numeric pid directory; parse to get `pid`, `ppid`, `comm` (field 2, the process name in parentheses), `state` (field 3), `utime+stime` (fields 14+15), `rss` pages (field 24). Take two snapshots 100 ms apart (mirrors `processCPUSampleWindow = 100ms` at `grpc_runtime.go:36`) and compute CPU percent from the delta divided by the aggregate `/proc/stat` total delta (mirrors lines 226-253). Use `comm` ONLY - NEVER `cmdline` (mirrors the SECURITY comment at `grpc_runtime.go:209-212`: cmdline may contain secrets passed as argv).

```rust
fn parse_pid_stat(data: &[u8]) -> Option<PidStat> {
    // Format: pid (comm) state ppid ... utime stime ... rss ...
    // The comm field is enclosed in parens and may contain spaces;
    // find the last ')' to locate the remaining fields.
    let s = std::str::from_utf8(data).ok()?;
    let rparen = s.rfind(')')?;
    let pid_str = &s[..s.find('(')?].trim();
    let comm_str = &s[s.find('(')?+1..rparen];
    let rest: Vec<&str> = s[rparen+2..].split_whitespace().collect();
    Some(PidStat {
        pid:       pid_str.parse().ok()?,
        comm:      comm_str.to_string(),
        state:     rest.first()?.to_string(),
        ppid:      rest.get(1)?.parse().ok()?,
        utime:     rest.get(11)?.parse::<u64>().ok()?,
        stime:     rest.get(12)?.parse::<u64>().ok()?,
        rss_pages: rest.get(21)?.parse::<u64>().ok()?,
    })
}
```

**Signal**: validate `pid > 1` and `signal in 1..=64`, then call `sys::signal::kill(pid, signal)`. Map `ESRCH` to `NotFound`, `EPERM` to `PermissionDenied`, other errors to `Internal` (mirrors `grpc_runtime.go:307-315`).

- [ ] **Step 1: Write failing tests, implement, run, commit.**

```bash
git add guest/agent-rs/src/service/processes.rs guest/agent-rs/src/service/mod.rs \
        guest/agent-rs/tests/conformance.rs
git commit -s -m "feat(agent-rs): Processes and Signal RPC implementations (#310)"
```

---

### Task 2.6: PortForward (service/portforward.rs)

**Files:**
- Create: `guest/agent-rs/src/service/portforward.rs`
- Modify: `guest/agent-rs/src/service/mod.rs`
- Test: `tests/conformance.rs`

**Interfaces:**
- Consumes: `sandbox_v1::{Frame, PortForwardOpen}`.
- Produces: PortForward RPC that dials `127.0.0.1:<port>` inside the guest and splices bytes bidirectionally.

Mirror `grpc_server.go PortForward` (lines 602-700):
1. Receive first Frame; require `open` oneof with `port in 1..=65535`.
2. Dial `127.0.0.1:<port>` with a 5-second timeout (mirrors `tunnelDialTimeout = 5s` in `tunnel.go:18`). Loopback ONLY - the client carries only a port number.
3. Spawn two tasks: one reads from the gRPC stream and writes to the TCP socket; one reads from the TCP socket and sends `Frame::Data` messages on the gRPC stream.
4. Both directions call a shared `stop()` on error or EOF; `stop()` closes the TCP socket once so both tasks join (mirrors `sync.Once` at `grpc_server.go:628-635`).
5. Stream context cancel closes the TCP socket (mirrors `grpc_server.go:640-645`).

Use `tokio::net::TcpStream` and `tokio::io::copy` in each direction inside `tokio::spawn`. Use `tokio::sync::oneshot` as the `once` equivalent.

- [ ] **Step 1: Write failing test, implement, run, commit.**

```bash
git add guest/agent-rs/src/service/portforward.rs guest/agent-rs/src/service/mod.rs \
        guest/agent-rs/tests/conformance.rs
git commit -s -m "feat(agent-rs): PortForward bidirectional TCP splice RPC (#310)"
```

---

### Task 2.7: Vitals (service/vitals.rs)

**Files:**
- Create: `guest/agent-rs/src/service/vitals.rs`
- Modify: `guest/agent-rs/src/service/mod.rs`
- Test: `tests/conformance.rs`

**Interfaces:**
- Consumes: `sandbox_v1::{VitalsRequest, GuestVitals}`.
- Produces: Vitals streaming RPC.

Mirror `grpc_server.go Vitals` (lines 550-578) and `vitals.go sampleVitals` (lines 52-83):
1. Read `/proc/stat` twice 100 ms apart, compute steal fraction from the difference (mirrors `guestvitals.StealDelta` and `StealFraction()`).
2. Read `/proc/meminfo` for `MemTotal`, `MemAvailable` (used = total - available).
3. Assemble `GuestVitals { sampled_at_unix, cpu_percent: 0.0, cpu_steal_percent, mem_used_bytes, mem_total_bytes, mem_balloon_bytes: 0, process_count }`.
4. If `interval_seconds <= 0`: send one sample and close (mirrors `grpc_server.go:559`).
5. If `interval_seconds > 0`: send the first sample immediately, then tick on a `tokio::time::interval` until context cancels (mirrors `grpc_server.go:565-578`).

Parse `/proc/stat` line 1: `cpu  user nice system idle iowait irq softirq steal ...` where steal is field 8 (0-indexed from "cpu"). Steal percent = `steal_delta / total_delta * 100`.

Parse `/proc/meminfo`: scan for `MemTotal:` and `MemAvailable:` lines, parse KB values.

- [ ] **Step 1: Write failing test, implement, run, commit.**

```bash
git add guest/agent-rs/src/service/vitals.rs guest/agent-rs/src/service/mod.rs \
        guest/agent-rs/tests/conformance.rs
git commit -s -m "feat(agent-rs): Vitals streaming RPC with /proc sampler (#310)"
```

---

### Task 2.8: RunCode and kernel/ module (kernel/mod.rs, kernel/driver.rs, service/runcode.rs)

**Files:**
- Modify: `guest/agent-rs/src/kernel/mod.rs`
- Create: `guest/agent-rs/src/kernel/driver.rs`
- Create: `guest/agent-rs/src/service/runcode.rs`
- Modify: `guest/agent-rs/src/service/mod.rs`
- Test: `tests/conformance.rs`

**Risk note:** RunCode is the largest single port (~150 LOC of kernel.go plus protocol translation). The ipykernel subprocess may not be present in CI. The test is conditional: if `/opt/mitos/kernel_driver.py` is absent, the test confirms that `RunCode` returns a `KernelUnavailable` error frame with exit code 127 rather than panicking.

**Interfaces:**
- Consumes: `sandbox_v1::{RunCodeRequest, RunCodeOpen, RunCodeResponse, RunResult, RunError}`.
- Produces:
  ```rust
  // kernel/mod.rs
  pub struct KernelManager { /* cfg, started, dead, child, stdin_writer, stdout_lines */ }
  impl KernelManager {
      pub fn new() -> Self;
      /// Mirror kernelManager.run in kernel.go:82-168. Serialized by Mutex.
      pub async fn run(
          &mut self,
          code: &str,
          language: &str,
          timeout_secs: i64,
          emit: &mut dyn FnMut(KernelFrame),
      ) -> Result<(), crate::error::AgentError>;
  }

  pub enum KernelFrame {
      Stdout(Vec<u8>), Stderr(Vec<u8>),
      Result { text: String, data: std::collections::HashMap<String,Vec<u8>> },
      Error { name: String, value: String, traceback: Vec<String> },
      Exit(i32),
  }
  ```

- [ ] **Step 1: Write failing conformance test for RunCode.**

```rust
#[tokio::test]
async fn runcode_kernel_unavailable_returns_error_frame() {
    // When /opt/mitos/kernel_driver.py is absent, RunCode must emit a
    // KernelUnavailable error frame then exit 127, NOT panic.
    // Mirrors errorFrames in kernel.go:67-76 and the Go test behavior.
    use tokio_stream::StreamExt;
    let mut client = test_client().await;
    let open = sandbox_v1::RunCodeRequest {
        msg: Some(sandbox_v1::run_code_request::Msg::Open(sandbox_v1::RunCodeOpen {
            code: "print(1+1)".into(),
            language: "python".into(),
            timeout_seconds: 10,
        })),
    };
    let (tx, rx) = tokio::sync::mpsc::channel(4);
    tx.send(open).await.unwrap();
    drop(tx);

    let mut stream = client.run_code(tokio_stream::wrappers::ReceiverStream::new(rx))
        .await.unwrap().into_inner();
    tokio::pin!(stream);

    let mut got_error = false;
    let mut exit_code = -1i32;
    while let Some(msg) = stream.next().await {
        match msg.unwrap().msg.unwrap() {
            sandbox_v1::run_code_response::Msg::Error(e) => {
                assert_eq!(e.name, "KernelUnavailable");
                got_error = true;
            }
            sandbox_v1::run_code_response::Msg::ExitCode(c) => {
                exit_code = c;
            }
            _ => {}
        }
    }
    // If the kernel driver IS present, this test just verifies the RPC works.
    // If absent, got_error must be true and exit_code must be 127.
    if !std::path::Path::new("/opt/mitos/kernel_driver.py").exists() {
        assert!(got_error, "expected KernelUnavailable error frame");
        assert_eq!(exit_code, 127);
    }
}
```

Run: `cargo test --test conformance runcode` -> Expected: FAIL (stub).

- [ ] **Step 2: Implement `kernel/driver.rs` (JSON line protocol to kernel_driver.py).**

Mirror `kernel.go ensureStarted` (lines 172-220) and the event loop in `run` (lines 122-168):

1. `ensureStarted()`: stat `/opt/mitos/kernel_driver.py`; if absent, return `KernelUnavailable`. Spawn `python3 /opt/mitos/kernel_driver.py` in `/workspace` if it exists; wire `stdin_pipe`, `stdout_pipe`. Read lines until a `{"kind":"ready"}` line is received (mirrors `kernel.go:204-213`). Store `stdin_writer: BufWriter<ChildStdin>`, `stdout_lines: Lines<BufReader<ChildStdout>>`.

2. `run(code, language, timeout, emit)`: validate language is `"python"` or `""` (mirrors `kernel.go:83-87`). Call `ensureStarted()`; on failure emit `KernelUnavailable` frame + exit 127. Write `{"id":"e","code":"...","timeout":N}\n` to stdin. Read lines from stdout; for each:
   - `"kind":"stdout"` -> `emit(KernelFrame::Stdout(text.into_bytes()))`
   - `"kind":"stderr"` -> `emit(KernelFrame::Stderr(text.into_bytes()))`
   - `"kind":"result"` -> `emit(KernelFrame::Result { text, data })`
   - `"kind":"error"` -> `emit(KernelFrame::Error { name, value, traceback })`
   - `"kind":"done"` -> emit `KernelFrame::Exit(if status=="error" {1} else {0})`; return Ok.
   - Any other line causes `dead = true` and returns an error (mirrors `kernel.go:164-168`).

Use `std::process::Command` (not `tokio::process`) because the kernel subprocess is long-lived and serialized by the Mutex; async is not needed here and complicates `BufReader::lines`.

- [ ] **Step 3: Implement `service/runcode.rs` (mirrors `grpc_server.go RunCode` at lines 480-507).**

```rust
pub async fn run_code_handler(
    kernel: Arc<Mutex<KernelManager>>,
    mut stream: tonic::Streaming<sandbox_v1::RunCodeRequest>,
    tx: tokio::sync::mpsc::Sender<Result<sandbox_v1::RunCodeResponse, Status>>,
) -> Result<(), Status> {
    // Receive first message; must carry open.
    let first = stream.message().await
        .map_err(|e| Status::internal(format!("run_code: recv: {}", e)))?
        .ok_or_else(|| Status::invalid_argument("run_code: stream closed before open"))?;
    let open = match first.msg {
        Some(sandbox_v1::run_code_request::Msg::Open(o)) => o,
        _ => return Err(Status::invalid_argument("run_code: first message must carry open")),
    };

    let mut guard = kernel.lock().await;
    let mut send_err = None;
    let emit = &mut |frame: KernelFrame| {
        if send_err.is_some() { return; }
        let msg = kernel_frame_to_proto(frame);
        if tx.blocking_send(Ok(msg)).is_err() {
            send_err = Some(Status::unavailable("run_code: stream send failed"));
        }
    };

    if let Err(e) = guard.run(&open.code, &open.language, open.timeout_seconds, emit).await {
        emit(KernelFrame::Error { name: "KernelStreamError".into(), value: e.to_string(), traceback: vec![] });
        emit(KernelFrame::Exit(1));
    }
    send_err.map(Err).unwrap_or(Ok(()))
}
```

Note: because `KernelManager::run` holds the Mutex for its entire duration (exactly as Go serializes with `k.mu.Lock()`), `blocking_send` should not be used in an async context with a blocking Mutex. The implementer must use `tokio::sync::Mutex` for `KernelManager` and send via `tx.send(...).await` within a `spawn_blocking` block, or restructure the emit callback to collect frames into a `Vec` and send them after the synchronous `run` call returns.

- [ ] **Step 4: Run conformance tests and commit.**

```bash
cd guest/agent-rs && cargo test --test conformance runcode
git add guest/agent-rs/src/kernel/mod.rs guest/agent-rs/src/kernel/driver.rs \
        guest/agent-rs/src/service/runcode.rs guest/agent-rs/src/service/mod.rs \
        guest/agent-rs/tests/conformance.rs
git commit -s -m "feat(agent-rs): RunCode RPC and ipykernel bridge (#310)"
```

---

## Phase 3: Fork-correctness (fork/ module)

**Security note:** This entire phase is a named security-sensitive path. Every PR touching `fork/` requires a named human reviewer before merge.

### Task 3.1: Reseed CRNG (fork/reseed.rs)

**Files:**
- Create: `guest/agent-rs/src/fork/reseed.rs`
- Modify: `guest/agent-rs/src/fork/mod.rs`

**Interfaces:**
- Consumes: `sys::entropy::reseed_crng`.
- Produces:
  ```rust
  /// Calls sys::entropy::reseed_crng. Returns true ONLY on credited success.
  /// Fail-closed: returns false rather than claiming success on any failure.
  /// Mirrors reseedCRNG in notifyforked.go:71-119.
  pub fn reseed(entropy: &[u8]) -> bool;
  ```

- [ ] **Step 1: Write failing test.**

```rust
#[cfg(test)]
mod tests {
    #[test]
    fn empty_entropy_is_false() {
        assert!(!super::reseed(&[]));
    }
    #[test]
    #[cfg(target_os = "linux")]
    fn nonzero_entropy_does_not_panic() {
        // Return value depends on capability; both outcomes are valid.
        let _ = super::reseed(b"divergence-material");
    }
}
```

- [ ] **Step 2: Implement and commit.**

```bash
git add guest/agent-rs/src/fork/reseed.rs guest/agent-rs/src/fork/mod.rs
git commit -s -m "feat(agent-rs): fork/reseed - credited CRNG reseed, fail-closed (#310)"
```

---

### Task 3.2: Clock step (fork/clock.rs)

**Files:**
- Create: `guest/agent-rs/src/fork/clock.rs`
- Modify: `guest/agent-rs/src/fork/mod.rs`

**Interfaces:**
- Produces:
  ```rust
  const CLOCK_STEP_THRESHOLD_NANOS: i64 = 500 * 1_000_000; // 500ms, mirrors notifyforked.go:23

  /// Steps CLOCK_REALTIME toward host_wall_clock_nanos when drift exceeds the
  /// 500ms threshold. Returns the signed step applied in nanoseconds (0 when
  /// within tolerance, 0 when host_wall_clock_nanos == 0, 0 on any syscall error).
  /// Mirrors stepClock in notifyforked.go:138-164.
  /// NOTE: CLOCK_MONOTONIC is deliberately NOT touched (Linux rejects it with
  /// EINVAL). See notifyforked.go:125-135 for the rationale.
  pub fn step_clock(host_wall_clock_nanos: i64) -> i64;
  ```

- [ ] **Step 1: Write failing test.**

```rust
#[test]
fn zero_host_time_returns_zero() {
    assert_eq!(super::step_clock(0), 0);
}
#[test]
fn within_threshold_returns_zero() {
    // A host time very close to now should be within 500ms and return 0.
    let now_nanos = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH).unwrap()
        .as_nanos() as i64;
    let result = super::step_clock(now_nanos + 100_000_000); // 100ms ahead
    assert_eq!(result, 0);
}
```

- [ ] **Step 2: Implement and commit.**

Implementation mirrors `stepClock` in `notifyforked.go:138-164`: call `sys::clock::get_realtime_nanos()`, compute `drift = host - guest`, if `abs(drift) <= CLOCK_STEP_THRESHOLD_NANOS` return 0, else call `sys::clock::set_realtime_nanos(host_wall_clock_nanos)` and return `drift` on success or 0 on error.

```bash
git add guest/agent-rs/src/fork/clock.rs guest/agent-rs/src/fork/mod.rs
git commit -s -m "feat(agent-rs): fork/clock - CLOCK_REALTIME step with 500ms threshold (#310)"
```

---

### Task 3.3: Network reconfiguration (fork/network.rs)

**Files:**
- Create: `guest/agent-rs/src/fork/network.rs`
- Modify: `guest/agent-rs/src/fork/mod.rs`

**Interfaces:**
- Produces:
  ```rust
  pub struct NetworkConfig {
      pub guest_ip:    String,
      pub gateway_ip:  String,
      pub prefix_len:  u32,
      pub guest_mac:   String,
      pub resolver_ip: String,
  }
  /// Configures eth0 with the per-fork address and default route. Writes
  /// /etc/resolv.conf when resolver_ip is non-empty. No-op when cfg is None.
  /// Mirrors configureNetwork in notifyforked.go:193-210 and writeResolvConf
  /// at notifyforked.go:223-232.
  /// Uses rtnetlink (via the `rtnetlink` crate or raw Netlink sockets) because
  /// templates may ship no iproute2 binary (mirrors the rationale in
  /// notifyforked.go:188-191).
  pub fn configure_network(cfg: Option<&NetworkConfig>);
  ```

The Go reference calls `guestnet.Configure(guestNetIface, ...)` which uses rtnetlink internally. The Rust implementation should use the `rtnetlink` crate or issue raw AF_NETLINK sockets to: (1) flush existing eth0 addresses, (2) add the new address/prefix, (3) add a default route via the gateway. This is a security-sensitive path. The address values are config, not secrets.

- [ ] **Step 1: Write failing test (no-op on non-Linux or without a network interface).**

```rust
#[test]
fn none_config_is_noop() {
    // Must not panic or error when cfg is None.
    super::configure_network(None);
}
```

- [ ] **Step 2: Implement and commit.**

```bash
git add guest/agent-rs/src/fork/network.rs guest/agent-rs/src/fork/mod.rs
git commit -s -m "feat(agent-rs): fork/network - eth0 reconfiguration via rtnetlink (#310)"
```

---

### Task 3.4: Volume mounts (fork/volumes.rs)

**Files:**
- Create: `guest/agent-rs/src/fork/volumes.rs`
- Modify: `guest/agent-rs/src/fork/mod.rs`

**Interfaces:**
- Produces:
  ```rust
  pub struct VolumeMountEntry {
      pub device:     String,
      pub mount_path: String,
      pub read_only:  bool,
  }
  /// Mounts each volume, skipping already-mounted paths (idempotent).
  /// Returns the count of paths now mounted.
  /// Mirrors mountVolumes in notifyforked.go:247-276 exactly:
  /// - Skips entries with empty device or mount_path (logs warning).
  /// - Skips already-mounted paths via sys::mount::is_mounted.
  /// - mkdir -p the mount path.
  /// - Mounts with ext4 filesystem, optionally MS_RDONLY.
  /// - Best effort per entry: logs failure and continues.
  pub fn mount_volumes(entries: &[VolumeMountEntry]) -> i32;
  ```

- [ ] **Step 1: Write failing test, implement, commit.**

```rust
#[test]
fn empty_entries_returns_zero() {
    assert_eq!(super::mount_volumes(&[]), 0);
}
#[test]
fn skip_entry_with_empty_device() {
    let e = super::VolumeMountEntry { device: "".into(), mount_path: "/mnt/x".into(), read_only: false };
    assert_eq!(super::mount_volumes(&[e]), 0);
}
```

```bash
git add guest/agent-rs/src/fork/volumes.rs guest/agent-rs/src/fork/mod.rs
git commit -s -m "feat(agent-rs): fork/volumes - per-fork volume mounts (#310)"
```

---

### Task 3.5: SIGUSR2 signal and handle_notify_forked orchestrator

**Files:**
- Create: `guest/agent-rs/src/fork/signal.rs`
- Modify: `guest/agent-rs/src/fork/mod.rs`

**Interfaces:**
- Produces:
  ```rust
  // fork/signal.rs
  /// Sends SIGUSR2 to all userspace processes except PID 1 and self.
  /// Mirrors signalUserspace in notifyforked.go:299-328.
  pub fn signal_userspace() -> i32 { crate::sys::signal::signal_userspace() }

  // fork/mod.rs
  pub struct NotifyForkedRequest {
      pub generation:            u64,
      pub host_wall_clock_nanos: i64,
      pub entropy:               Vec<u8>,
      pub network:               Option<network::NetworkConfig>,
      pub volumes:               Vec<volumes::VolumeMountEntry>,
  }
  pub struct NotifyForkedResponse {
      pub applied_clock_step_nanos: i64,
      pub reseeded_rng:             bool,
      pub signaled_processes:       i32,
  }
  /// Orchestrates all 5 fork-correctness actions in the same order as
  /// handleNotifyForked in notifyforked.go:33-57.
  pub fn handle_notify_forked(req: &NotifyForkedRequest) -> NotifyForkedResponse;
  ```

The orchestration order from `notifyforked.go:33-57`:
1. `reseed_crng(entropy)` -> `reseeded`
2. `step_clock(host_wall_clock_nanos)` -> `step`
3. `write_fork_generation(generation)` (write `generation` as decimal to `/run/sandbox/fork-generation`)
4. `configure_network(network)`
5. `mount_volumes(volumes)` -> `mounted`
6. `signal_userspace()` -> `signaled`
7. Log: `info!(generation, entropy_bytes = entropy.len(), reseeded, clock_step_ns = step, volumes_mounted = mounted, signaled)` - entropy bytes only as count, never the bytes themselves.
8. Return `NotifyForkedResponse { applied_clock_step_nanos: step, reseeded_rng: reseeded, signaled_processes: signaled }`.

- [ ] **Step 1: Write failing integration test for handle_notify_forked.**

```rust
#[test]
fn notify_forked_with_empty_entropy_returns_false_reseed() {
    let req = super::NotifyForkedRequest {
        generation: 1, host_wall_clock_nanos: 0,
        entropy: vec![], network: None, volumes: vec![],
    };
    let resp = super::handle_notify_forked(&req);
    assert!(!resp.reseeded_rng);
    assert_eq!(resp.applied_clock_step_nanos, 0);
}

#[test]
fn notify_forked_logs_count_not_entropy_bytes() {
    // This test is enforced more rigorously by the secret_log_audit test in Task 3.6.
    // Here we just confirm the function does not panic with real entropy bytes.
    let req = super::NotifyForkedRequest {
        generation: 2, host_wall_clock_nanos: 0,
        entropy: vec![0xde, 0xad, 0xbe, 0xef],
        network: None, volumes: vec![],
    };
    let _ = super::handle_notify_forked(&req);
}
```

- [ ] **Step 2: Implement and commit.**

```bash
git add guest/agent-rs/src/fork/signal.rs guest/agent-rs/src/fork/mod.rs
git commit -s -m "feat(agent-rs): fork/mod - handle_notify_forked orchestrator (#310)"
```

---

## Phase 4: PID-1 init and main wiring

### Task 4.1: Wire NotifyForked into the tonic Control service and main

**Files:**
- Create: `guest/agent-rs/src/service/control.rs`
- Modify: `guest/agent-rs/src/service/mod.rs`
- Modify: `guest/agent-rs/src/main.rs`
- Test: `tests/conformance.rs`

**Interfaces:**

The Go gRPC server serves BOTH `sandbox.v1.Sandbox` AND `sandbox.internal.v1.Control` on the same gRPC port (`grpc_server.go:43-48`). The Rust agent must do the same. However, `proto/sandbox/controlv1/` may not exist in the Rust worktree (it is part of the `feat+api-v1-consolidation` work). If it is absent, the NotifyForked path is wired as a method on the Sandbox service itself under the `internal/` namespace, matching whatever contract the Go gRPC server exposes.

Check whether `proto/sandbox/controlv1/control.proto` exists:
```bash
ls ../../proto/sandbox/controlv1/ 2>/dev/null || echo "control proto not present"
```

If present: copy it alongside `sandbox.proto`, add it to `build.rs`, and implement `ControlService` mirroring `grpc_server.go controlServer` (lines 703-802).

If absent: implement `handle_notify_forked` as an additional method wired via a separate mechanism (e.g., a Unix socket for internal control traffic) and document this as a known gap pending the control proto being merged.

- [ ] **Step 1: Determine control proto availability and implement accordingly.**

- [ ] **Step 2: Wire both services onto the tonic Server in main.rs.**

```rust
Server::builder()
    .add_service(SandboxServer::new(svc.clone()))
    .add_service(ControlServer::new(ctrl_svc))  // if control proto is present
    .serve_with_incoming(listener)
    .await?;
```

- [ ] **Step 3: Add conformance test for NotifyForked (via the control service).**

- [ ] **Step 4: Commit.**

```bash
git add guest/agent-rs/src/service/control.rs guest/agent-rs/src/service/mod.rs \
        guest/agent-rs/src/main.rs guest/agent-rs/tests/conformance.rs
git commit -s -m "feat(agent-rs): wire NotifyForked control service and final main (#310)"
```

---

## Phase 5: No-regression gates

### Task 5.1: Secret-log audit test (tests/secret_log_audit.rs)

**Files:**
- Create: `guest/agent-rs/tests/secret_log_audit.rs`

**Interfaces:**
- Consumes: `tracing-subscriber` subscriber that captures log output to a `Vec<String>`.
- Produces: a property test that runs `ConfiguredEnv::merge` with a known secret value and asserts the value does not appear in any tracing log line.

The global constraint from the spec: "a hard, test-enforced rule that secret values, entropy bytes, argv, and file contents are never logged."

```rust
// tests/secret_log_audit.rs

use std::sync::{Arc, Mutex};
use tracing_subscriber::layer::SubscriberExt;

struct CaptureLayer { lines: Arc<Mutex<Vec<String>>> }

impl<S: tracing::Subscriber> tracing_subscriber::Layer<S> for CaptureLayer {
    fn on_event(&self, event: &tracing::Event<'_>, _ctx: tracing_subscriber::layer::Context<'_, S>) {
        let mut visitor = StringVisitor(String::new());
        event.record(&mut visitor);
        self.lines.lock().unwrap().push(visitor.0);
    }
}

struct StringVisitor(String);
impl tracing::field::Visit for StringVisitor {
    fn record_debug(&mut self, field: &tracing::field::Field, value: &dyn std::fmt::Debug) {
        self.0.push_str(&format!("{:?}", value));
    }
    fn record_str(&mut self, field: &tracing::field::Field, value: &str) {
        self.0.push_str(value);
    }
}

#[tokio::test]
async fn configure_secret_never_appears_in_logs() {
    let captured = Arc::new(Mutex::new(Vec::<String>::new()));
    let layer = CaptureLayer { lines: Arc::clone(&captured) };
    let subscriber = tracing_subscriber::registry().with(layer);
    let _guard = tracing::subscriber::set_default(subscriber);

    let env = sandbox_agent::env::ConfiguredEnv::new();
    env.merge(
        &[],
        &[("API_KEY".to_string(), "super-secret-value-12345".to_string())],
    ).await;

    let lines = captured.lock().unwrap();
    for line in lines.iter() {
        assert!(
            !line.contains("super-secret-value-12345"),
            "secret value leaked into log line: {:?}", line
        );
    }
}

#[test]
fn entropy_bytes_never_appear_in_logs() {
    let captured = Arc::new(Mutex::new(Vec::<String>::new()));
    let layer = CaptureLayer { lines: Arc::clone(&captured) };
    let subscriber = tracing_subscriber::registry().with(layer);
    let _guard = tracing::subscriber::set_default(subscriber);

    let entropy = b"\xde\xad\xbe\xef\xca\xfe";
    // Hex representation of entropy bytes as it would appear in a debug log.
    let hex = entropy.iter().map(|b| format!("{:02x}", b)).collect::<String>();

    let _ = sandbox_agent::fork::reseed(&entropy[..]);

    let lines = captured.lock().unwrap();
    for line in lines.iter() {
        assert!(
            !line.contains(&hex),
            "entropy bytes leaked into log line: {:?}", line
        );
    }
}
```

- [ ] **Step 1: Run the failing test.**

```bash
cargo test --test secret_log_audit
```

Expected: FAIL until tracing calls are confirmed to not log secret values.

- [ ] **Step 2: Verify all tracing calls in `env.rs` and `fork/mod.rs` log only counts and field names.**

Audit: `env.rs` must log `configured_count` (a count), never any key or value. `fork/mod.rs` must log `entropy_bytes = req.entropy.len()` (a count), never `req.entropy`. Make any needed corrections, then rerun.

- [ ] **Step 3: Run and confirm PASS, then commit.**

```bash
git add guest/agent-rs/tests/secret_log_audit.rs
git commit -s -m "test(agent-rs): enforce no-secret-in-log invariant (#310)"
```

---

### Task 5.2: Agent conformance harness (cross-agent regression net)

**Files:**
- Create: `bench/agent-conformance/` (a new Go program, or extend `cmd/bench`)
- Modify: `.github/workflows/kvm-test.yaml`

**Interfaces:**
- Consumes: the tonic client (or a Go gRPC client) connecting to either the Go or Rust agent.
- Produces: a harness that runs the same suite of RPC assertions against both agents and reports any divergence. This is "gate 1" from the spec: "both agents serve identical sandbox.v1 from the same proto."

The harness is a Go test binary (to reuse the Go `sandbox/v1` proto client) that:
1. Accepts a `--agent-addr vsock:<cid>:<port>` or `--agent-addr unix:<path>` flag.
2. Runs each of the 15 RPCs with a fixed input.
3. Compares the proto-level response (ignoring wall-clock fields like `sampled_at_unix`) between the Go agent baseline and the Rust agent under test.
4. Exits non-zero if any RPC diverges.

- [ ] **Step 1: Write the harness skeleton with one test (Stat on `/tmp`).**

```go
// bench/agent-conformance/main_test.go
package conformance

import (
    "testing"
    "google.golang.org/grpc"
    sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

func TestStatTmpIsDir(t *testing.T) {
    conn, err := grpc.Dial(*agentAddr, grpc.WithInsecure())
    // ...
    client := sandboxv1.NewSandboxClient(conn)
    fi, err := client.Stat(ctx, &sandboxv1.StatRequest{Path: "/tmp"})
    if err != nil { t.Fatal(err) }
    if !fi.IsDir { t.Errorf("expected /tmp to be a directory") }
}
```

- [ ] **Step 2: Extend to all 15 RPCs, run against both agents, commit.**

```bash
git add bench/agent-conformance/
git commit -s -m "test(agent-rs): agent-conformance harness for cross-agent regression (#310)"
```

---

### Task 5.3: Static build, binary size gate, CI integration

**Files:**
- Modify: `hack/build-rust-agent.sh`
- Modify: `.github/workflows/kvm-test.yaml`
- Create: `docs/threat-model.md` delta (add `guest/agent-rs` row)
- Create: `docs/fork-correctness.md` delta (note Rust reseed/clock path)

**Interfaces:**
- Produces: updated `hack/build-rust-agent.sh` that builds the tonic agent (not the spike), reports binary size, and fails if size exceeds a gate (e.g., 10 MiB - well below Go's 4.97 MiB ceiling would be ideal, but the real size is measured and the gate set from the first passing build). The gate is set AFTER the first real measurement, per operating principle 1 (no unverified claims).

- [ ] **Step 1: Update `hack/build-rust-agent.sh` for the new crate layout.**

The script is already correct structurally; verify it still produces a static binary:
```bash
bash hack/build-rust-agent.sh
file target/x86_64-unknown-linux-musl/release/sandbox-agent
```

Expected: `statically linked`.

- [ ] **Step 2: Record the binary size as the gate in the script.**

After measuring the real size on box1:
```bash
printf 'rust-agent size check: %s bytes (gate: <= 12582912)\n' "$(stat -c%s "$BIN")"
[ "$(stat -c%s "$BIN")" -le 12582912 ] || { echo "FAIL: binary size exceeds gate"; exit 1; }
```

The gate value (12 MiB) is a placeholder pending the first real measurement. Update it to 110% of the measured size after Task 5.4.

- [ ] **Step 3: Add `guest/agent-rs` row to `docs/threat-model.md` and note in `docs/fork-correctness.md`.**

In `docs/threat-model.md`, add a row for `guest/agent-rs` noting: same vsock isolation posture as the Go agent; `sys/` module contains all unsafe; `fork/reseed` uses RNDADDENTROPY fail-closed; named human reviewer required before graduation to default.

In `docs/fork-correctness.md`, add a note under each of the 5 fork-correctness actions indicating the Rust implementation location (`fork/reseed.rs`, `fork/clock.rs`, `fork/network.rs`, `fork/volumes.rs`, `fork/signal.rs`) and that the fail-closed behavior is identical to the Go reference.

- [ ] **Step 4: Commit.**

```bash
git add hack/build-rust-agent.sh docs/threat-model.md docs/fork-correctness.md
git commit -s -m "feat(agent-rs): static build gate, threat model, fork-correctness docs (#310)"
```

---

### Task 5.4: Bare-metal bench gate (box1 + box2)

**Files:**
- Create: `bench/results/2026-06-23-sp1-rust-agent-grpc.md`

**Interfaces:**
- Consumes: existing `cmd/bench`, the Rust agent template baked per `hack/rust-agent-rootfs.md`, both bench boxes.
- Produces: fork-exec p50/p90/p99 comparison (Go vs Rust), exec-rt p50/p90/p99, per-VM RSS, binary size. Every number reproducible from `bench/`. Numbers archived as JSON.

The gate from the spec: "the bare-metal bench on box1 AND box2 must show the fork-exec win holds and NO metric regresses."

- [ ] **Step 1: Run on box1, then box2, using the identical harness as the spike baseline.**

```bash
# On each box:
go build -o /tmp/bench ./cmd/bench/
/tmp/bench --mode fork-exec --template "$RUST_GRPC_TEMPLATE_ID" --iterations 200 --warmup 20 \
  --summary --json /tmp/rust-grpc-fork-box1.json
/tmp/bench --mode exec-rt  --template "$RUST_GRPC_TEMPLATE_ID" --iterations 200 --warmup 20 \
  --summary --json /tmp/rust-grpc-execrt-box1.json
```

- [ ] **Step 2: Write the comparison doc; commit.**

```bash
git add bench/results/2026-06-23-sp1-rust-agent-grpc.md \
        bench/results/rust-grpc-fork-box1.json bench/results/rust-grpc-execrt-box1.json
git commit -s -m "bench: SP1 Rust gRPC agent bare-metal bench results (#310)"
```

---

### Task 5.5: Opt-in rootfs selector and cutover strategy

**Files:**
- Modify: `hack/rust-agent-rootfs.md` (update for the gRPC agent)
- Create: docs note on per-pool/template selector

**Interfaces:**
- Produces: documented cutover strategy matching the spec's four steps: opt-in per-pool/template selector, soak in CI + canary pool on box2's k3s cluster, flip default only after all four gates are green, rollback is reverting the template selection.

- [ ] **Step 1: Update `hack/rust-agent-rootfs.md` for the tonic binary path and vsock port 53.**

- [ ] **Step 2: Commit.**

```bash
git add hack/rust-agent-rootfs.md
git commit -s -m "docs(agent-rs): SP1 rootfs cutover strategy for the gRPC agent (#310)"
```

---

## Self-Review

### Spec coverage check

| Spec requirement | Covered by |
|---|---|
| 15 runtime RPCs: Exec, ReadFile, WriteFile, List, Stat, Mkdir, Remove, Archive, Upload, Watch, Processes, Signal, PortForward, Vitals, RunCode | Tasks 2.1-2.8 |
| Self-service RPCs (Fork, Checkpoint, ExtendLifetime, Budget) as Unimplemented | Task 1.3 service skeleton |
| Credited RNDADDENTROPY reseed, fail-closed | Tasks 1.2 (sys/entropy.rs) and 3.1 |
| CLOCK_REALTIME step with 500ms threshold | Tasks 1.2 (sys/clock.rs) and 3.2 |
| SIGUSR2 to userspace processes | Tasks 1.2 (sys/signal.rs) and 3.5 |
| eth0 network reconfiguration | Task 3.3 |
| Per-fork volume mounts | Task 3.4 |
| PID-1 init (proc/sys/dev/tmp/run, /workspace, hostname) | Task 1.3 init/ |
| gRPC over vsock AgentGRPCPort=53 | Task 1.3 main.rs |
| tonic-build from proto/sandbox/v1/sandbox.proto | Task 0.1 build.rs |
| `#![deny(unsafe_code)]` + sys/ only unsafe | Tasks 0.1, 1.2 |
| No panics on request path (unwrap/expect/panic denied) | Task 0.1 lib.rs |
| Secret values never logged; test enforced | Tasks 1.1 (env.rs comments) and 5.1 |
| rust-toolchain.toml + edition 2024 + musl static build | Tasks 0.1, 5.3 |
| cargo-deny + cargo-audit + miri over sys/ | Tasks 0.1, 1.2 |
| Agent conformance harness (gate 1) | Task 5.2 |
| Bare-metal bench gate on box1 + box2 (gate 2) | Task 5.4 |
| Fork-correctness CI suite (gate 3) | Tasks 3.1-3.5 + existing CI |
| Threat model + fork-correctness docs updated (gate 4) | Task 5.3 |
| Named human reviewer note | Task 5.3 + this document |
| Opt-in cutover, reversible | Task 5.5 |
| Punctuation: no em/en dashes | Global constraint (enforced by CI lint on commit messages and source) |
| DCO Signed-off-by on every commit | Every commit step uses `git commit -s` |

### Placeholder scan

No step says "TBD" or "add error handling" without showing the implementation. The following conditional gaps are explicitly noted with rationale, not elided:

1. **Task 4.1 (Control proto):** whether `proto/sandbox/controlv1/control.proto` exists in the Rust worktree cannot be determined from this plan; the task provides both branches (present / absent) with explicit handling for each.
2. **Task 2.8 (RunCode blocking_send note):** the note on `blocking_send` in async context is a concrete implementation warning, not a placeholder; the implementer must choose between `spawn_blocking` or collecting frames before sending.
3. **Task 5.4 (bench gate value):** "12 MiB placeholder pending first real measurement" is honest per operating principle 1; the plan says to update it from the actual measurement.
4. **Task 3.3 (rtnetlink crate choice):** "use the `rtnetlink` crate or raw Netlink sockets" is a deliberate choice left to the implementer because the right crate depends on what is available in the workspace's `deny.toml` license set; both achieve the same syscall sequence.

### Type/signature consistency

- `ConfiguredEnv::snapshot() -> HashMap<String,String>` is used in Task 1.1 (definition) and Task 2.1 `exec_handler` (consumption).
- `sys::entropy::reseed_crng(&[u8]) -> bool` is defined in Task 1.2 and consumed in Task 3.1 `fork/reseed.rs`.
- `sys::signal::signal_userspace() -> i32` is defined in Task 1.2 and consumed in Task 3.5 `fork/signal.rs`.
- `fork::handle_notify_forked(&NotifyForkedRequest) -> NotifyForkedResponse` is defined in Task 3.5 and consumed in Task 4.1 `service/control.rs`.
- `KernelManager` is declared in Task 2.8 `kernel/mod.rs` and consumed as `Arc<Mutex<KernelManager>>` in `SandboxService` (Task 1.3) and `service/runcode.rs` (Task 2.8).
- `AgentError` and `impl From<AgentError> for tonic::Status` are defined in Task 1.1 and used across all service/ modules.
- `path_allowed(p: &str) -> bool` is defined in Task 2.3 (`service/archive.rs`) and must also be available in Task 2.4 (`service/watch.rs`). Move it to a shared `service/util.rs` module and import from both.

### Missing doc delta

`docs/api/runtime-protocol.md` should be updated when the Rust agent graduates to default (cutover step 3) to note that the Rust binary serves the protocol. This is deferred to the graduation PR, not this plan.

### Named human reviewer requirement

The following files require a named human reviewer before any PR touching them is merged to main:
- `guest/agent-rs/src/sys/` (all files)
- `guest/agent-rs/src/fork/` (all files)
- `guest/agent-rs/src/init/mod.rs`
- `guest/agent-rs/src/main.rs`

This matches the CLAUDE.md security-sensitive path rule for `guest/agent`.
