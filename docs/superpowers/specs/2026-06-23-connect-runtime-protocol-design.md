# Design: API v2 Connect runtime protocol (#24)

Date: 2026-06-23
Issue: #24 (Connect runtime protocol). Epic: #22. Related: docs/api/v2-spec.md
section 4, docs/api/runtime-protocol.md, proto/sandbox/v1/sandbox.proto,
docs/api/errors.md (#28 LLM-legible errors), #25 (capability budgets, closed).

## Goal

Replace today's two ad-hoc runtime transports, the JSON-over-HTTP sandbox API on
forkd :9091 and the JSON-lines vsock protocol the guest agent speaks, with ONE
`sandbox.v1.Sandbox` gRPC/Connect service reached over three transports: vsock
(in-guest), cluster-internal (forkd :9091), and browser (Connect-Web, no proxy
tier). The proto and generated stubs already exist
(`proto/sandbox/v1/sandbox.proto`, `proto/sandbox/v1/sandboxv1connect/`); this
work implements the server, migrates the guest agent, cuts ALL SIX SDKs over,
wires the budget-gated self-service RPCs, and REMOVES the old JSON-HTTP runtime
API. The e2b drop-in rides the Python SDK's cutover. (The sandbox-server
lifecycle REST, create/fork/list/terminate, is a separate management API and is
unchanged.)

