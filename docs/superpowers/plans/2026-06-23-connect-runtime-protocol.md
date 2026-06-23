# Connect Runtime Protocol Implementation Plan (#24)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the JSON-over-HTTP sandbox runtime API and the guest JSON-lines vsock protocol with one `sandbox.v1.Sandbox` gRPC/Connect service over three transports (vsock, forkd :9091, browser), migrate all six SDKs, wire the budget-gated self-service RPCs, and remove the old runtime API.

**Architecture:** One Service implementation (`internal/sandboxrpc`) behind the generated Connect/gRPC interface, bridged to a gRPC server the guest agent runs over vsock. forkd and sandbox-server mount the Service with a per-sandbox bearer-token interceptor; the browser reaches it via Connect-Web. The fork-correctness handshake and secret delivery stay on a separate internal forkd-to-guest vsock service.

**Tech Stack:** Go, connectrpc (connect-go), grpc-go over vsock, buf/protoc, Firecracker microVM (KVM CI), Python/TypeScript/Go/Ruby/Rust/Java SDKs.

## Global Constraints

- Punctuation (strict): never use em (U+2014) or en (U+2013) dashes anywhere (code, comments, docs, YAML, proto, commit messages). Only `.` `,` `;` `:` and ASCII hyphen-minus.
- DCO: every commit MUST carry a `Signed-off-by: Name <email>` trailer (`git commit -s`); the dco-check CI job fails otherwise.
- Go: `fmt.Errorf("context: %w", err)`; octal `0o644`; gofmt + golangci-lint clean (BOTH `golangci-lint run --timeout=5m` AND `GOOS=linux golangci-lint run --timeout=5m`); guest cross-build `GOOS=linux GOARCH=amd64 go build ./guest/agent/`.
- Secrets: secret VALUES never logged, never in errors/conditions, never on host paths. Log keys and counts only. The per-sandbox token is host-side only; the guest never sees it.
- Errors are LLM-legible (#28): every RPC error detail carries `{code, message, cause, remediation}` per docs/api/errors.md.
- The INTERNAL control service (`NotifyForked`, `Configure`, `ping`) is never exposed on the public `Sandbox` service or on :9091; a test asserts the public service has no such methods.
- TDD: failing test first; every behavior change lands with its test in the same commit.
- Stage explicit paths only; never `git add -A`. Conventional commits.
- `guest/agent`, `internal/daemon`, `internal/fork` are security-sensitive paths needing a named human reviewer before merge.
- Work in a git worktree on branch `feat/connect-runtime-protocol` (created via using-git-worktrees at execution time). The spec and this plan are committed there first.

## Verification commands (used throughout)

- Build / vet: `go build ./...` / `go vet ./...`
- Proto regen: `make proto` (after extending it to cover proto/sandbox/v1)
- Unit (fork/workspace/vsock): `make test-unit`
- Targeted: `go test ./internal/sandboxrpc/...`, `go test ./internal/daemon/...`, `go test ./guest/agent/...`
- Python SDK: `make test-python`; TS: `cd sdk/typescript && npm test`; Go SDK: `cd sdk/go && go test ./...`; Ruby/Rust/Java per their READMEs.
- KVM CI: the `firecracker-test` job (kvm-test.yaml) plus the new streaming exec/PTY phase.

## File structure (created or modified)

- `proto/sandbox/v1/sandbox.proto` (modify): add RunCode, Mkdir, Remove, Upload.
- `proto/sandbox/v1/sandbox*.pb.go`, `sandboxv1connect/` (regen).
- `proto/sandbox/internal/v1/internal.proto` (create): the internal control service (NotifyForked, Configure, Ping).
- `internal/sandboxrpc/` (create): `service.go` (the Service), `guestconn.go` (the `GuestConn` port + vsock-gRPC bridge), `exec.go`, `files.go`, `runcode.go`, `portforward.go`, `vitals.go`, `watch.go`, `process.go`, `selfservice.go`, `interceptor.go` (token), and `*_test.go`.
- `internal/daemon/` (modify): mount the Connect server; the JSON runtime handlers delegate then are removed (stage 9).
- `cmd/sandbox-server/main.go` (modify): mount the Connect server.
- `guest/agent/` (modify): replace the JSON-lines loop with a gRPC server; one file per RPC group, plus `internal_server.go` for the control service.
- `internal/vsock/` (modify): a `net.Conn` dialer/listener usable by grpc-go (keep the existing dial primitives; drop the JSON-lines codec once the guest flips).
- SDKs: `sdk/python/mitos/direct.py` + transport; `sdk/typescript/src/*`; `sdk/go`, `sdk/ruby`, `sdk/rust`, `sdk/java` exec transport; `sdk/conformance` streaming/PTY scenarios.
- `.github/workflows/kvm-test.yaml` (modify): streaming exec + PTY phase.
- Docs: `docs/api/runtime-protocol.md`, `docs/api/v2-spec.md`, `docs/threat-model.md` (guest protocol surface moved), `CLAUDE.md` (component descriptions).

---

## Stage 1: Proto additions

### Task 1.1: Add RunCode, Mkdir, Remove, Upload to the Sandbox proto

**Files:**
- Modify: `proto/sandbox/v1/sandbox.proto`
- Modify: `Makefile` (the `proto` target to cover `proto/sandbox/v1`)
- Regenerate: `proto/sandbox/v1/sandbox.pb.go`, `sandbox_grpc.pb.go`, `sandboxv1connect/sandbox.connect.go`

**Interfaces:**
- Produces: `sandboxv1.RunCodeRequest/RunCodeResponse/RunCodeOpen/Result/RunError`, `MkdirRequest/MkdirResponse`, `RemoveRequest/RemoveResponse`, `UploadRequest/UploadResult`, and the four new RPC methods on the generated `SandboxServiceHandler`/client.

- [ ] **Step 1: Add the proto messages and RPCs**

In `proto/sandbox/v1/sandbox.proto`, add to `service Sandbox`:

```proto
  rpc RunCode(stream RunCodeRequest) returns (stream RunCodeResponse);
  rpc Mkdir(MkdirRequest) returns (MkdirResponse);
  rpc Remove(RemoveRequest) returns (RemoveResponse);
  rpc Upload(stream UploadRequest) returns (UploadResult);
```

and the messages:

```proto
message RunCodeOpen {
  string code = 1;
  string language = 2;          // default "python"
  int64 timeout_seconds = 3;
}
message RunCodeRequest {
  oneof msg { RunCodeOpen open = 1; bytes stdin = 2; }
}
message RunResult {
  string text = 1;
  map<string, bytes> data = 2;  // mime -> payload (Jupyter rich output)
}
message RunError {
  string name = 1;
  string value = 2;
  repeated string traceback = 3;
}
message RunCodeResponse {
  oneof msg {
    bytes stdout = 1;
    bytes stderr = 2;
    RunResult result = 3;
    RunError error = 4;
    int32 exit_code = 5;
  }
}
message MkdirRequest { string path = 1; }
message MkdirResponse {}
message RemoveRequest { string path = 1; bool recursive = 2; }
message RemoveResponse {}
message UploadOpen { string dest = 1; }   // untar destination dir
message UploadRequest { oneof msg { UploadOpen open = 1; bytes chunk = 2; } }
message UploadResult { int64 bytes_written = 1; }
```

No em or en dashes in comments.

- [ ] **Step 2: Make the proto target cover proto/sandbox/v1**

In `Makefile`, the `proto` target currently regenerates `proto/forkd.proto`. Add the `proto/sandbox/v1/sandbox.proto` invocation (protoc with `protoc-gen-go`, `protoc-gen-go-grpc`, and `protoc-gen-connect-go`), output in place. Match the existing buf/protoc invocation style in the repo.

- [ ] **Step 3: Regenerate and build**

Run: `make proto && go build ./proto/...`
Expected: the four new RPCs appear in `sandboxv1connect/sandbox.connect.go` and the package builds.

- [ ] **Step 4: Commit**

```bash
git add proto/sandbox/v1/sandbox.proto proto/sandbox/v1/ Makefile
git commit -s -m "feat(proto): add RunCode, Mkdir, Remove, Upload to sandbox.v1"
```

### Task 1.2: Define the internal control service proto

**Files:**
- Create: `proto/sandbox/internal/v1/internal.proto`
- Regenerate: its stubs

**Interfaces:**
- Produces: `internalv1.Control` service with `NotifyForked`, `Configure`, `Ping`, mapping the existing `internal/vsock` `NotifyForkedRequest/Response`, `ConfigureRequest/Response`, `PingResponse` fields exactly.

- [ ] **Step 1: Author the proto**

Mirror the existing `internal/vsock/protocol.go` `NotifyForkedRequest` (generation, host_wall_clock_nanos, entropy bytes, network, volumes), `ConfigureRequest` (env, secrets map), and `PingResponse` (uptime_seconds) field-for-field. This service is served by the guest on a DISTINCT vsock service name; it carries secrets, so it is never part of `sandbox.v1.Sandbox`.

- [ ] **Step 2: Regenerate + build + commit**

Run: `make proto && go build ./proto/...`
```bash
git add proto/sandbox/internal/v1/ Makefile
git commit -s -m "feat(proto): internal control service (NotifyForked, Configure, Ping)"
```

---

## Stage 2: internal/sandboxrpc.Service (against a fake guest)

### Task 2.1: The GuestConn port and the Service skeleton

**Files:**
- Create: `internal/sandboxrpc/guestconn.go`, `internal/sandboxrpc/service.go`, `internal/sandboxrpc/service_test.go`

**Interfaces:**
- Produces: `type GuestConn interface { Exec(ctx, *sandboxv1.ExecOpen) (ExecStream, error); ReadFile(...); WriteFile(...); List(...); Stat(...); Mkdir(...); Remove(...); RunCode(...); PortForward(...); Vitals(...); Watch(...); Processes(...); Signal(...) }` (one method per logical op, returning a stream handle or value). `type Service struct { Guest func(sandboxID string) (GuestConn, error) }` implementing `sandboxv1connect.SandboxServiceHandler`.
- Consumes: the generated `sandboxv1connect.SandboxServiceHandler` interface.

- [ ] **Step 1: Write the failing test (Exec unary path over a fake guest)**

```go
func TestServiceExecStreamsStdoutAndExit(t *testing.T) {
	fake := &fakeGuest{execChunks: []string{"hel", "lo\n"}, exit: 0}
	svc := &Service{Guest: func(string) (GuestConn, error) { return fake, nil }}
	// drive svc.Exec via an in-memory connect stream; collect ExecResponse frames
	got := drainExec(t, svc, &sandboxv1.ExecOpen{Command: "echo hello"})
	if got.stdout != "hello\n" || got.exit != 0 {
		t.Fatalf("want hello\\n exit 0, got %q exit %d", got.stdout, got.exit)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/sandboxrpc/ -run TestServiceExecStreams -v`
Expected: FAIL (Service/GuestConn not defined).

- [ ] **Step 3: Implement GuestConn interface + Service.Exec**

Define `GuestConn` in guestconn.go and `Service` in service.go. `Service.Exec` reads the client `ExecOpen`, opens `guest.Exec(open)`, and copies guest stdout/stderr/exit frames to the connect `ExecResponse` stream. Map errors to LLM-legible connect errors.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/sandboxrpc/ -run TestServiceExecStreams -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add -s internal/sandboxrpc/guestconn.go internal/sandboxrpc/service.go internal/sandboxrpc/service_test.go
git commit -s -m "feat(sandboxrpc): Service with GuestConn port; Exec streams via a fake guest"
```

### Tasks 2.2 - 2.7: one task per RPC group

For each group below, follow the SAME TDD shape as Task 2.1 (failing test against the fake guest, implement the Service method delegating to the matching `GuestConn` method, pass, commit). Each is its own file + test:

- **2.2 files.go**: `ReadFile` (server-stream Chunk), `WriteFile` (client-stream), `List` (AIP-158 page_size/page_token/filter), `Stat`, `Mkdir`, `Remove`. Test: a write then read round-trips; List returns the fake entries with a next_page_token when truncated.
- **2.3 runcode.go**: `RunCode` streaming with stdout/stderr chunks, a `RunResult` frame (text + data map), a `RunError` frame, and exit. Test: the fake guest emits a result frame with `data["text/html"]`; the Service forwards it intact.
- **2.4 portforward.go**: `PortForward` bidi Frame proxy. Test: bytes written client-side appear at the fake guest and back.
- **2.5 vitals.go**: `Vitals` server-stream (interval). Test: the fake emits two GuestVitals samples; the Service forwards both.
- **2.6 watch.go + process.go**: `Watch` (FsEvent stream), `Processes` (ProcessList), `Signal`. Test: the fake emits a CREATE FsEvent; Processes returns a fake list; Signal forwards pid+signal.
- **2.7 errors**: a shared helper `connectErr(code, cause, remediation)` producing the LLM-legible detail. Test: a guest not-found surfaces `code="not_found"` with remediation text.

Each ends: `go test ./internal/sandboxrpc/` PASS, commit.

---

## Stage 3: Mount the Connect server (forkd + sandbox-server), JSON handlers delegate

### Task 3.1: Token interceptor

**Files:**
- Create: `internal/sandboxrpc/interceptor.go`, `internal/sandboxrpc/interceptor_test.go`

**Interfaces:**
- Produces: `func BearerInterceptor(lookup func(sandboxID string) (token string, ok bool)) connect.Interceptor` enforcing constant-time bearer match; `func AllowTokenless() connect.Interceptor` (sandbox-server).

- [ ] **Step 1: Failing test**

```go
func TestBearerInterceptorRejectsWrongToken(t *testing.T) {
	ic := BearerInterceptor(func(id string) (string, bool) { return "secret", true })
	err := callWith(ic, "Bearer wrong", &sandboxv1.ExecRequest{/* sandbox=s */})
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("want unauthenticated, got %v", err)
	}
}
```

- [ ] **Step 2: Run fail** -> **Step 3: implement (subtle.ConstantTimeCompare, fail-closed)** -> **Step 4: run pass** -> **Step 5: commit**

`git commit -s -m "feat(sandboxrpc): per-sandbox bearer token interceptor, fail-closed"`

### Task 3.2: forkd mounts the Connect server; JSON runtime handlers delegate

**Files:**
- Modify: `internal/daemon/sandbox_api.go` (mount connect handler; route runtime ops through the Service), `cmd/forkd/main.go` (wire), and add a `Deprecation` header on the JSON runtime routes.

**Interfaces:**
- Consumes: `sandboxrpc.Service`, `sandboxrpc.BearerInterceptor`.
- Produces: forkd :9091 serves both `/sandbox.v1.Sandbox/*` (Connect) and the existing `/v1/exec` etc. (delegating, deprecated).

- [ ] **Step 1: Failing test** (envtest or httptest): a Connect Exec call to the mounted handler with the right token streams stdout from a fake engine; a JSON `/v1/exec` call returns the same result AND a `Deprecation: true` header.
- [ ] **Step 2-4: implement the mount + delegation; the JSON handler builds an ExecOpen and calls the same Service path.**
- [ ] **Step 5: commit** `git commit -s -m "feat(daemon): serve the Connect Sandbox service on :9091; JSON runtime delegates"`

### Task 3.3: sandbox-server mounts the Connect server (tokenless)

**Files:** Modify `cmd/sandbox-server/main.go`.
- [ ] TDD: a Connect Exec against the standalone server works without a token; commit.

---

## Stage 4+5: Guest gRPC-over-vsock (the security-sensitive core, lands as ONE unit)

Stages 4 and 5 land together: the guest serves EITHER JSON-lines OR gRPC, not both, so the bridge (`GuestConn` real impl) and the guest server flip in one reviewed unit, gated by the KVM exec/PTY phase. This is the `guest/agent` rewrite; closest review.

### Task 4.1 (SPIKE): gRPC-over-vsock dialer

**Files:** Create `internal/vsock/grpcconn.go`, `internal/vsock/grpcconn_test.go`.
- [ ] Prove grpc-go (or connect-go over HTTP/2 h2c) can run over a vsock `net.Conn`: a tiny in-test gRPC server on a `net.Pipe`/vsock pair, a client dialing via a custom `grpc.WithContextDialer`. Test: a unary echo round-trips. If grpc-go dialing is awkward over vsock, fall back to connect-go over an h2c `net.Conn`; record the choice in the test file comment. Commit.

### Task 5.1: Guest serves the Sandbox gRPC service

**Files:** Modify `guest/agent/main.go` (replace JSON-lines accept loop with a gRPC server on the vsock listener); re-express handlers in `guest/agent/{exec,files,runcode,pty,portforward,vitals,watch,process}.go`.
- [ ] Re-express each existing guest handler as the proto RPC, preserving behavior and every security invariant (path sanitization, /workspace restriction, no secret in logs). Exec gains stdin + pty (open.pty). Add Watch (inotify), Processes (/proc), Signal. TDD each handler against an in-process gRPC client in the guest test suite. Multiple commits (one per handler group) but the guest only builds/serves after the loop is replaced; land the group together.

### Task 5.2: Guest serves the internal control service

**Files:** Create `guest/agent/internal_server.go`.
- [ ] Serve `internalv1.Control` (NotifyForked, Configure, Ping) on a distinct vsock service. Re-key the existing notifyforked/configure/ping handlers. Test: the fork-correctness reseed handshake still works over the new transport; a test asserts the PUBLIC Sandbox service exposes NO Configure/NotifyForked method. Commit.

### Task 5.3: The real GuestConn (vsock bridge) replaces the fake

**Files:** Modify `internal/sandboxrpc/guestconn.go` to add `vsockGuestConn` dialing the guest gRPC server via Task 4.1's dialer; wire forkd to use it.
- [ ] TDD against a real in-process guest gRPC server. Then the controller/forkd path uses `vsockGuestConn`. Commit.

### Task 5.4: KVM CI streaming exec + PTY phase

**Files:** Modify `.github/workflows/kvm-test.yaml`.
- [ ] Add a phase that boots a real guest, runs a streaming exec asserting incremental stdout, and drives an interactive PTY program (e.g. `cat` echo + resize), over BOTH vsock (in-guest path) and :9091 (bridge). Assert the fork-correctness reseed phase still passes. This is the acceptance gate for stages 4+5.

---

## Stage 6: SDK runtime cutover (all six)

### Task 6.1: Python direct-mode runtime -> Connect

**Files:** Modify `sdk/python/mitos/direct.py` + the streaming layer; keep the public API identical.
- [ ] TDD: the existing direct-mode tests (exec/files/pty/run_code) pass against a Connect server stub instead of the JSON stub. `make test-python` green, including `tests/test_e2b_compat.py` (e2b rides this unchanged). Commit.

### Task 6.2: TypeScript direct-mode runtime -> Connect

**Files:** Modify `sdk/typescript/src/http.ts` + `sandbox.ts`.
- [ ] TDD: `cd sdk/typescript && npm test` green against the Connect wire. Commit.

### Tasks 6.3 - 6.6: Go, Ruby, Rust, Java exec -> Connect

For each: migrate the ONE runtime call (`exec`) to Connect `Exec` over the SDK's existing HTTP stack (one-shot exec = server-streaming-after-open over HTTP/1.1 chunked). Update the SDK's test stub to the Connect wire shape. Keep create/fork/list/terminate (lifecycle REST) unchanged.
- **6.3 Go** (`sdk/go`): net/http supports HTTP/2; commit.
- **6.4 Ruby** (`sdk/ruby`): net/http streaming; commit.
- **6.5 Rust** (`sdk/rust`): ureq is HTTP/1.1 unary-only; if streaming the exec response is not feasible with ureq, add a minimal streaming-capable HTTP dep (record the choice in the README's dependency note) OR consume the Connect server-stream over chunked HTTP/1.1 if ureq exposes the body reader. Commit.
- **6.6 Java** (`sdk/java`): java.net.http.HttpClient streams; commit.

### Task 6.7: Conformance streaming + PTY scenarios

**Files:** Modify `sdk/conformance`.
- [ ] Add streaming-exec-increments and PTY-drive scenarios; Python and TS both pass them. Commit.

---

## Stage 7: Browser transport

### Task 7.1: Connect-Web reachability on :9091

**Files:** a small test client (e.g. `sdk/typescript` connect-web) hitting forkd :9091.
- [ ] TDD: a connect-web client streams exec stdout from :9091 with no proxy tier, bearer token via Authorization. Document the browser transport in docs/api/runtime-protocol.md. Commit.

---

## Stage 8: Budget-gated self-service RPCs

### Task 8.1: Fork/Checkpoint/ExtendLifetime/Budget wired to the controller

**Files:** Create `internal/sandboxrpc/selfservice.go`; wire to the controller fork path + `internal/captoken` attenuation + `api/v1` budget types.
- [ ] TDD: a `Fork` from within budget materializes a child `v1.Sandbox` owner-referenced to the caller; the N+1th beyond `maxForks` returns an LLM-legible `budget_exhausted` error naming the escalation path; the attenuation never widens (reuse the never-widen invariant tests). Commit.

### Task 8.2: In-guest self-service socket

**Files:** the guest exposes `MITOS_SOCKET`; the caller's attenuated token forwards to forkd over the internal channel.
- [ ] TDD: an in-guest `me.fork()` reaches the self-service RPC with the attenuated token; a guest with no budget gets the not-enabled/exhausted error. Commit.

---

## Stage 9: Remove the JSON-HTTP runtime API

### Task 9.1: Delete the JSON runtime handlers; keep lifecycle REST

**Files:** Modify `internal/daemon/sandbox_api.go` (remove `/v1/exec`, `/v1/exec/stream`, `/v1/run_code/stream`, `/v1/pty`, `/v1/files/*`, `/v1/vitals`, `/v1/sandboxes/{id}/forward`); modify `cmd/sandbox-server/main.go`; update `internal/daemon/pty.go`/`lifecycle_api.go` as needed (lifecycle pause/resume/set_timeout STAY).
- [ ] **Step 1:** Confirm all six SDKs and the e2b shim are on Connect (grep the SDKs for the removed routes; none remain).
- [ ] **Step 2:** Remove the runtime routes and their handlers + the now-dead JSON codec paths in `internal/vsock` (the JSON-lines protocol).
- [ ] **Step 3:** `go build ./... && make test-unit && go test ./internal/daemon/...` green; both lint invocations; guest cross-build.
- [ ] **Step 4:** Update docs: `docs/api/runtime-protocol.md` (mark the JSON runtime API removed, the Connect service primary), `docs/api/v2-spec.md` status, `docs/threat-model.md` (the guest protocol surface row now describes gRPC-over-vsock + the internal control service boundary), `CLAUDE.md` (forkd :9091 now serves the Connect Sandbox service; guest speaks gRPC over vsock).
- [ ] **Step 5:** commit `git commit -s -m "feat(daemon): remove the JSON-HTTP runtime API; Connect is the only runtime transport"`

### Task 9.2: Threat-model + docs delta and final sweep

- [ ] **Step 1:** repo-wide grep: no SDK or doc references the removed runtime routes (except the migration record).
- [ ] **Step 2:** the full gate: `go build ./... && go vet ./... && make test-unit && eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ && golangci-lint run --timeout=5m && GOOS=linux golangci-lint run --timeout=5m && GOOS=linux GOARCH=amd64 go build ./guest/agent/ && make test-python && (cd sdk/typescript && npm test)`.
- [ ] **Step 3:** the KVM streaming exec/PTY phase green (the #24 acceptance).
- [ ] **Step 4:** commit any fixups; open the PR.

---

## Self-review notes

- Spec coverage: proto additions (Stage 1), Service (Stage 2), mount + token + JSON delegation (Stage 3), guest gRPC flip + internal control channel + KVM gate (Stages 4+5), all six SDKs + conformance (Stage 6), browser (Stage 7), budget self-service (Stage 8), JSON runtime removal + docs/threat-model (Stage 9). Every spec section maps to a stage.
- Security: the internal control service (NotifyForked/Configure/ping) is isolated (Task 5.2) with a test asserting the public service cannot reach it; the token interceptor is fail-closed (Task 3.1); the guest rewrite preserves path/secret invariants (Task 5.1).
- The gRPC-over-vsock dialer is de-risked by a spike (Task 4.1) before the guest flip.
- Stages 4+5 are the one non-incrementally-green unit (the guest serves one protocol at a time), gated by the KVM exec/PTY phase, mirroring the v1 controller cutover.
- The four light SDKs migrate only `exec`; Rust is the dependency-footprint watch-item (Task 6.5).
