# Rust Guest Agent Spike Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a minimal Rust guest agent that is a drop-in `/init` replacement for the Go agent, then benchmark it against the Go baseline on KVM (warm exec round-trip latency and per-VM RSS) and record a keep-Go-or-adopt-Rust decision with the numbers (issue #310).

**Architecture:** The host (forkd / vsock client) speaks a language-agnostic JSON-lines protocol over vsock. The guest agent is the only thing that changes: a separate Rust crate at `guest/agent-rs/` compiles to a static `x86_64-unknown-linux-musl` binary that mounts the same filesystems, listens on the same vsock port, and answers the same request types as the Go agent. No host code, no controller code, and no proto changes. The Rust agent is selected only by which binary is baked into the rootfs as `/init`, so the existing `cmd/bench` harness measures it unchanged once a Rust-agent rootfs exists. This is an evaluation spike: the Rust agent implements only the protocol subset the benchmark exercises, and graduates to a real component only if the recorded decision says so.

**Tech Stack:** Rust (stable, edition 2021), std threads (no async runtime, to keep the binary tiny and the RSS comparison honest), `serde` + `serde_json` for framing, `libc` for `AF_VSOCK` and the init-time `mount`/`sethostname` syscalls. Go `cmd/bench` (existing) is the measurement harness. KVM host is a Hetzner bench box (box1/box2).

## Global Constraints

- Punctuation: never use em (U+2014) or en (U+2013) dashes anywhere (source, comments, Markdown, commit messages, PR text). Use only `.` `,` `;` `:` and ASCII hyphen-minus for ranges and compound identifiers.
- No unverified claims: every latency or memory number written into a doc, README, or the decision record must be reproducible from `bench/`. With no reachable KVM host, the spike produces NO number rather than a fabricated one.
- Secret values are never logged, never in error messages: the agent logs request types and counts only, never `configure` env/secret values, never exec argv, never file contents.
- DCO: every commit carries `Signed-off-by: Name <email>` (use `git commit -s`).
- Conventional commits with these scopes; branch `feat/rust-guest-agent-spike` off `main`.
- Stage explicit paths only; never `git add -A`.
- Security-sensitive path: `guest/agent` (and its Rust sibling) requires a named human reviewer before merge. The threat model (`docs/threat-model.md`) and fork-correctness doc (`docs/fork-correctness.md`) must be updated in the same PR if the spike graduates or changes the security surface.
- Spike scope is the guest agent ONLY. The fork engine (`internal/fork`) restore path is explicitly out of scope for this spike and is named in the decision record as deferred follow-up.
- Wire compatibility is exact: the Rust agent must produce JSON that the existing Go `internal/vsock` client unmarshals without changes. The Go structs in `internal/vsock/protocol.go` are the source of truth for every field name (`json:"..."` tags) and every `[]byte` field is base64 in JSON.

---

## Protocol subset for the spike

The Rust agent implements exactly the request types the `exec-rt` and `fork-exec` bench modes exercise, plus the file ops needed for a fair file-path comparison. Everything else is explicitly out of scope and answered with a clear error so an accidental dependency is loud, not silent.

In scope (must implement, wire-identical to Go):
- `ping` -> `PingResponse{uptime_seconds}`
- `exec` -> `ExecResponse{exit_code, stdout, stderr, exec_time_ms}` (one-shot, buffered)
- `exec_stream` -> dedicated-connection stream of `ExecStreamFrame` (`chunk` frames then one `exit` frame)
- `read_file` -> `ReadFileResponse{content, size}`
- `write_file` -> `Response{ok}`
- `list_dir` -> `ListDirResponse{entries}`
- `mkdir`, `remove` -> `Response{ok}`
- `configure` -> merge env+secrets into the in-memory map, `Response{ok}` (values never logged)
- `notify_forked` -> `NotifyForkedResponse{applied_clock_step_nanos, reseeded_rng, signaled_processes}` (fork-correctness; needed so `fork-exec` mode is honest)

Out of scope (answer with `Response{ok:false, error:"<type> not implemented in spike agent"}`, documented in the decision record): `run_code`, `pty`, `tunnel`, `vitals`, `tar_dir`, `untar_dir`.

## File Structure

