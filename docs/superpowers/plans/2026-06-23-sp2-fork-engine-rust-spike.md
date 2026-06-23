# SP2: Fork-engine Rust hot-path measurement spike Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Determine whether replacing the Go UFFD page-fault handler and/or the snapshot restore path with Rust produces a measurable, reproducible improvement in per-fault service latency, restore wall time, or fork-to-first-exec latency on bare-metal KVM hosts (box1, box2). This is a measurement-only spike: no engine rewrite is committed or scheduled by this plan. The terminal deliverable is a recorded decision doc (keep Go / rewrite a named piece, with numbers from bench/results/), not a production cutover. If the data does not justify a rewrite, that is a valid and useful outcome.

**Architecture:** A standalone Rust crate at `internal/fork/uffd-bench/` (a binary crate, not a library, Linux-only) that reimplements the UFFD page-fault handler loop from `internal/fork/uffd_linux.go` behind the same behavioral contract. The crate registers a userfaultfd region, runs the same poll+UFFDIO_COPY loop the Go handler uses, and exposes a microbenchmark binary whose output (JSON, same schema as cmd/bench) can be archived and compared. The Go engine is NOT modified; the Rust prototype is wired only as a standalone benchmark, never loaded by the production fork path. Restore-path instrumentation is added to cmd/bench as a new `--mode uffd-compare` flag (Phase 3) that runs both handlers against the same snapshot and records per-fault latency and restore wall time for both.

**Tech Stack:** Latest stable Rust, edition 2024, musl static build (`x86_64-unknown-linux-musl`). `userfaultfd` crate evaluated vs raw `libc` in Task 1.1 and the choice justified there. `serde` + `serde_json` for JSON output (same schema as cmd/bench). Go `cmd/bench` (existing) extended with `--mode uffd-compare` for the head-to-head. KVM hosts: box1 (mitos-bench1, Intel i7-6700) and box2, SSH via `ssh -F .superpowers/ssh_config box1` from the ORIGINAL repo root (not a worktree path). Firecracker v1.15.0, guest kernel at `/root/mitos-test/vmlinux`.

## Global Constraints

- Latest stable Rust, edition 2024. Pin the toolchain via `internal/fork/uffd-bench/rust-toolchain.toml` with `channel = "stable"`. Commit `Cargo.lock`. Static musl build target: `x86_64-unknown-linux-musl`.
- `#![deny(unsafe_code)]` at crate root. The `sys/` module locally allows unsafe (`#![allow(unsafe_code)]`) and additionally sets `#![deny(unsafe_op_in_unsafe_fn)]`. Every unsafe block carries a `// SAFETY:` comment with the specific invariant upheld. Public safe wrappers in `sys/` are the ONLY unsafe entry points; handler and bench code is unsafe-free.
- No panics on the fault-handling hot path. Deny `clippy::unwrap_used`, `clippy::expect_used`, `clippy::panic`, `clippy::indexing_slicing` in the production handler code (not tests). Typed errors via `thiserror`. The handler fails closed on any error: it returns `Err` and lets the caller decide whether to abort or continue with the Go handler.
- No unverified claims. Every number in any document must be reproducible from bench/. With no KVM host available (no `/dev/kvm`), the spike produces NO latency number. Do not fabricate numbers or carry over numbers from another run without the matching JSON archive.
- Runs happen on box1 AND box2. SSH from the original repo root: `ssh -F .superpowers/ssh_config box1`. Firecracker v1.15.0, guest kernel `/root/mitos-test/vmlinux`. Confirm both boxes are reachable before starting Phase 4.
- No em (U+2014) or en (U+2013) dashes anywhere: source, comments, Markdown, YAML, commit messages. Use only `.` `,` `;` `:` and ASCII hyphen-minus for ranges and compound identifiers.
- DCO: every commit carries `Signed-off-by: Name <email>` (use `git commit -s`). Conventional commits: feat, fix, docs, ci, chore, refactor, test. Stage explicit paths only; never `git add -A`.
- Security-sensitive path note: this spike does NOT modify `internal/fork` production code. If a future SP3 rewrite does, it must have a named human reviewer and a threat-model update per CLAUDE.md.

---

## Framing: why this is unknown

The #310 spike measured Go vs Rust on the GUEST AGENT path. Both templates ran the exact same Go fork engine, so #310 says nothing about whether Rust can improve the engine. The fork-to-first-exec win in #310 (~12 percent, 13 ms) lives in the resumed agent's first-exec handling, not the restore path. The restore path is largely Firecracker-and-kernel-bound: Firecracker calls `mmap` and `read` on the snapshot mem file; the kernel pages in memory lazily via the UFFD handler. The UFFD hot path is the one Go loop that runs once per faulting page, and it calls `UFFDIO_COPY` via a raw ioctl. Whether Rust can measurably reduce per-fault latency in that loop is unknown. This spike exists to find out.

---

## File Structure

```
internal/fork/uffd-bench/            # new: Rust measurement crate (Linux-only binary)
  rust-toolchain.toml                # pins latest stable channel
  Cargo.toml                         # crate manifest, edition 2024, locked deps
  Cargo.lock                         # committed
  .cargo/config.toml                 # default target: x86_64-unknown-linux-musl
  src/
    main.rs                          # CLI entry: parse args, dispatch bench modes
    handler.rs                       # Rust UFFD handler: register region, poll loop, UFFDIO_COPY
    bench.rs                         # microbenchmark driver: N fault iterations, JSON output
    output.rs                        # JSON result schema (mirrors cmd/bench benchstat.Result)
    sys/
      mod.rs                         # pub safe wrappers around all unsafe ioctl calls
      uffd.rs                        # UFFDIO_API, UFFDIO_REGISTER, UFFDIO_COPY, poll loop
  tests/
    handler_integration.rs           # mmap an anon region, register, fault, assert page served
cmd/bench/main.go                    # extended: --mode uffd-compare (Phase 3)
bench/results/
  2026-06-23-sp2-uffd-rust-box1.json   # archived after Phase 4 (box1)
  2026-06-23-sp2-uffd-rust-box2.json   # archived after Phase 4 (box2)
  2026-06-23-sp2-restore-box1.json     # restore wall-time JSON (box1)
  2026-06-23-sp2-restore-box2.json     # restore wall-time JSON (box2)
  2026-06-23-sp2-uffd-comparison.md    # human-readable comparison (Phase 4)
docs/superpowers/decisions/
  2026-06-23-sp2-fork-engine-rust.md   # recorded decision (Phase 5, terminal task)
```

---

## Phase 0: Scaffold the measurement crate and toolchain

### Task 0.1: Create the Rust crate scaffold