Acceptance (from #24): an SDK exec streams stdout incrementally over all three
transports; PTY mode drives an interactive program; the old HTTP shape is shimmed
with a deprecation note.

## Decisions taken (constraints for this work)

1. The old JSON-HTTP RUNTIME sandbox API (/v1/exec, /v1/files/*, /v1/pty,
   /v1/run_code, /v1/vitals, /v1/sandboxes/{id}/forward) is REMOVED, not shimmed.
   ALL SIX SDKs migrate their runtime calls to the Connect Sandbox service in this
   work, and the JSON-HTTP runtime handlers are deleted once they are gone.
   (Scope note: the sandbox-server LIFECYCLE REST, create template / fork / list /
   terminate, is a separate management API, out of #24 scope and unchanged.) The
   four stdlib-light SDKs (Go, Ruby, Rust, Java) today make only ONE runtime call,
   exec; it becomes the Connect Exec RPC. To preserve their minimal-dependency
   posture, they speak the Connect protocol over their existing HTTP stack where
   feasible (a one-shot exec is server-streaming-after-open, expressible over
   HTTP/1.1 chunked). The one watch-item is Rust (ureq is HTTP/1.1 unary-only),
   which may need a small streaming-capable HTTP dependency. The e2b drop-in rides
   the Python SDK's Connect cutover (sdk/python/mitos/e2b.py is layered on
   mitos.direct), and its run_code maps onto the new RunCode RPC.
2. `run_code` gets its OWN streaming RPC (`RunCode`), not an `Exec` overload: its
   Jupyter-style structured result/error frames do not fit `Exec`'s
   stdout/stderr/exit shape.
3. Lifecycle controls (`set_timeout`, `pause`, `resume`) are NOT runtime RPCs;
   they are control-plane operations (#218) and stay on their current path.
4. The fork-correctness handshake (`NotifyForked`), claim-time secret delivery
   (`Configure`), and `ping` stay on an INTERNAL forkd-to-guest channel, NOT the
   public `Sandbox` service. They are host-trusted control messages, never
   exposed to an SDK or browser.

## Non-goals

- The sandbox-server LIFECYCLE REST (create template / fork / list / terminate)
  is unchanged; #24 is the runtime exec/files protocol only.
- Cluster mode for Go/Ruby/Rust/Java (that is #303-#306); this work migrates only
  their direct-mode runtime exec call.
- gRPC mTLS between controller and forkd (that channel is :9090, unchanged).

## Architecture: one service, three transports

The `sandbox.v1.Sandbox` service is implemented once, against the engine and the
guest. Three transports mount it:

- **vsock (in-guest, source of truth).** The guest agent runs a gRPC server on
  its vsock port (replacing the JSON-lines accept loop). Every exec/file/pty/etc.
  operation executes here, inside the VM. No auth (the in-VM boundary is the
  microVM itself).
- **cluster-internal (forkd :9091).** forkd runs a Connect/gRPC server that
  BRIDGES each RPC to the guest over vsock gRPC. A Connect interceptor enforces
  the per-sandbox bearer token (constant-time compare, today's mechanism). This
  is what the SDKs and the controller status path reach.
- **browser.** Connect-Web reaches forkd :9091 directly over HTTP(S); Connect is
  browser-native, so exec streams to a UI with no proxy tier. Same bearer token
  via `Authorization`.

Data path for a cluster-internal exec: SDK -> Connect client -> forkd :9091
(Connect server, token interceptor) -> vsock gRPC client -> guest gRPC server ->
exec. The bridge is transport-only: forkd does not re-interpret the stream, it
proxies the bidi frames.

## Proto additions

The existing 16 RPCs stay. Add to `proto/sandbox/v1/sandbox.proto`:

- `rpc RunCode(stream RunCodeRequest) returns (stream RunCodeResponse);` with
  `RunCodeOpen` (code, language, timeout_seconds) and a `RunCodeResponse` oneof
  of `stdout`/`stderr` chunks, a `Result` frame (text + map<string,bytes> mime ->
  payload, the Jupyter rich output), an `Error` frame (name, value, traceback[]),
  and `exit`. Stateful kernel semantics (the kernel persists across calls in a
  sandbox) are documented on the RPC.
- `rpc Mkdir(MkdirRequest) returns (MkdirResponse);` and
  `rpc Remove(RemoveRequest) returns (RemoveResponse);` (the two file ops present
  in today's API but absent from the proto).
- Extend `Archive` to a client-streamed UNTAR: keep `Archive(ArchiveRequest)
  returns (stream Chunk)` for DOWNLOAD, add `rpc Upload(stream UploadRequest)
  returns (UploadResult)` for streamed untar (replacing the buffered untar_dir).

Regenerate stubs via `make proto` (extend it to cover proto/sandbox/v1, not only
forkd.proto).

## Components

### internal/sandboxrpc (new): the service implementation + vsock bridge

- `Service` implements the generated `sandbox.v1.Sandbox` server interface
  against a small engine-facing port interface (`GuestConn`): one method per
  logical operation that opens a vsock gRPC stream to the guest. This is the
  single implementation forkd and sandbox-server both mount.
- `vsockBridge`: given a sandbox id, dials the guest's vsock gRPC server and
  proxies the RPC. Reuses `internal/vsock` dialing; the JSON-lines codec is
  replaced by gRPC-over-vsock (a `net.Conn` from the vsock dialer handed to
  grpc.NewClient with a custom dialer).
- One file per RPC group (exec.go, files.go, runcode.go, portforward.go,
  vitals.go, watch.go, process.go, selfservice.go) so each unit is focused and
  testable against a fake guest.

### guest/agent: gRPC server over vsock

- Replace the JSON-lines accept loop (`guest/agent/main.go`) with a gRPC server
  bound to the vsock listener serving the `Sandbox` service.
- Re-express each existing handler as the proto RPC: exec_stream -> `Exec`,
  pty -> `Exec` with `open.pty`, kernel -> `RunCode`, tunnel -> `PortForward`,
  read/write/list/stat/mkdir/remove -> the file RPCs, tar_dir -> `Archive`,
  untar_dir -> `Upload`, vitals -> `Vitals`, plus the NEW `Watch` (inotify),
  `Processes` (/proc), `Signal`.
- The INTERNAL control channel (NotifyForked, Configure, ping) stays a separate,
  small server on a distinct vsock service (`sandbox.internal.v1`), reachable
  only by forkd, never bridged to :9091. This preserves the secret-delivery and
  fork-handshake trust boundary.
- Security: this is `guest/agent`, a named security-sensitive path. The rewrite
  preserves every existing invariant: no secret value logged, path-traversal
  sanitization on file ops and Upload, the /workspace restriction on Archive,
  the per-sandbox token never seen by the guest (auth is host-side only).

### forkd + sandbox-server: mount the Connect server

- forkd serves the Connect/gRPC `Sandbox` service on :9091 alongside the
  deprecated JSON-HTTP shim, with the bearer-token Connect interceptor.
- sandbox-server (standalone, tokenless by default) mounts the same Service.
- During the SDK cutover the JSON-HTTP runtime handlers
  (internal/daemon/sandbox_api.go) delegate to the same Service methods (no
  duplicate engine logic) and carry a `Deprecation` header; once all six SDKs are
  on Connect (stage 9) the runtime routes are REMOVED. The lifecycle REST routes
  stay.

### Budget-gated self-service RPCs

- `Fork`, `Checkpoint`, `ExtendLifetime`, `Budget` are served by forkd but
  MATERIALIZE control-plane objects: `Fork` creates a child `v1.Sandbox`
  (owner-referenced to the caller) via the controller path, gated on the caller's
  remaining budget (the never-widen attenuation, #25 core in internal/captoken +
  api/v1 budget types). The caller reaches them via the in-guest self-service
  socket (`MITOS_SOCKET`) using its attenuated token; the guest forwards to forkd
  over the internal channel. Budget exhaustion returns an LLM-legible error
  (#28) naming the orchestrator escalation path.

### SDKs (all six)

- Python + TypeScript: swap the direct-mode runtime transport (sdk/python/mitos/
  direct.py + sandbox.py streaming; sdk/typescript/src/http.ts + sandbox.ts) to
  the Connect client (connectrpc for both), covering the full runtime surface
  (exec/files/pty/run_code/vitals/port_forward). Public SDK API unchanged; cluster
  mode untouched.
- Go, Ruby, Rust, Java: migrate the ONE runtime call they make, exec, to the
  Connect Exec RPC. Keep the minimal-dependency posture by speaking Connect over
  the SDK's existing HTTP stack (a one-shot exec is server-streaming-after-open
  over HTTP/1.1 chunked: Go net/http, Ruby net/http, Java HttpClient all stream;
  Rust ureq is HTTP/1.1 unary-only and may take a small streaming HTTP dep). Their
  lifecycle calls (create/fork/list/terminate) are unchanged. Each SDK's test
  stub is updated to the Connect wire shape.
- The shared conformance suite (sdk/conformance) gains the streaming exec + PTY
  scenarios so Python and TS stay at parity.
- The e2b drop-in (sdk/python/mitos/e2b.py) is NOT modified: it is built on
  DirectSandbox, so the transport cutover carries it onto Connect unchanged. Its
  run_code path now resolves to the dedicated RunCode RPC (richer result/error
  fidelity). tests/test_e2b_compat.py is the regression guard that its E2B-shaped
  surface still behaves identically across the transport change.

## Error model (#28)

Every RPC error is a Connect error whose detail carries the normative
LLM-legible envelope (code, message, cause, remediation) from docs/api/errors.md,
so an agent can branch on `code` and read `remediation`. The JSON-HTTP shim keeps
returning the same envelope shape it does today.

## Testing

- TDD per behavior. The `guest/agent` rewrite is the highest-risk slice and gets
  the closest review (a named human security reviewer per CLAUDE.md).
- Unit: the Service against a fake `GuestConn`; the guest handlers against an
  in-process gRPC client; the token interceptor (fail-closed, constant-time).
- KVM CI (kvm-test.yaml): a NEW phase proving streaming exec increments and an
  interactive PTY drive over the real guest, end to end over vsock and over
  :9091. Plus the existing fork-correctness handshake still green (it moved to
  the internal channel).
- Conformance: Python and TS run the shared streaming + PTY scenarios.
- Both lint invocations; `GOOS=linux GOARCH=amd64 go build ./guest/agent/`.

## Implementation staging (one spec, staged plan)

1. Proto additions (RunCode, Mkdir/Remove, Upload) + `make proto` covers
   proto/sandbox/v1; stubs regenerate.
2. `internal/sandboxrpc.Service` against a fake `GuestConn`, all RPCs, unit
   tested (no transport yet).
3. forkd + sandbox-server mount the Connect server with the token interceptor;
   the JSON-HTTP handlers delegate to the Service and gain the Deprecation
   header. SDKs still on JSON-HTTP here; both paths exercise the same Service.
4. vsock gRPC bridge: the Service's `GuestConn` dials the guest. Guest still
   JSON-lines at this point is NOT possible (one or the other), so stage 4 lands
   WITH stage 5.
5. guest/agent gRPC-over-vsock migration (with the internal control channel for
   NotifyForked/Configure/ping). Stages 4+5 land together (the guest transport
   flips); KVM CI streaming exec + PTY phase added.
6. SDK runtime cutover to Connect for all six: Python + TS full runtime surface;
   Go/Ruby/Rust/Java exec via Connect over their HTTP stack (Rust may take a small
   streaming HTTP dep). Python+TS conformance streaming/PTY scenarios.
7. Browser transport: Connect-Web reachability on :9091 proven (a small browser
   or connect-web client test streaming exec).
8. Budget-gated self-service RPCs (Fork/Checkpoint/ExtendLifetime/Budget) wired
   to the controller + in-guest socket with attenuated tokens.
9. REMOVE the JSON-HTTP runtime handlers (the runtime routes in
   internal/daemon/sandbox_api.go and the matching sandbox-server routes) now that
   all six SDKs are on Connect; document the removed routes and the new RPCs
   (Watch/Processes/Signal/Upload/Mkdir/Remove). The lifecycle REST stays.

Stages 4+5 are the security-sensitive core (guest protocol flip) and the only
ones that cannot land incrementally green per-package; they land as one unit
gated by the KVM CI streaming/PTY phase, like the controller cutover in the v1
migration.

## Risks

- The guest transport flip (stages 4+5) is all-or-nothing per the guest: a
  half-migrated guest serves neither protocol. Mitigation: land it as one
  reviewed unit gated by the KVM exec/PTY phase; keep the internal control
  channel (NotifyForked/Configure) working throughout (fork-correctness must stay
  green).
- gRPC-over-vsock: grpc-go needs a custom dialer over the vsock `net.Conn`;
  unproven here. Mitigation: a focused spike/test in stage 4 before the full
  guest cutover; fall back to connectrpc-over-vsock if grpc-go dialing is awkward.
- Secret-delivery boundary: moving Configure to a separate internal service must
  not widen who can call it. Mitigation: the internal service binds a distinct
  vsock service name reachable only from forkd; a test asserts the public Sandbox
  service has no Configure/NotifyForked method.
- SDK behavior drift: the Connect client must reproduce the exact streaming
  semantics (incremental stdout, PTY resize) the JSON path had. Mitigation: the
  conformance suite runs the same scenarios against both transports during the
  cutover.