- `guest/agent-rs/Cargo.toml` -- crate manifest, pinned deps, release profile tuned for size.
- `guest/agent-rs/src/protocol.rs` -- serde structs and enums mirroring `internal/vsock/protocol.go` for the in-scope subset; the wire contract lives here in one file.
- `guest/agent-rs/src/handlers.rs` -- pure-ish request handlers (exec, files, configure, notify_forked) that take a parsed request and return a response; unit-tested without a socket.
- `guest/agent-rs/src/transport.rs` -- vsock listener (AF_VSOCK via libc) with a Unix-socket fallback for host-side tests; line framing (read newline-delimited, write newline-terminated).
- `guest/agent-rs/src/init.rs` -- PID-1 init: mount proc/sys/dev/tmp/run, mkdir /workspace, sethostname.
- `guest/agent-rs/src/main.rs` -- wires init + transport + dispatch; one thread per connection.
- `guest/agent-rs/tests/protocol_roundtrip.rs` -- integration tests over the Unix-socket fallback, asserting byte-level JSON compatibility against fixtures captured from the Go agent.
- `hack/build-rust-agent.sh` -- reproducible static musl build producing `guest/agent-rs/target/x86_64-unknown-linux-musl/release/sandbox-agent`.
- `hack/rust-agent-rootfs.md` -- exact steps to bake the Rust binary into a template rootfs as `/init` so `cmd/bench` forks it.
- `docs/superpowers/decisions/2026-06-23-rust-guest-agent.md` -- the recorded decision (acceptance criterion 2), filled in Phase 3 with the measured numbers.
- `.github/workflows/kvm-test.yaml` -- add a non-blocking bench phase that builds the Rust agent and captures its numbers alongside the Go ones (Phase 4, optional).

---

## Phase 0: Baseline and scope freeze

### Task 0.1: Capture the Go agent baseline numbers

**Files:**
- Create: `bench/results/2026-06-23-go-agent-baseline.md`

**Interfaces:**
- Consumes: existing `cmd/bench` (`--mode exec-rt`, `--mode fork-exec`), an existing verified template on the KVM host.
- Produces: `go-baseline.json` (exec-rt) and `go-fork.json` (fork-exec) archived next to the results doc; the p50/p90/p99 table that Phase 3 diffs the Rust agent against.

- [ ] **Step 1: Confirm a KVM host and template are available**

Run on the Hetzner bench box (box1 or box2):

```sh
test -e /dev/kvm && echo "kvm ok" || echo "NO KVM: stop, this spike needs a KVM host"
ls "$DATA_DIR/templates/$TEMPLATE_ID/snapshot/mem"   # must exist and be verified
```

Expected: `kvm ok` and the snapshot file listed. If not, stop and provision per `bench/README.md`; do not fabricate numbers.

- [ ] **Step 2: Build and run the Go baseline**

```sh
go build -o /tmp/bench ./cmd/bench/
/tmp/bench --mode exec-rt   --template "$TEMPLATE_ID" --data-dir "$DATA_DIR" \
  --firecracker /usr/local/bin/firecracker --kernel "$DATA_DIR/vmlinux" \
  --iterations 200 --warmup 20 --summary --json /tmp/go-baseline.json
/tmp/bench --mode fork-exec --template "$TEMPLATE_ID" --data-dir "$DATA_DIR" \
  --firecracker /usr/local/bin/firecracker --kernel "$DATA_DIR/vmlinux" \
  --iterations 200 --warmup 20 --summary --json /tmp/go-fork.json
```

Expected: two summary tables printed (count/min/p50/p90/p99/max/mean) and two JSON files written.

- [ ] **Step 3: Capture per-VM RSS for the Go agent**

While one forked VM is alive (or via a short hold loop in the harness), record the agent RSS as the host sees it. Document the exact method used so it is reproducible:

```sh
# the guest agent runs as the VM's init; on the host, measure the firecracker
# process RSS for a single idle forked VM (steady state, post first-exec).
ps -o rss= -p "$(pgrep -n firecracker)"   # KB; record the method, not just the number
```

Expected: a single steady-state RSS figure in KB, with the measurement command recorded verbatim.

- [ ] **Step 4: Write the baseline results doc and commit**

Record host (CPU, kernel, Firecracker version, rootfs id), the two summary tables, and the RSS method + figure into `bench/results/2026-06-23-go-agent-baseline.md`. Archive `go-baseline.json` and `go-fork.json` next to it.

```sh
git add bench/results/2026-06-23-go-agent-baseline.md bench/results/go-baseline.json bench/results/go-fork.json
git commit -s -m "bench: capture Go guest-agent baseline for the Rust spike (#310)"
```

---

## Phase 1: Minimal Rust guest agent (host-testable, no KVM)

### Task 1.1: Crate scaffold and size-tuned release profile

**Files:**
- Create: `guest/agent-rs/Cargo.toml`
- Create: `guest/agent-rs/src/main.rs`