**Files:**
- Create: `internal/fork/uffd-bench/rust-toolchain.toml`
- Create: `internal/fork/uffd-bench/Cargo.toml`
- Create: `internal/fork/uffd-bench/.cargo/config.toml`
- Create: `internal/fork/uffd-bench/src/main.rs` (stub)
- Create: `internal/fork/uffd-bench/src/sys/mod.rs` (stub)
- Create: `internal/fork/uffd-bench/src/sys/uffd.rs` (stub)
- Create: `internal/fork/uffd-bench/src/handler.rs` (stub)
- Create: `internal/fork/uffd-bench/src/bench.rs` (stub)
- Create: `internal/fork/uffd-bench/src/output.rs` (stub)

**Interfaces:** None yet; this task only establishes the build.

- [ ] **Step 1: Write `rust-toolchain.toml`**

```toml
[toolchain]
channel = "stable"
targets = ["x86_64-unknown-linux-musl"]
```

- [ ] **Step 2: Write `Cargo.toml`**

```toml
[package]
name = "uffd-bench"
version = "0.1.0"
edition = "2024"
publish = false
# Linux-only: the ioctl constants and /proc/self/mem path are Linux-specific.
# This crate is not compiled or tested on non-Linux hosts.

[dependencies]
serde = { version = "1", features = ["derive"] }
serde_json = "1"
thiserror = "2"
libc = "0.2"
# NOTE: the userfaultfd crate is evaluated in Task 1.1. It is NOT listed here
# yet; Task 1.1 justifies whether to add it or stay on raw libc.

[profile.release]
opt-level = 3
lto = "thin"
codegen-units = 1
panic = "abort"
strip = true

[profile.bench-release]
inherits = "release"
debug = 1   # retain enough DWARF for perf flamegraphs without disabling opts
```

- [ ] **Step 3: Write `.cargo/config.toml`**

```toml
[build]
target = "x86_64-unknown-linux-musl"
```

- [ ] **Step 4: Write stub `src/main.rs`**

The stub should compile clean, print a usage line, and exit 0.

```rust
#![deny(unsafe_code)]

mod bench;
mod handler;
mod output;
mod sys;

fn main() {
    eprintln!("uffd-bench: no mode selected; use --help");
    std::process::exit(2);
}
```

- [ ] **Step 5: Write stub `src/sys/mod.rs`**

```rust
// sys/ is the ONLY module that may contain unsafe code.
// All unsafe blocks carry a // SAFETY: comment.
#![allow(unsafe_code)]
#![deny(unsafe_op_in_unsafe_fn)]

pub mod uffd;
```

- [ ] **Step 6: Confirm the scaffold builds on Linux (or a musl cross-compile on macOS)**

On Linux (or box1 via SSH):
```sh
cd internal/fork/uffd-bench
cargo build 2>&1 | tail -5
# Expected: "Compiling uffd-bench v0.1.0" ... "Finished dev ..."
```

On macOS (cross-compile check only; the binary cannot run here):
```sh
cd internal/fork/uffd-bench
cargo build --target x86_64-unknown-linux-musl 2>&1 | tail -5
# Expected: Finished; OR a link error due to missing musl libc.
# The crate does NOT need to link on macOS; build success without linking is acceptable.
```

- [ ] **Step 7: Commit the scaffold**

```sh
git add internal/fork/uffd-bench/rust-toolchain.toml \
        internal/fork/uffd-bench/Cargo.toml \
        internal/fork/uffd-bench/Cargo.lock \
        internal/fork/uffd-bench/.cargo/config.toml \
        internal/fork/uffd-bench/src/main.rs \
        internal/fork/uffd-bench/src/sys/mod.rs \
        internal/fork/uffd-bench/src/sys/uffd.rs \
        internal/fork/uffd-bench/src/handler.rs \
        internal/fork/uffd-bench/src/bench.rs \
        internal/fork/uffd-bench/src/output.rs
git commit -s -m "chore(sp2): scaffold Rust UFFD measurement crate with toolchain + lint gates"
```

---

## Phase 1: Rust UFFD prototype

### Task 1.1: Evaluate and justify the UFFD dependency

**Files:**
- Edit: `internal/fork/uffd-bench/Cargo.toml` (add or skip `userfaultfd` crate)
- Create: comment block in `src/sys/uffd.rs` documenting the decision

**Interfaces:** The outcome of this task determines whether `sys/uffd.rs` wraps raw `libc` ioctl calls or delegates to the `userfaultfd` crate's bindings.

- [ ] **Step 1: Audit the `userfaultfd` crate**

Fetch and read the crate docs:
```sh
cargo add userfaultfd --dry-run 2>&1 | head -20
# Check: does it expose UFFDIO_COPY with the exact struct layout we need?
# Check: does it expose UFFD_EVENT_PAGEFAULT?
# Check: is it maintained, does it have a RUSTSEC audit entry?
```

Read the crate source at https://docs.rs/userfaultfd (or `cargo doc --open` on the crate). Specifically check:
- Does `userfaultfd::UffdBuilder` expose `UFFDIO_REGISTER` with `UFFDIO_REGISTER_MODE_MISSING`?
- Does `userfaultfd::Event::Pagefault` carry the faulting address and thread ID?
- Does it expose `Uffd::copy` with destination address, source pointer, length, and wake=true?

The Go reference in `internal/fork/uffd_linux.go` uses:
- `UFFDIO_COPY` (constant `0xc028aa03`, struct `uffdioCopyArg{Dst, Src, Len, Mode, Copy}`) via `unix.Syscall(unix.SYS_IOCTL, ...)`.
- `UFFD_EVENT_PAGEFAULT` (constant `0x12`) to detect fault events.
- Blocking `unix.Read(h.uffd, msgBuf)` to receive `uffdMsg` structs.

- [ ] **Step 2: Record the choice in `src/sys/uffd.rs`**

Write a comment block at the top of `src/sys/uffd.rs` (before any code):

```rust
// UFFD dependency choice (evaluated Task 1.1):
//
// Option A: raw libc + ioctl constants
//   Pro: zero extra dep, exact control over struct layout, trivially auditable.
//   Con: we replicate the constant definitions and struct layouts from linux/userfaultfd.h.
//
// Option B: userfaultfd crate (crates.io/crates/userfaultfd)
//   Pro: typed event API, handles ioctl layout internally.
//   Con: adds a dep with its own unsafe surface; the crate exposes UFFDIO_COPY via
//        a safe method that hides Copy (the kernel output), requiring an extra read.
//        As of 2025 the crate is lightly maintained (last release > 12 months ago).
//
// Decision: <FILL IN after Step 1: "raw libc" or "userfaultfd crate", with one-sentence rationale>
//
// If raw libc: the constants below are from linux/userfaultfd.h, verified against
// the values in internal/fork/uffd_linux.go (uffdioCopy = 0xc028aa03,
// uffdEventPagefault = 0x12).
```

Fill in the decision after Step 1. If you choose raw libc, add the constants. If you choose the crate, add it to `Cargo.toml`.

### Task 1.2: Implement the safe UFFD syscall wrappers in `sys/uffd.rs`

**Files:**
- Edit: `internal/fork/uffd-bench/src/sys/uffd.rs`