**Interfaces:**
- Produces: a buildable binary named `sandbox-agent`; `cargo test -p sandbox-agent` runs (with zero tests so far); the release profile later Tasks rely on for the size measurement.

- [ ] **Step 1: Write the failing build check**

Create `guest/agent-rs/Cargo.toml`:

```toml
[package]
name = "sandbox-agent"
version = "0.0.0"
edition = "2021"
publish = false

[[bin]]
name = "sandbox-agent"
path = "src/main.rs"

[dependencies]
serde = { version = "1", features = ["derive"] }
serde_json = "1"
libc = "0.2"

[profile.release]
opt-level = "z"
lto = true
codegen-units = 1
panic = "abort"
strip = true
```

Create `guest/agent-rs/src/main.rs`:

```rust
fn main() {
    eprintln!("sandbox-agent (rust spike): not yet wired");
    std::process::exit(1);
}
```

- [ ] **Step 2: Run the build to verify it compiles**

Run: `cd guest/agent-rs && cargo build --release`
Expected: PASS (binary at `target/release/sandbox-agent`).

- [ ] **Step 3: Commit**

```bash
git add guest/agent-rs/Cargo.toml guest/agent-rs/src/main.rs
git commit -s -m "feat(agent-rs): scaffold Rust guest-agent spike crate (#310)"
```

### Task 1.2: Protocol structs mirroring the Go wire contract

**Files:**
- Create: `guest/agent-rs/src/protocol.rs`
- Modify: `guest/agent-rs/src/main.rs` (add `mod protocol;`)
- Test: inline `#[cfg(test)]` module in `protocol.rs`

**Interfaces:**
- Consumes: field names and JSON tags from `internal/vsock/protocol.go` (`Request`, `Response`, `ExecRequest`, `ExecResponse`, `ReadFileRequest`, `ReadFileResponse`, `WriteFileRequest`, `ListDirRequest`, `ListDirResponse`, `FileEntry`, `PingResponse`, `ExecStreamFrame`, `ConfigureRequest`, `NotifyForkedRequest`, `NotifyForkedResponse`).
- Produces: Rust types `Request`, `Response`, `ExecResponse`, `ReadFileResponse`, `ListDirResponse`, `FileEntry`, `PingResponse`, `ExecStreamFrame`, `NotifyForkedResponse`, with serde attributes that produce byte-identical JSON. `[]byte` Go fields (`content`, `data`) are `Vec<u8>` serialized as base64 strings (the same encoding `encoding/json` applies to `[]byte`); add a small base64 serde helper module since serde_json does not base64 `Vec<u8>` by default.

- [ ] **Step 1: Write the failing test**

In `guest/agent-rs/src/protocol.rs`, add the struct definitions plus this test (the test will not compile until the structs exist, which is the failing state):

```rust
#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ping_response_matches_go_json() {
        let r = Response { ok: true, ping: Some(PingResponse { uptime_seconds: 1.5 }), ..Default::default() };
        let s = serde_json::to_string(&r).unwrap();
        // Go omitempty drops zero/None fields; ok and ping must be present.
        assert!(s.contains("\"ok\":true"));
        assert!(s.contains("\"uptime_seconds\":1.5"));
        assert!(!s.contains("\"error\""));
    }

    #[test]
    fn read_file_content_is_base64() {
        let r = Response { ok: true, read_file: Some(ReadFileResponse { content: b"hi".to_vec(), size: 2 }), ..Default::default() };
        let s = serde_json::to_string(&r).unwrap();
        assert!(s.contains("\"content\":\"aGk=\"")); // base64("hi")
        assert!(s.contains("\"size\":2"));
    }

    #[test]
    fn exec_request_parses_from_go_json() {
        let line = r#"{"type":"exec","exec":{"command":"echo hi","working_dir":"/workspace","timeout":30}}"#;
        let req: Request = serde_json::from_str(line).unwrap();
        assert_eq!(req.r#type, "exec");
        assert_eq!(req.exec.unwrap().command, "echo hi");
    }
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd guest/agent-rs && cargo test protocol`
Expected: FAIL (compile error: structs `Response`, `PingResponse`, etc. not defined).

- [ ] **Step 3: Write the structs**

Implement the serde structs in `protocol.rs`. Use `#[serde(rename = "...")]` to match every Go JSON tag exactly, `#[serde(skip_serializing_if = "...")]` for the `omitempty` fields, `#[serde(default)]` on optional request fields, and a `base64_bytes` serde-with module for `Vec<u8>` content fields. Add `mod protocol;` to `main.rs`. Derive `Default` on `Response` so the `..Default::default()` pattern works.

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd guest/agent-rs && cargo test protocol`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add guest/agent-rs/src/protocol.rs guest/agent-rs/src/main.rs
git commit -s -m "feat(agent-rs): wire-compatible protocol structs for the spike subset (#310)"
```