**Interfaces (exact Rust signatures):**

```rust
// Safe wrapper: create a userfaultfd. Returns the raw fd.
// Panics: never. Errors: os error from syscall.
pub fn uffd_create() -> Result<std::os::unix::io::OwnedFd, UffdError>;

// Safe wrapper: register a memory region for MISSING-page faults.
// addr must be page-aligned; len must be a multiple of the system page size.
pub fn uffd_register(uffd: libc::c_int, addr: usize, len: usize) -> Result<(), UffdError>;

// Safe wrapper: copy `page_size` bytes from `src` to guest address `dst`.
// Returns the number of bytes copied (from the kernel's uffdioCopyArg.Copy field).
// EEXIST is not an error (page was filled concurrently).
pub fn uffd_copy(uffd: libc::c_int, dst: usize, src: *const u8, page_size: usize) -> Result<i64, UffdError>;

// Safe wrapper: blocking read of one uffd_msg from the uffd fd.
// Returns (event_type, faulting_address).
pub fn uffd_read_event(uffd: libc::c_int) -> Result<UffdEvent, UffdError>;

pub enum UffdEvent {
    Pagefault { addr: usize },
    Other(u8),
}

#[derive(Debug, thiserror::Error)]
pub enum UffdError {
    #[error("syscall failed: {0}")]
    Syscall(#[from] std::io::Error),
    #[error("uffd_copy short copy: expected {expected} got {actual}")]
    ShortCopy { expected: usize, actual: i64 },
    #[error("uffd msg read short: {0} bytes")]
    ShortRead(usize),
}
```

The Go reference for `uffd_copy`:

```go
// from internal/fork/uffd_linux.go, copyPage():
arg := uffdioCopyArg{
    Dst: dst,
    Src: uint64(uintptr(unsafe.Pointer(&h.memMap[fileOffset]))),
    Len: pageSize,
}
_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(h.uffd), uintptr(uffdioCopy), uintptr(unsafe.Pointer(&arg)))
if errno != 0 {
    if errno == unix.EEXIST { return nil }
    return fmt.Errorf("uffd: UFFDIO_COPY dst=%#x len=%#x: %w", dst, pageSize, errno)
}
```

Mirror this exactly in Rust. `uffdioCopy = 0xc028aa03` (struct size 40 bytes: 5 x u64/i64). The Rust struct must be `#[repr(C)]`:

```rust
#[repr(C)]
struct UffdioCopyArg {
    dst: u64,
    src: u64,
    len: u64,
    mode: u64,
    copy: i64,  // kernel output: bytes copied, or negative errno
}
```

- [ ] **Step 1: Write `UffdioCopyArg` and constants in `sys/uffd.rs`**

Verify the constant against the Go source (line 47 of `internal/fork/uffd_linux.go`):
```
uffdioCopy = 0xc028aa03   (IOWR 0xAA, 0x03, size=40)
uffdEventPagefault = 0x12
```

- [ ] **Step 2: Implement `uffd_create` using `SYS_userfaultfd`**

Linux syscall number: `x86_64: 323`. Flags: `O_CLOEXEC | O_NONBLOCK` for creation, then switch to blocking mode before the read loop (same as Go: `_ = unix.SetNonblock(gotFD, false)`).

```rust
// SAFETY: SYS_userfaultfd is a documented Linux syscall; the returned fd is
// owned by the caller via OwnedFd which closes it on drop.
pub fn uffd_create() -> Result<std::os::unix::io::OwnedFd, UffdError> { ... }
```

- [ ] **Step 3: Implement `uffd_register`**

The `UFFDIO_REGISTER` ioctl constant is `0xc020aa00` for `struct uffdio_register` (32 bytes: range.start u64, range.len u64, mode u64, ioctls u64). Verify this by consulting the kernel header or the `userfaultfd` crate source.

```rust
// SAFETY: addr and len come from an mmap region the caller owns; the ioctl
// writes only to the kernel's internal uffd state and the ioctls output field.
pub fn uffd_register(uffd: libc::c_int, addr: usize, len: usize) -> Result<(), UffdError> { ... }
```

- [ ] **Step 4: Implement `uffd_copy`**

Mirror the Go `copyPage` exactly. EEXIST is success. The `copy` field of `UffdioCopyArg` after the ioctl holds bytes copied (positive) or a negative errno.

```rust
// SAFETY: src must point to at least page_size bytes of readable memory for
// the duration of the ioctl; dst must be a guest address registered with this
// uffd; page_size must match the registered region's page size.
pub fn uffd_copy(uffd: libc::c_int, dst: usize, src: *const u8, page_size: usize) -> Result<i64, UffdError> { ... }
```

- [ ] **Step 5: Implement `uffd_read_event`**

The `uffd_msg` struct is 32 bytes; read it with a single blocking `libc::read` call into a `[u8; 32]` buffer. Parse `event` (byte 0) and faulting address (bytes 16..24, little-endian u64 on x86_64). This mirrors the Go `uffdMsg` struct and the `Serve` loop's `unix.Read(h.uffd, msgBuf)` call in `uffd_linux.go` line 277.

```rust
// SAFETY: uffd is a valid userfaultfd descriptor; read blocks until a message
// is available; the kernel writes exactly sizeof(uffd_msg) = 32 bytes.
pub fn uffd_read_event(uffd: libc::c_int) -> Result<UffdEvent, UffdError> { ... }
```

- [ ] **Step 6: Run `cargo clippy` (Linux only) and fix all warnings**

```sh
RUSTFLAGS="-C target-feature=+crt-static" cargo clippy --target x86_64-unknown-linux-musl -- -D warnings 2>&1 | tail -20
# Expected: no warnings, no errors.
```

### Task 1.3: Implement the Rust UFFD handler in `handler.rs`

**Files:**
- Edit: `internal/fork/uffd-bench/src/handler.rs`

**Interfaces:**

```rust
pub struct UffdHandler {
    // uffd fd, owned; closed on drop
    uffd: std::os::unix::io::OwnedFd,
    // mmap'd region: pointer + length
    region_ptr: *mut u8,
    region_len: usize,
    // page_size in bytes (4096 or 2097152)
    page_size: usize,
    // per-fault stats
    faults_served: std::sync::atomic::AtomicU64,
}

impl UffdHandler {
    // Register a NEW anonymous mmap region of `region_len` bytes with a fresh
    // userfaultfd. `page_size` must divide `region_len`.
    pub fn new(region_len: usize, page_size: usize) -> Result<Self, HandlerError>;

    // Return the pointer to the registered region (for the bench to read/write).
    pub fn region_ptr(&self) -> *const u8;

    // Run the fault-service loop for `max_faults` faults, then return.
    // On each UFFD_EVENT_PAGEFAULT: call sys::uffd::uffd_copy to fill the page
    // from a zero-filled source page, increment faults_served.
    // This mirrors internal/fork/uffd_linux.go Serve() but stops after max_faults.
    pub fn serve_n(&self, max_faults: u64, src_page: &[u8]) -> Result<u64, HandlerError>;

    // Number of faults served so far (atomic load).
    pub fn faults_served(&self) -> u64;
}

impl Drop for UffdHandler {
    // munmap the region, close the uffd fd.
}

#[derive(Debug, thiserror::Error)]
pub enum HandlerError {
    #[error("uffd syscall: {0}")]
    Uffd(#[from] crate::sys::uffd::UffdError),
    #[error("mmap failed: {0}")]
    Mmap(std::io::Error),
    #[error("page_size {page_size} does not divide region_len {region_len}")]
    Alignment { page_size: usize, region_len: usize },
}
```