### Task 1.3: Request handlers (ping, exec, files, configure, notify_forked)

**Files:**
- Create: `guest/agent-rs/src/handlers.rs`
- Modify: `guest/agent-rs/src/main.rs` (add `mod handlers;`)
- Test: inline `#[cfg(test)]` module in `handlers.rs`

**Interfaces:**
- Consumes: `protocol::{Request, Response, ...}` from Task 1.2.
- Produces: `pub fn dispatch(req: &Request, env: &Mutex<HashMap<String,String>>) -> Response` for the one-shot types; `pub fn handle_exec(...)`, `pub fn handle_read_file(...)`, `pub fn handle_write_file(...)`, `pub fn handle_list_dir(...)`, `pub fn handle_configure(...)`, `pub fn handle_notify_forked(...)`. The streaming `exec_stream` is handled in transport (Task 1.5) because it owns its connection.

- [ ] **Step 1: Write the failing test**

In `handlers.rs`:

```rust
#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;
    use std::sync::Mutex;

    #[test]
    fn exec_returns_stdout_and_exit_zero() {
        let env = Mutex::new(HashMap::new());
        let req = serde_json::from_str(r#"{"type":"exec","exec":{"command":"printf hello","timeout":5}}"#).unwrap();
        let resp = dispatch(&req, &env);
        assert!(resp.ok);
        let e = resp.exec.unwrap();
        assert_eq!(e.exit_code, 0);
        assert_eq!(e.stdout, "hello");
    }

    #[test]
    fn write_then_read_file_roundtrips() {
        let env = Mutex::new(HashMap::new());
        let dir = std::env::temp_dir().join("agentrs_test");
        let path = dir.join("f.txt");
        let p = path.to_str().unwrap();
        let w: super::super::protocol::Request =
            serde_json::from_str(&format!(r#"{{"type":"write_file","write_file":{{"path":"{p}","content":"aGk=","mode":420}}}}"#)).unwrap();
        assert!(dispatch(&w, &env).ok);
        let r: super::super::protocol::Request =
            serde_json::from_str(&format!(r#"{{"type":"read_file","read_file":{{"path":"{p}"}}}}"#)).unwrap();
        let resp = dispatch(&r, &env);
        assert_eq!(resp.read_file.unwrap().content, b"hi");
    }

    #[test]
    fn configure_values_are_not_returned_or_logged() {
        let env = Mutex::new(HashMap::new());
        let req = serde_json::from_str(r#"{"type":"configure","configure":{"secrets":{"TOKEN":"s3cret"}}}"#).unwrap();
        let resp = dispatch(&req, &env);
        assert!(resp.ok);
        assert_eq!(env.lock().unwrap().get("TOKEN").map(String::as_str), Some("s3cret"));
        // the response carries no echo of the secret value
        assert!(!serde_json::to_string(&resp).unwrap().contains("s3cret"));
    }

    #[test]
    fn out_of_scope_type_is_a_clear_error() {
        let env = Mutex::new(HashMap::new());
        let req = serde_json::from_str(r#"{"type":"vitals"}"#).unwrap();
        let resp = dispatch(&req, &env);
        assert!(!resp.ok);
        assert!(resp.error.contains("not implemented in spike agent"));
    }
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd guest/agent-rs && cargo test handlers`
Expected: FAIL (`dispatch` not defined).

- [ ] **Step 3: Implement the handlers**

Implement `dispatch` and the per-type handlers. Mirror the Go semantics from `guest/agent/main.go`: exec runs `/bin/sh -c <command>` with cwd defaulting to `/workspace`, env merged from process env + configured + per-request, 30s default timeout, exit 124 on timeout; write_file `mkdir -p`s the parent and defaults mode 0o644; configure merges additively and logs only the count; notify_forked applies the clock step and RNG reseed best-effort and returns the counts. Log request types and counts only, never values.

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd guest/agent-rs && cargo test handlers`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add guest/agent-rs/src/handlers.rs guest/agent-rs/src/main.rs
git commit -s -m "feat(agent-rs): request handlers for ping/exec/files/configure/notify_forked (#310)"
```

### Task 1.4: notify_forked fork-correctness behavior

**Files:**
- Modify: `guest/agent-rs/src/handlers.rs`
- Test: inline `#[cfg(test)]` in `handlers.rs`