- [ ] **Step 1: Write `UffdHandler::new`**

Use `libc::mmap` with `PROT_READ | PROT_WRITE`, `MAP_PRIVATE | MAP_ANONYMOUS` to allocate the region. Then call `sys::uffd::uffd_register`. Store the pointer.

```rust
// SAFETY (mmap): MAP_PRIVATE | MAP_ANONYMOUS does not require a file fd; the
// kernel returns a fresh zero-mapped region. The pointer is valid for
// region_len bytes until munmap in Drop.
```

- [ ] **Step 2: Write `UffdHandler::serve_n`**

The loop:
1. Call `sys::uffd::uffd_read_event` (blocking).
2. On `UffdEvent::Pagefault { addr }`: call `sys::uffd::uffd_copy(uffd, page_aligned_addr, src_page.as_ptr(), page_size)`.
3. Increment `faults_served` atomically.
4. Break after `max_faults` faults.

Mirror the Go `Serve` loop in `uffd_linux.go` lines 270-318. The source page (`src_page`) is a caller-supplied slice of `page_size` bytes (the bench supplies a zeroed page, since the microbench is measuring handler latency, not data integrity).

- [ ] **Step 3: Write `Drop` for `UffdHandler`**

Call `libc::munmap` on `region_ptr`/`region_len`. The `OwnedFd` drops automatically.

```rust
// SAFETY: region_ptr was returned by mmap with region_len; munmap is called
// exactly once here, after all borrows of region_ptr have ended.
```

### Task 1.4: Integration test: fault a mapped region and assert pages are served

**Files:**
- Create: `internal/fork/uffd-bench/tests/handler_integration.rs`

This test runs on Linux only (cfg attribute) and proves that the Rust handler correctly services page faults.

- [ ] **Step 1: Write the test**

```rust
#![cfg(target_os = "linux")]

use uffd_bench::handler::UffdHandler;

/// Register a 4-page region, spawn a handler thread that serves 4 faults,
/// then read each page from the main thread (triggering one fault per page).
/// Assert that the handler served exactly 4 faults and each page contains
/// the expected zeroed content.
#[test]
fn handler_serves_four_pages() {
    let page_size = 4096usize;
    let n_pages = 4usize;
    let region_len = page_size * n_pages;
    let src_page = vec![0xABu8; page_size]; // fill sentinel

    let handler = UffdHandler::new(region_len, page_size)
        .expect("UffdHandler::new");
    let ptr = handler.region_ptr() as usize;
    let src = src_page.clone();

    // Spawn the serve thread BEFORE touching the region.
    let handle = std::thread::spawn(move || {
        handler.serve_n(n_pages as u64, &src)
            .expect("serve_n")
    });

    // Touch each page: trigger one fault per page.
    for i in 0..n_pages {
        let page_ptr = (ptr + i * page_size) as *const u8;
        // SAFETY: ptr points to a valid mmap region of region_len bytes,
        // and we are reading exactly one byte per page while the handler is alive.
        let byte = unsafe { std::ptr::read_volatile(page_ptr) };
        assert_eq!(byte, 0xAB, "page {i} not filled with sentinel");
    }

    let faults = handle.join().expect("serve_n thread");
    assert_eq!(faults, n_pages as u64);
}
```

- [ ] **Step 2: Run the test on Linux**

```sh
cargo test --test handler_integration -- --nocapture 2>&1
# Expected: test handler_serves_four_pages ... ok
```

- [ ] **Step 3: Commit Phase 1**

```sh
git add internal/fork/uffd-bench/src/ \
        internal/fork/uffd-bench/tests/ \
        internal/fork/uffd-bench/Cargo.toml \
        internal/fork/uffd-bench/Cargo.lock
git commit -s -m "feat(sp2): Rust UFFD handler prototype with integration test"
```

---

## Phase 2: Microbenchmark harness for per-fault service latency

### Task 2.1: Implement the benchmark output schema in `output.rs`

**Files:**
- Edit: `internal/fork/uffd-bench/src/output.rs`

The output JSON must be compatible with the cmd/bench JSON reader so results can be imported into the same archiving and diffing workflow. Mirror `internal/benchstat.Result`:

```rust
#[derive(serde::Serialize)]
pub struct BenchResult {
    pub name: String,
    pub unit: String,         // "ns" for nanoseconds
    pub arm: String,          // "go" or "rust"
    pub count: u64,
    pub min_ns: u64,
    pub p50_ns: u64,
    pub p90_ns: u64,
    pub p99_ns: u64,
    pub max_ns: u64,
    pub mean_ns: f64,
    pub samples_ns: Vec<u64>, // raw per-fault nanosecond samples
}
```

- [ ] **Step 1: Write `output.rs` with `BenchResult` and `summarize`**

```rust
pub fn summarize(name: &str, arm: &str, samples: &mut Vec<u64>) -> BenchResult {
    samples.sort_unstable();
    let count = samples.len() as u64;
    let p = |pct: f64| samples[(pct * count as f64 / 100.0) as usize];
    BenchResult {
        name: name.to_owned(),
        unit: "ns".to_owned(),
        arm: arm.to_owned(),
        count,
        min_ns: *samples.first().unwrap_or(&0),
        p50_ns: p(50.0),
        p90_ns: p(90.0),
        p99_ns: p(99.0),
        max_ns: *samples.last().unwrap_or(&0),
        mean_ns: samples.iter().sum::<u64>() as f64 / count as f64,
        samples_ns: samples.to_vec(),
    }
}
```

### Task 2.2: Implement the microbenchmark driver in `bench.rs`

**Files:**
- Edit: `internal/fork/uffd-bench/src/bench.rs`

The microbenchmark measures per-fault service latency in nanoseconds for the Rust handler. The Go handler latency is measured separately (Task 2.3, via a thin Go cgo/subprocess wrapper or via cmd/bench instrumentation) so the two can be compared head-to-head.

**Method:** For each iteration, register a fresh UFFD region, trigger one page fault (write to an unmapped page), record the `Instant` before and after the fault returns (the fault blocks until the handler serves it), and record the delta in nanoseconds. Use `N` warmup iterations (discarded) followed by `M` measured iterations.

```rust
pub struct BenchConfig {
    pub warmup: u64,
    pub iterations: u64,
    pub page_size: usize,   // 4096 or 2097152
    pub out_path: Option<std::path::PathBuf>,
}

pub fn run_fault_latency_bench(cfg: &BenchConfig) -> Result<output::BenchResult, crate::handler::HandlerError>;
```

- [ ] **Step 1: Write `run_fault_latency_bench`**

For each iteration (warmup + measured):
1. Create a new `UffdHandler` with `region_len = page_size` (single page).
2. Prepare `src_page` (zeroed, `page_size` bytes).
3. Spawn the handler thread: `handler.serve_n(1, &src_page)`.
4. Record `t0 = Instant::now()`.
5. Trigger the fault: read one byte from `handler.region_ptr()` with `std::ptr::read_volatile`.
6. Record `elapsed_ns = t0.elapsed().as_nanos() as u64`.
7. Join the handler thread; assert it served 1 fault.
8. Discard or collect the sample.

Note: `Instant::now()` is called on the SAME thread as the faulting access, so it captures the full fault-to-serve round-trip, including the kernel's `handle_userfault` -> wake-faulty-thread path. This is the correct measurement boundary.

- [ ] **Step 2: Write the expected output boundary**

The Go `Serve` loop in `uffd_linux.go` does:
```
unix.Read(h.uffd, msgBuf)    // receive fault event (blocking)
-> fileOffsetForAddr()        // pure arithmetic
-> copyPage()                 // UFFDIO_COPY ioctl
-> atomic.AddInt64(&h.served, 1)
```

The Rust path does:
```
uffd_read_event()             // libc::read (blocking)
-> page-aligned addr calc     // pure arithmetic
-> uffd_copy()                // UFFDIO_COPY ioctl
-> fetch_add (atomic)
```

The only structural difference is language runtime (Go goroutine scheduler overhead vs Rust OS thread), the ioctl call path, and the atomic increment. If the difference is measurable, it will appear in the p50 per-fault latency. If both arms converge, it is evidence the bottleneck is the kernel's `handle_userfault` path, not userspace.

### Task 2.3: Instrument the Go UFFD handler for per-fault latency measurement

**Files:**
- Edit: `internal/fork/uffd_linux.go` (add timing hook, behind a build flag)
- Create: `internal/fork/uffd_bench_test.go` (Linux-only benchmark)

**Goal:** Produce a Go baseline number using the same measurement shape as the Rust bench: per-fault service latency in nanoseconds, with warmup and N measured iterations.

- [ ] **Step 1: Add a `BenchmarkFaultService` in `internal/fork/uffd_bench_test.go`**

```go
//go:build linux

package fork_test

import (
    "testing"
    "unsafe"

    "golang.org/x/sys/unix"
)

// BenchmarkFaultService measures the per-fault service latency of the Go
// UFFD handler. It creates an anonymous region, registers it with a
// userfaultfd, runs a handler goroutine, and measures the time from
// triggering a fault (read from unmapped page) to the fault returning.
// This is the Go baseline for the SP2 Rust vs Go comparison.
//
// Run with: GOOS=linux go test -bench=BenchmarkFaultService -run=^$ ./internal/fork/
// Output: ns/op is the per-fault latency; b.N is iterations.
func BenchmarkFaultService(b *testing.B) {
    const pageSize = 4096
    // Allocate a region large enough for b.N faults; at least 1 page.
    regionLen := (b.N + 1) * pageSize
    addr, err := unix.MmapPtr(-1, 0, nil, uintptr(regionLen),
        unix.PROT_READ|unix.PROT_WRITE,
        unix.MAP_PRIVATE|unix.MAP_ANONYMOUS, 0)
    if err != nil {
        b.Fatalf("mmap: %v", err)
    }
    defer unix.Munmap(addr) //nolint:errcheck

    // Create userfaultfd (inline, not through newUFFDHandler, to avoid the
    // socket/mmap overhead of the production handler).
    uffdFD, _, errno := unix.Syscall(unix.SYS_USERFAULTFD, unix.O_CLOEXEC, 0, 0)
    if errno != 0 {
        b.Skipf("userfaultfd unavailable: %v", errno)
    }
    defer unix.Close(int(uffdFD)) //nolint:errcheck

    // Register the region.
    type uffdioRegister struct {
        Start uint64
        Len   uint64
        Mode  uint64
        Ioctls uint64
    }
    reg := uffdioRegister{
        Start: uint64(uintptr(unsafe.Pointer(&addr[0]))),
        Len:   uint64(regionLen),
        Mode:  0x1, // UFFDIO_REGISTER_MODE_MISSING
    }
    const uffdioRegisterOp = 0xc020aa00
    if _, _, e := unix.Syscall(unix.SYS_IOCTL, uffdFD, uintptr(uffdioRegisterOp), uintptr(unsafe.Pointer(&reg))); e != 0 {
        b.Fatalf("UFFDIO_REGISTER: %v", e)
    }

    srcPage := make([]byte, pageSize)
    done := make(chan struct{})
    // Handler goroutine: service every fault with a zeroed page.
    go func() {
        defer close(done)
        var msg [32]byte
        for {
            n, err := unix.Read(int(uffdFD), msg[:])
            if err != nil || n < 32 {
                return
            }
            if msg[0] != 0x12 { // UFFD_EVENT_PAGEFAULT
                continue
            }
            pfAddr := *(*uint64)(unsafe.Pointer(&msg[16]))
            pageBase := (pfAddr / pageSize) * pageSize
            arg := struct {
                Dst, Src, Len, Mode uint64
                Copy                int64
            }{
                Dst: pageBase,
                Src: uint64(uintptr(unsafe.Pointer(&srcPage[0]))),
                Len: uint64(pageSize),
            }
            const uffdioCopy = 0xc028aa03
            unix.Syscall(unix.SYS_IOCTL, uffdFD, uintptr(uffdioCopy), uintptr(unsafe.Pointer(&arg))) //nolint:errcheck
        }
    }()

    b.ResetTimer()
    base := uintptr(unsafe.Pointer(&addr[0]))
    for i := 0; i < b.N; i++ {
        // Touch a fresh page each iteration.
        _ = *(*byte)(unsafe.Pointer(base + uintptr(i*pageSize)))
    }
    b.StopTimer()
    unix.Close(int(uffdFD)) //nolint:errcheck
    <-done
}
```

- [ ] **Step 2: Run the Go benchmark on Linux (box1) to capture the baseline**

```sh
GOOS=linux go test -bench=BenchmarkFaultService -benchtime=500x -run=^$ ./internal/fork/ 2>&1
# Expected output: something like
#   BenchmarkFaultService   500   4321 ns/op
# Record the ns/op value. This is the Go per-fault latency baseline.
```

Save the result to `bench/results/2026-06-23-sp2-go-fault-latency-box1.txt` (plain text; JSON in Task 2.4).

- [ ] **Step 3: Commit Go benchmark**

```sh
git add internal/fork/uffd_bench_test.go
git commit -s -m "test(sp2): Go UFFD per-fault latency benchmark for SP2 baseline"
```