**Interfaces:**
- Consumes: `handle_notify_forked` from Task 1.3.
- Produces: a `handle_notify_forked` that performs the same fork-correctness actions the Go agent does (per `docs/fork-correctness.md`): step `CLOCK_REALTIME` toward the host-provided wall time and reseed the kernel RNG, returning `applied_clock_step_nanos`, `reseeded_rng`, `signaled_processes`.

- [ ] **Step 1: Write the failing test**

```rust
#[test]
fn notify_forked_reports_reseed() {
    // On a host without CAP_SYS_TIME the clock step may be 0, but the RNG
    // reseed path (writing to /dev/urandom, which any process may do) must be
    // attempted and reported. The test asserts the response shape and that a
    // zero-drift notify yields a zero clock step, not an error.
    let env = std::sync::Mutex::new(std::collections::HashMap::new());
    let req = serde_json::from_str(r#"{"type":"notify_forked","notify_forked":{"wall_clock_unix_nanos":0}}"#).unwrap();
    let resp = dispatch(&req, &env);
    assert!(resp.ok);
    let n = resp.notify_forked.unwrap();
    assert_eq!(n.applied_clock_step_nanos, 0); // 0 host time => no step
    assert!(n.reseeded_rng); // writing entropy to /dev/urandom does not need caps
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd guest/agent-rs && cargo test notify_forked_reports_reseed`
Expected: FAIL (reseed not yet implemented or returns false).

- [ ] **Step 3: Implement the reseed and clock step**