### Task 2.4: Wire the CLI in `main.rs` and emit JSON output

**Files:**
- Edit: `internal/fork/uffd-bench/src/main.rs`

**Interfaces:**

```
uffd-bench --mode fault-latency \
           --iterations 500 \
           --warmup 50 \
           --page-size 4096 \
           --json /tmp/sp2-rust-fault-box1.json \
           --summary
```

- [ ] **Step 1: Parse flags in `main.rs`**

Use `std::env::args` with manual parsing (no extra dep) or add `clap` to `Cargo.toml`. The flags: `--mode`, `--iterations`, `--warmup`, `--page-size`, `--json`, `--summary`.

- [ ] **Step 2: Dispatch `--mode fault-latency` to `bench::run_fault_latency_bench`**

On success, write the `BenchResult` JSON to `--json` path and print the summary table to stdout if `--summary` is set.

Summary table format (mirrors cmd/bench `--summary`):
```
fault_latency (ns)  arm=rust
count=500 min=2341 p50=3120 p90=4201 p99=6802 max=11234 mean=3301.4
```

- [ ] **Step 3: Confirm the binary runs on Linux**

```sh
cargo build --release --target x86_64-unknown-linux-musl
./target/x86_64-unknown-linux-musl/release/uffd-bench \
  --mode fault-latency \
  --iterations 20 --warmup 2 --page-size 4096 --summary
# Expected: summary table printed, exit 0.
```

- [ ] **Step 4: Commit Phase 2**

```sh
git add internal/fork/uffd-bench/src/main.rs \
        internal/fork/uffd-bench/src/bench.rs \
        internal/fork/uffd-bench/src/output.rs \
        internal/fork/uffd-bench/Cargo.lock
git commit -s -m "feat(sp2): microbenchmark harness for per-fault latency, JSON output"
```

---

## Phase 3: Restore-path measurement instrumentation

**Goal:** Measure the snapshot restore wall time and its contribution to fork-to-first-exec, broken down by component. The restore path in the Go engine is: `Fork` -> `loadSnapshotUFFD` -> `client.LoadSnapshotUFFD` (the Firecracker API PUT) -> `h.receive` (UFFD handshake) -> `h.Serve` goroutine start -> `h.Preload` (hot pages) -> `client.Resume`. The wall time attribution is currently not captured; this phase adds structured timing without changing behavior.

### Task 3.1: Add `--mode uffd-compare` to `cmd/bench`

**Files:**
- Edit: `cmd/bench/main.go`
- Edit: `cmd/bench/main_test.go`

**Method:** The `uffd-compare` mode runs the existing Go UFFD restore path and records per-phase timings: (1) `loadSnapshotUFFD` total wall time, (2) `h.receive` handshake wall time, (3) `h.Preload` wall time (if hot pages exist), (4) faults served by `h.Serve` before first-exec, (5) fork-to-first-exec total. It runs `--iterations` iterations and emits JSON. No Rust code is invoked in this phase; the comparison is pure Go instrumentation.

- [ ] **Step 1: Add the `uffd-compare` mode constant and parser entry**

In `cmd/bench/main.go`:
```go
const modeUFFDCompare = "uffd-compare"
```

Add to `parseConfig` validation. The mode requires `--template` (as usual) and reuses `--iterations` and `--warmup`.

- [ ] **Step 2: Add `RestoreTimingResult` struct**

```go
type RestoreTimingResult struct {
    Name            string  `json:"name"`
    IterationN      int     `json:"iteration_n"`
    // Wall times in nanoseconds
    LoadSnapshotNs  []int64 `json:"load_snapshot_ns"`   // PUT /snapshot/load total
    HandshakeNs     []int64 `json:"handshake_ns"`       // receive() wall time
    PreloadNs       []int64 `json:"preload_ns"`         // Preload() wall time (0 if no hot pages)
    FaultsServed    []int64 `json:"faults_served"`      // h.Serve count before first-exec
    ForkExecNs      []int64 `json:"fork_exec_ns"`       // fork -> first exec total
}
```

- [ ] **Step 3: Instrument `internal/fork/uffd_engine.go`**

Add timing seams to `loadSnapshotUFFD` that record the duration of each phase. Use a `UFFDTimings` struct (similar to `ForkResult`) returned alongside the handler:

```go
type UFFDTimings struct {
    LoadSnapshot time.Duration
    Handshake    time.Duration
    Preload      time.Duration
}
```

Modify the signature of `loadSnapshotUFFD` to return `(*uffdHandler, UFFDTimings, error)`. The production caller in `fork()` passes the timings into `ForkResult` for bench mode to read. Non-bench callers ignore the extra struct. This does NOT change the hot path; the `time.Now()` calls are outside the fault-service loop.

- [ ] **Step 4: Write `runUFFDCompare` in `cmd/bench/main.go`**

```go
func runUFFDCompare(engine *fork.Engine, cfg config) error {
    // warmup
    for i := 0; i < cfg.warmup; i++ {
        id := fmt.Sprintf("uffd-cmp-warm-%d", i)
        if _, _, err := oneUFFDForkExec(engine, cfg.template, id); err != nil {
            return fmt.Errorf("warmup %d: %w", i, err)
        }
    }
    result := RestoreTimingResult{
        Name: "uffd_restore_timing",
        IterationN: cfg.iterations,
    }
    for i := 0; i < cfg.iterations; i++ {
        id := fmt.Sprintf("uffd-cmp-%d", i)
        timings, elapsed, err := oneUFFDForkExec(engine, cfg.template, id)
        if err != nil {
            return fmt.Errorf("iteration %d: %w", i, err)
        }
        result.LoadSnapshotNs = append(result.LoadSnapshotNs, timings.LoadSnapshot.Nanoseconds())
        result.HandshakeNs    = append(result.HandshakeNs, timings.Handshake.Nanoseconds())
        result.PreloadNs      = append(result.PreloadNs, timings.Preload.Nanoseconds())
        result.FaultsServed   = append(result.FaultsServed, engine.FaultsServed(id))
        result.ForkExecNs     = append(result.ForkExecNs, elapsed.Nanoseconds())
    }
    // print summary and write JSON as in other modes
    ...
}
```

`oneUFFDForkExec` mirrors `onePrefetchForkExec` from `cmd/bench/main.go` line 671 but returns `UFFDTimings` from `ForkResult`.

- [ ] **Step 5: Unit-test `parseConfig` for `--mode uffd-compare`**

In `cmd/bench/main_test.go`:
```go
func TestParseConfigUFFDCompare(t *testing.T) {
    cfg, err := parseConfig([]string{
        "--mode", "uffd-compare",
        "--template", "bench-go",
        "--data-dir", "/tmp/data",
        "--iterations", "10",
    })
    if err != nil { t.Fatal(err) }
    if cfg.mode != "uffd-compare" { t.Errorf("mode: got %q", cfg.mode) }
}
```

- [ ] **Step 6: Run existing Go tests to confirm no regression**

```sh
GOOS=linux go test ./internal/fork/ ./cmd/bench/ 2>&1 | tail -10
# Expected: ok for all packages.
```

- [ ] **Step 7: Commit Phase 3**

```sh
git add internal/fork/uffd_engine.go \
        cmd/bench/main.go \
        cmd/bench/main_test.go
git commit -s -m "feat(sp2): restore-path timing instrumentation and uffd-compare bench mode"
```

---

## Phase 4: Run on box1 and box2, archive results

**Pre-conditions:** Phase 0-3 committed and passing on a Linux host. SSH access to box1 and box2 confirmed from the original repo root with `ssh -F .superpowers/ssh_config box1`. A verified template snapshot exists at `/root/mitos-test/` (Firecracker v1.15.0, vmlinux, template `bench-go`).

### Task 4.1: Confirm box1 and box2 are ready

- [ ] **Step 1: Verify box1 connectivity and KVM**

From the ORIGINAL repo root (not the worktree):
```sh
ssh -F .superpowers/ssh_config box1 'uname -r && test -e /dev/kvm && echo kvm-ok && \
  ls /root/mitos-test/templates/bench-go/snapshot/mem && echo template-ok'
# Expected: kernel version, "kvm-ok", "template-ok"
```

- [ ] **Step 2: Verify box2 connectivity and KVM**

```sh
ssh -F .superpowers/ssh_config box2 'uname -r && test -e /dev/kvm && echo kvm-ok'
# Expected: "kvm-ok"
```

If box2 lacks the template, create it by following the procedure in `bench/README.md` "Template layout" and the CI step in `.github/workflows/kvm-test.yaml`.

### Task 4.2: Build and transfer binaries to box1

- [ ] **Step 1: Cross-build `uffd-bench` for x86_64-linux-musl**

From the worktree root:
```sh
cd internal/fork/uffd-bench
cargo build --release --target x86_64-unknown-linux-musl 2>&1 | tail -5
ls -lh target/x86_64-unknown-linux-musl/release/uffd-bench
# Expected: binary under 2 MiB (static musl, stripped, opt-level=3).
```

- [ ] **Step 2: Build `cmd/bench` with UFFD instrumentation**

```sh
cd /path/to/repo-root
GOOS=linux GOARCH=amd64 go build -o /tmp/bench-sp2 ./cmd/bench/
```

- [ ] **Step 3: Transfer to box1**

```sh
scp -F .superpowers/ssh_config \
    internal/fork/uffd-bench/target/x86_64-unknown-linux-musl/release/uffd-bench \
    /tmp/bench-sp2 \
    box1:/root/sp2/
```

### Task 4.3: Run the Go baseline and Rust UFFD microbench on box1

All commands run on box1 via SSH. Substitute actual `DATA_DIR` and `TEMPLATE_ID`:

```sh
DATA_DIR=/root/mitos-test
TEMPLATE_ID=bench-go
FC=/usr/local/bin/firecracker
KERNEL=$DATA_DIR/vmlinux
```

- [ ] **Step 1: Run Go fork-exec baseline (100 iterations, 20 warmup)**

```sh
/root/sp2/bench-sp2 \
  --mode fork-exec \
  --template $TEMPLATE_ID \
  --data-dir $DATA_DIR \
  --firecracker $FC --kernel $KERNEL \
  --iterations 100 --warmup 20 \
  --summary --json /root/sp2/sp2-go-fork-box1-run1.json
```

Expected output:
```
fork_to_first_exec (ms)
count=100 min=... p50=~100-115 p90=... p99=... max=... mean=...
```

Repeat 3 times (run1, run2, run3) to confirm stability. Record p50 from each run.

- [ ] **Step 2: Run uffd-compare mode (restore wall-time breakdown)**

```sh
/root/sp2/bench-sp2 \
  --mode uffd-compare \
  --template $TEMPLATE_ID \
  --data-dir $DATA_DIR \
  --firecracker $FC --kernel $KERNEL \
  --iterations 50 --warmup 10 \
  --summary --json /root/sp2/sp2-restore-box1.json
```

Expected: JSON with `load_snapshot_ns`, `handshake_ns`, `preload_ns`, `faults_served`, `fork_exec_ns`. The `load_snapshot_ns` p50 is the key restore contribution number; record it.

- [ ] **Step 3: Run Go fault-latency benchmark**

```sh
GOOS=linux go test -bench=BenchmarkFaultService -benchtime=500x -run=^$ ./internal/fork/ \
  2>&1 | tee /root/sp2/sp2-go-fault-latency-box1.txt
```

Expected: `BenchmarkFaultService N ns/op`. Record the ns/op as the Go per-fault latency baseline.

- [ ] **Step 4: Run Rust fault-latency microbench**

```sh
/root/sp2/uffd-bench \
  --mode fault-latency \
  --iterations 500 --warmup 50 \
  --page-size 4096 --summary \
  --json /root/sp2/sp2-rust-fault-box1.json
```

Expected: summary table with p50 ns. Compare to Step 3.

- [ ] **Step 5: Retrieve and archive JSON files**

```sh
scp -F .superpowers/ssh_config \
    box1:/root/sp2/sp2-go-fork-box1-run1.json \
    box1:/root/sp2/sp2-go-fork-box1-run2.json \
    box1:/root/sp2/sp2-go-fork-box1-run3.json \
    box1:/root/sp2/sp2-restore-box1.json \
    box1:/root/sp2/sp2-rust-fault-box1.json \
    box1:/root/sp2/sp2-go-fault-latency-box1.txt \
    bench/results/
```

Rename files to the dated archive names:
```
bench/results/2026-06-23-sp2-uffd-rust-box1.json       (sp2-rust-fault-box1.json)
bench/results/2026-06-23-sp2-restore-box1.json
bench/results/2026-06-23-sp2-go-fault-latency-box1.txt
bench/results/2026-06-23-sp2-go-fork-box1-run{1,2,3}.json
```

### Task 4.4: Repeat on box2

Mirror Task 4.3 on box2. Save results with `-box2` suffix.

- [ ] **Step 1: Transfer binaries to box2**

```sh
scp -F .superpowers/ssh_config \
    internal/fork/uffd-bench/target/x86_64-unknown-linux-musl/release/uffd-bench \
    /tmp/bench-sp2 \
    box2:/root/sp2/
```

- [ ] **Step 2: Run the same four bench invocations on box2**

Mirror Task 4.3 Steps 1-4, substituting box2 paths. Confirm the DATA_DIR and TEMPLATE_ID match box2's layout.

- [ ] **Step 3: Retrieve and archive box2 JSON**