Write fresh entropy to `/dev/urandom` (the `RNDADDENTROPY`-free path: a plain write credits the pool for reseed-on-fork; this matches the Go agent's userspace reseed) and set `reseeded_rng = true` on success. Apply the clock step only when a nonzero host wall time is provided and the drift exceeds tolerance; otherwise report 0. Keep the cross-reference comment to `docs/fork-correctness.md`.

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd guest/agent-rs && cargo test notify_forked`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add guest/agent-rs/src/handlers.rs
git commit -s -m "feat(agent-rs): notify_forked RNG reseed and clock step (#310)"
```

### Task 1.5: Transport (line framing, Unix-socket fallback, exec_stream)

**Files:**
- Create: `guest/agent-rs/src/transport.rs`
- Modify: `guest/agent-rs/src/main.rs` (add `mod transport;`)
- Test: `guest/agent-rs/tests/protocol_roundtrip.rs`

**Interfaces:**
- Consumes: `protocol` and `handlers` modules.
- Produces: `pub fn serve_unix(path: &str, env: Arc<Mutex<HashMap<String,String>>>)` (test entry point) and `pub fn serve_vsock(port: u32, env: ...)` (production entry point); a per-connection `handle_conn(stream, env)` that reads newline-delimited requests, dispatches one-shot types via `handlers::dispatch`, and for `exec_stream` takes over the connection to emit `chunk` frames then one `exit` frame, exactly like `guest/agent/main.go` `handleConnection`.

- [ ] **Step 1: Write the failing integration test**

`guest/agent-rs/tests/protocol_roundtrip.rs`:

```rust
use std::io::{BufRead, BufReader, Write};
use std::os::unix::net::UnixStream;

// Spawns the agent on a unix socket and drives it like the Go vsock client would.
fn with_agent<F: FnOnce(&str)>(name: &str, f: F) {
    let sock = std::env::temp_dir().join(name);
    let _ = std::fs::remove_file(&sock);
    let p = sock.to_str().unwrap().to_string();
    let env = std::sync::Arc::new(std::sync::Mutex::new(std::collections::HashMap::new()));
    let p2 = p.clone();
    let h = std::thread::spawn(move || sandbox_agent::transport::serve_unix(&p2, env));
    // wait for the socket to appear
    for _ in 0..100 { if std::path::Path::new(&p).exists() { break } std::thread::sleep(std::time::Duration::from_millis(10)); }
    f(&p);
    drop(h);
}

#[test]
fn exec_stream_emits_chunks_then_exit() {
    with_agent("agentrs_stream.sock", |p| {
        let mut s = UnixStream::connect(p).unwrap();
        s.write_all(b"{\"type\":\"exec_stream\",\"exec_stream\":{\"command\":\"printf abc\"}}\n").unwrap();
        let mut r = BufReader::new(s);
        let mut kinds = vec![];
        let mut got = String::new();
        loop {
            let mut line = String::new();
            if r.read_line(&mut line).unwrap() == 0 { break }
            let v: serde_json::Value = serde_json::from_str(&line).unwrap();
            kinds.push(v["kind"].as_str().unwrap().to_string());
            if v["kind"] == "exit" { break }
            if let Some(d) = v["data"].as_str() {
                got.push_str(&String::from_utf8(base64_decode(d)).unwrap());
            }
        }
        assert!(kinds.last().unwrap() == "exit");
        assert_eq!(got, "abc");
    });
}

fn base64_decode(s: &str) -> Vec<u8> { /* tiny inline decoder or reuse the crate helper */ unimplemented!() }
```

(Replace `base64_decode` with the crate's helper or a small standard decoder; the test must actually decode.)

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd guest/agent-rs && cargo test --test protocol_roundtrip`
Expected: FAIL (`serve_unix` / `transport` not defined).

- [ ] **Step 3: Implement transport**

Implement line framing (read with a buffered reader, 96 MiB max line to match `MaxMessageBytes`), one thread per accepted connection, one-shot dispatch via `handlers::dispatch`, and the `exec_stream` takeover that spawns `/bin/sh -c <command>` and streams stdout/stderr as base64 `chunk` frames followed by one `exit` frame carrying the exit code. Add `serve_vsock` using `libc` `AF_VSOCK` socket/bind/listen/accept (mirror `listenVsock` in the Go agent, including the Unix-socket fallback message). Expose the modules from `main.rs` as a library target so the integration test can call `sandbox_agent::transport::serve_unix` (add a `[lib]` section or a `lib.rs` re-exporting the modules).

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd guest/agent-rs && cargo test --test protocol_roundtrip`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add guest/agent-rs/src/transport.rs guest/agent-rs/src/main.rs guest/agent-rs/src/lib.rs guest/agent-rs/tests/protocol_roundtrip.rs
git commit -s -m "feat(agent-rs): vsock+unix transport with exec_stream framing (#310)"
```

### Task 1.6: PID-1 init and main wiring

**Files:**
- Create: `guest/agent-rs/src/init.rs`
- Modify: `guest/agent-rs/src/main.rs`
- Test: inline `#[cfg(test)]` in `init.rs` (pure helpers only)

**Interfaces:**
- Consumes: `transport::serve_vsock`.
- Produces: `pub fn init_system()` (mounts proc/sys/dev/tmp/run, mkdir /workspace, sethostname "sandbox"), and a `main` that calls `init_system()` when `getpid()==1`, then serves vsock on the agent port. The agent port constant must equal `vsock.AgentPort` (read it from `internal/vsock` and hardcode the same value with a comment citing the source).

- [ ] **Step 1: Write the failing test**

`init.rs` pure-helper test (the mount table itself is not unit-testable off-VM, so test the table contents):

```rust
#[cfg(test)]
mod tests {
    use super::mount_table;
    #[test]
    fn mount_table_matches_go_agent() {
        let t = mount_table();
        let targets: Vec<&str> = t.iter().map(|m| m.target).collect();
        assert_eq!(targets, vec!["/proc", "/sys", "/dev", "/tmp", "/run"]);
    }
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd guest/agent-rs && cargo test mount_table`
Expected: FAIL (`mount_table` not defined).

- [ ] **Step 3: Implement init and wire main**

Implement `mount_table()` returning the same five mounts as `guest/agent/main.go` `initSystem`, `init_system()` performing the mounts + mkdir + sethostname via `libc` (non-fatal on error, log and continue, matching the Go agent), and a `main` that branches on `getpid()==1` and then calls `serve_vsock(AGENT_PORT, ...)`.

- [ ] **Step 4: Run the test and a full build**

Run: `cd guest/agent-rs && cargo test && cargo build --release`
Expected: PASS and a release binary.

- [ ] **Step 5: Commit**

```bash
git add guest/agent-rs/src/init.rs guest/agent-rs/src/main.rs
git commit -s -m "feat(agent-rs): PID-1 init and main wiring (#310)"
```

---

## Phase 2: Reproducible build and rootfs swap

### Task 2.1: Static musl build script

**Files:**
- Create: `hack/build-rust-agent.sh`

**Interfaces:**
- Produces: a script that emits `guest/agent-rs/target/x86_64-unknown-linux-musl/release/sandbox-agent`, fully static (no dynamic interpreter), and prints its stripped size in bytes (one of the spike's headline numbers).

- [ ] **Step 1: Write the build script**

```sh
#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../guest/agent-rs"
rustup target add x86_64-unknown-linux-musl >/dev/null 2>&1 || true
cargo build --release --target x86_64-unknown-linux-musl
BIN=target/x86_64-unknown-linux-musl/release/sandbox-agent
file "$BIN"                       # expect: statically linked
printf 'rust-agent size (bytes): %s\n' "$(stat -c%s "$BIN" 2>/dev/null || stat -f%z "$BIN")"
```

- [ ] **Step 2: Run it (or confirm it parses on a host without the musl target)**

Run: `bash hack/build-rust-agent.sh`
Expected on a Linux host with rustup: a statically linked binary and a printed size. On darwin without the musl toolchain, the script fails at the cargo step with a clear toolchain message (record that the size number is captured on the Linux bench host).

- [ ] **Step 3: Commit**

```bash
chmod +x hack/build-rust-agent.sh
git add hack/build-rust-agent.sh
git commit -s -m "build(agent-rs): reproducible static musl build script (#310)"
```

### Task 2.2: Rootfs bake instructions

**Files:**
- Create: `hack/rust-agent-rootfs.md`

**Interfaces:**
- Consumes: the static binary from Task 2.1, an existing template rootfs the Go bench uses.
- Produces: exact steps to copy the Rust binary into a clone of the template rootfs as `/init`, re-snapshot it through `forkd CreateTemplate`, and write the `verified` marker so `cmd/bench` will fork it. This yields a second template id (e.g. `<base>-rustagent`) the Phase 3 bench targets, leaving the Go template untouched for an apples-to-apples comparison.

- [ ] **Step 1: Write the instructions**

Document: mount the template `rootfs.ext4` loopback (or rebuild it), replace `/init` with the static Rust binary (same path the Go agent occupies per `bench/README.md`), unmount, then run the same `forkd CreateTemplate` flow the Go template used so the snapshot is created by the engine and content-addressed. Record the resulting template id. Note that the kernel and rootfs base are otherwise byte-identical to the Go template so the only variable is the agent binary.

- [ ] **Step 2: Commit**

```bash
git add hack/rust-agent-rootfs.md
git commit -s -m "docs(agent-rs): rootfs bake steps for the Rust-agent template (#310)"
```

---

## Phase 3: Measure on KVM and record the decision

### Task 3.1: Run the head-to-head bench

**Files:**
- Create: `bench/results/2026-06-23-rust-agent-comparison.md`

**Interfaces:**
- Consumes: the Go baseline (Task 0.1), the Rust-agent template (Task 2.2), the unchanged `cmd/bench`.
- Produces: `rust-execrt.json`, `rust-fork.json`, and a side-by-side table (Go vs Rust: p50/p90/p99 exec-rt, p50/p90/p99 fork-exec, per-VM RSS, binary size) archived in `bench/results/`.

- [ ] **Step 1: Run the same bench modes against the Rust template**

On the KVM host, with the SAME iteration/warmup counts as the baseline:

```sh
go build -o /tmp/bench ./cmd/bench/
/tmp/bench --mode exec-rt   --template "$RUST_TEMPLATE_ID" --data-dir "$DATA_DIR" \
  --firecracker /usr/local/bin/firecracker --kernel "$DATA_DIR/vmlinux" \
  --iterations 200 --warmup 20 --summary --json /tmp/rust-execrt.json
/tmp/bench --mode fork-exec --template "$RUST_TEMPLATE_ID" --data-dir "$DATA_DIR" \
  --firecracker /usr/local/bin/firecracker --kernel "$DATA_DIR/vmlinux" \
  --iterations 200 --warmup 20 --summary --json /tmp/rust-fork.json
```

Expected: two summary tables and two JSON files. If the host or template is missing, stop and produce no number.

- [ ] **Step 2: Capture per-VM RSS for the Rust agent with the identical method**

Use the exact command recorded in Task 0.1 Step 3 so the two RSS figures are comparable.

- [ ] **Step 3: Write the comparison doc**

Tabulate Go vs Rust for exec-rt p50/p90/p99, fork-exec p50/p90/p99, steady-state per-VM RSS, and stripped binary size. Record host details and both JSON file names. State the delta plainly, including if Rust is NOT faster (the honesty rule: the wedge may be RSS/footprint, not latency).

- [ ] **Step 4: Commit**

```bash
git add bench/results/2026-06-23-rust-agent-comparison.md bench/results/rust-execrt.json bench/results/rust-fork.json
git commit -s -m "bench: Go vs Rust guest-agent head-to-head (#310)"
```

### Task 3.2: Record the decision

**Files:**
- Create: `docs/superpowers/decisions/2026-06-23-rust-guest-agent.md`
- Modify: `docs/threat-model.md` (only if the decision is to adopt)
- Modify: `docs/fork-correctness.md` (note the Rust agent's reseed/clock path if adopted)

**Interfaces:**
- Consumes: the comparison numbers from Task 3.1.
- Produces: a recorded decision (acceptance criterion 2 of #310): keep Go, or adopt Rust for the guest agent behind the vsock contract, with the numbers that justify it; plus the explicit deferral of the fork-engine restore path to a separate follow-up spike.

- [ ] **Step 1: Write the decision record**

Structure: Context (the #310 thesis and the #24 concurrency-bug argument), Measured results (the table from Task 3.1, with links to the archived JSON), Decision (one of: keep Go; adopt Rust guest agent; need more data), Rationale tied to the numbers, Consequences (if adopt: dual toolchain, CI lane, the remaining out-of-scope protocol types to port, a named human reviewer for `guest/agent-rs`), and Follow-ups (fork-engine restore-path spike as a separate issue). Include a threshold stated up front: e.g. "adopt only if the Rust agent shows a material per-VM RSS reduction or a p99 exec-rt improvement beyond measurement noise; otherwise keep Go."

- [ ] **Step 2: Update threat-model and fork-correctness docs only if adopting**

If the decision is adopt, add the `guest/agent-rs` row to `docs/threat-model.md` and note the Rust reseed/clock-step path in `docs/fork-correctness.md` in the same commit. If the decision is keep-Go, state in the decision record that no security-surface change occurred and these docs are unchanged.

- [ ] **Step 3: Commit**

```bash
git add docs/superpowers/decisions/2026-06-23-rust-guest-agent.md
# plus docs/threat-model.md docs/fork-correctness.md only if adopting
git commit -s -m "docs: record Rust guest-agent spike decision (#310)"
```

---

## Phase 4 (optional, only if the decision is adopt): CI lane

### Task 4.1: Non-blocking Rust-agent bench phase in kvm-test

**Files:**
- Modify: `.github/workflows/kvm-test.yaml`

**Interfaces:**
- Consumes: `hack/build-rust-agent.sh`, the Rust-agent rootfs steps.
- Produces: a CI step that builds the Rust agent and captures its bench numbers as an artifact alongside the Go ones, initially non-blocking (it informs, it does not gate), so regressions in the Rust path are visible without destabilizing the required checks.

- [ ] **Step 1: Add the build + bench step**

In the existing KVM bench phase, add a step that runs `hack/build-rust-agent.sh`, bakes the Rust template per `hack/rust-agent-rootfs.md`, runs the two bench modes against it, and uploads the JSON as an artifact. Mark the job `continue-on-error: true` until the team decides to make it a required check.

- [ ] **Step 2: Confirm the workflow parses**

Run: `actionlint .github/workflows/kvm-test.yaml` (or the repo's lint equivalent)
Expected: no errors.

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/kvm-test.yaml
git commit -s -m "ci: non-blocking Rust guest-agent bench phase (#310)"
```

---

## Self-Review

**Spec coverage (issue #310 acceptance criteria):**
- "A benchmark comparison (Go vs Rust) for the guest agent exec/stream path, reproducible from bench/" -> Phase 0 (baseline) + Phase 3 Task 3.1 (comparison), both using the existing `cmd/bench` and archiving JSON under `bench/results/`.
- "A recorded decision: keep Go, or adopt Rust for a specific component behind its interface, with the numbers that justify it" -> Phase 3 Task 3.2.
- "behind the existing clean interfaces (GuestConn / vsock)" -> the Rust agent is a drop-in `/init` and the host is unchanged; the vsock JSON-lines contract in `internal/vsock/protocol.go` is the seam (Task 1.2 mirrors it exactly).
- Fork-engine restore path -> explicitly out of scope and deferred in the decision record (Task 3.2), matching the issue's "and/or" phrasing by choosing the guest agent as the smaller, lower-risk first target.

**Placeholder scan:** the only intentional stub is `base64_decode` in the Task 1.5 test, which is called out to be replaced with a real decoder; every other step carries concrete code or commands.

**Type consistency:** `dispatch(&Request, &Mutex<HashMap<String,String>>) -> Response` is used identically in Tasks 1.3, 1.4, and referenced by Task 1.5; `serve_unix`/`serve_vsock` signatures match between Task 1.5 (definition) and Tasks 1.6/test (use); `mount_table()` is defined and tested in Task 1.6; template ids (`$TEMPLATE_ID` Go, `$RUST_TEMPLATE_ID` Rust) are distinct and consistent across Phases 0, 2, 3.