```sh
scp -F .superpowers/ssh_config \
    box2:/root/sp2/sp2-rust-fault-box2.json \
    box2:/root/sp2/sp2-restore-box2.json \
    box2:/root/sp2/sp2-go-fault-latency-box2.txt \
    box2:/root/sp2/sp2-go-fork-box2-run1.json \
    box2:/root/sp2/sp2-go-fork-box2-run2.json \
    box2:/root/sp2/sp2-go-fork-box2-run3.json \
    bench/results/
```

### Task 4.5: Write the human-readable comparison doc

**Files:**
- Create: `bench/results/2026-06-23-sp2-uffd-comparison.md`

- [ ] **Step 1: Write the comparison doc**

Use the SAME structure as `bench/results/2026-06-23-rust-agent-comparison.md`. Sections:

1. **Host** (box1 and box2 specs, kernel, Firecracker version, Rust toolchain, Go version)
2. **Raw archives** (list the JSON files archived next to this doc)
3. **Per-fault service latency (Go baseline vs Rust, ns)** - table: run, Go ns/op, Rust p50 ns
4. **Restore wall-time breakdown (Go engine, uffd-compare mode, ms)** - table: phase, p50, p90
5. **Fork-to-first-exec (existing Go baseline, ms)** - carried from box1/box2 run JSON; confirm stability vs the pre-existing result in `bench/results/2026-06-19-bare-metal-fork-exec.md`
6. **Honest summary** - what the numbers say, what they do not say

The comparison doc must NOT pre-write a conclusion. Fill it in with the actual numbers from Steps 4.3 and 4.4 only after the runs are complete.

- [ ] **Step 2: Commit Phase 4**

```sh
git add bench/results/
git commit -s -m "chore(sp2): archive SP2 UFFD comparison results (box1 + box2)"
```

---

## Phase 5: Recorded decision

**Terminal task. No production engine rewrite is committed by this plan. This task writes the decision with the measured numbers and gates any potential SP3.**

### Task 5.1: Write the decision document

**Files:**
- Create: `docs/superpowers/decisions/2026-06-23-sp2-fork-engine-rust.md`

- [ ] **Step 1: Write the decision document structure**

Use the #310 decision doc (`docs/superpowers/decisions/2026-06-23-rust-guest-agent.md`) as the template. Required sections:

**Header:**
```
Status: recorded. Date: 2026-06-23. Hardware: box1 (Intel i7-6700) and box2. Issue: SP2.
```

**Context:** What SP2 measured and why. Cite the spec section: `docs/superpowers/specs/2026-06-23-rust-rewrites-design.md` SP2 section. State explicitly: "Both #310 templates ran the same Go engine; #310 produced no data about engine headroom. SP2 exists to find out."

**Pre-registered thresholds (fill in BEFORE running the bench):**

Pre-register these thresholds before the measurements are in, so the decision is not goal-post-moved:

"Rewrite the UFFD handler in Rust only if: (1) the Rust per-fault service latency is more than 10 percent lower than Go at p50 on BOTH box1 AND box2, AND (2) the improvement is reproducible across at least 3 bench runs per box with no overlap in the confidence intervals."

"Rewrite the restore path in Rust only if the uffd-compare breakdown shows the Go userspace contribution to restore wall time (handshake + preload, excluding the Firecracker API call itself) exceeds 5 ms at p50 AND the Rust UFFD handler shows a measurable per-fault reduction."

**Measured results:** Fill in from `bench/results/2026-06-23-sp2-uffd-comparison.md` after Phase 4. Include p50/p90/p99 for per-fault latency, restore breakdown, and fork-to-first-exec.

**Decision:** Based on whether the thresholds are met:
- If threshold met: "Rewrite the [named piece] in Rust. SP3 is warranted." Specify what SP3 would cover.
- If threshold not met: "Keep Go for the UFFD handler and restore path. The bottleneck is the kernel/Firecracker path, not the Go handler loop. SP3 is not warranted."
- Either way: state explicitly that no production engine change is made by this plan.

**Consequences:** Update `docs/superpowers/specs/2026-06-23-rust-rewrites-design.md` to reflect the decision (add a paragraph to the SP2 section with the recorded outcome). If SP3 is warranted, note that a new spec is the next step.

- [ ] **Step 2: Commit the decision**

```sh
git add docs/superpowers/decisions/2026-06-23-sp2-fork-engine-rust.md \
        docs/superpowers/specs/2026-06-23-rust-rewrites-design.md
git commit -s -m "docs(sp2): recorded decision on fork-engine Rust hot-path spike"
```

---

## Self-Review

Before declaring Phase 5 complete, verify the following:

**Correctness:**
- [ ] `UffdioCopyArg` layout (`repr(C)`, 5 x 8-byte fields) verified against `uffdioCopyArg` in `internal/fork/uffd_linux.go` line 53. Any layout mismatch silently corrupts the ioctl.
- [ ] `uffd_copy` EEXIST handling: `EEXIST` is treated as success (the page was filled concurrently), mirroring `internal/fork/uffd_linux.go` line 216.
- [ ] `uffd_read_event` reads exactly 32 bytes and parses the faulting address from bytes 16..24 (little-endian u64), matching `uffdMsg.PfAddress` at offset 16 in the Go struct (`internal/fork/uffd_linux.go` line 63).
- [ ] The integration test `handler_serves_four_pages` passes on Linux (box1) before Phase 4 runs.

**Safety:**
- [ ] Every `unsafe` block has `// SAFETY:` with the specific invariant.
- [ ] `cargo clippy -- -D warnings` clean.
- [ ] `#![deny(unsafe_op_in_unsafe_fn)]` is set in `sys/mod.rs`.
- [ ] `UffdHandler::Drop` calls `libc::munmap` exactly once; no double-free risk.

**Measurement discipline:**
- [ ] No numbers are written in any doc before the bench runs are complete and JSON is archived.
- [ ] The decision pre-registers thresholds BEFORE the comparison doc is written.
- [ ] Box1 AND box2 results are both present in bench/results/ before the decision is written.
- [ ] The comparison doc cites the archived JSON filenames next to it.

**Scope:**
- [ ] No production file in `internal/fork/` was modified except `uffd_engine.go` (timing seams, additive only) and `uffd_bench_test.go` (test file).
- [ ] The decision document states explicitly that no engine rewrite is scheduled by this plan.
- [ ] The decision document names a specific triggering condition for SP3 (if warranted) or states SP3 is not warranted.

**Conventions:**
- [ ] No em/en dashes anywhere in the committed files.
- [ ] Every commit carries `Signed-off-by:` (DCO).
- [ ] `Cargo.lock` committed.
- [ ] `internal/fork/uffd_bench_test.go` has `//go:build linux` at the top.

**Gap: UFFDIO_REGISTER constant.** The Go code does not use `UFFDIO_REGISTER` (it receives the uffd from Firecracker over the socket rather than creating one itself). Task 1.2 Step 3 must verify the constant `0xc020aa00` from the kernel header (`linux/userfaultfd.h`) or the `userfaultfd` crate source before committing, since it is not present in the existing Go reference files. This is the only unresolved constant; all others are cited from the Go source.
