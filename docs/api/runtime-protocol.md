# Sandbox runtime protocol

Status: the proto contract is defined and compiles; the wire migration is not yet
active (see section 7).

This document is the normative companion to the `sandbox.v1.Sandbox` proto
(`proto/sandbox/v1/sandbox.proto`). It specifies the API v2 runtime protocol:
the imperative surface a caller has against ONE live sandbox (its own). It
cross-references docs/api/v2-spec.md section 4 (the runtime protocol chapter) and
ADR 0007 (the three-noun consolidation that fixes the surrounding object model).

The proto contract and its generated Go stubs are landed and compile
(`proto/sandbox/v1/`). The proto is not yet wired into any server: the current
JSON-over-HTTP sandbox API (forkd :9091) and the JSON-lines vsock protocol to the
guest agent remain the live transports and are unchanged. This doc fixes the
target the wire migration moves toward.

## 1. Why one protocol

Today the runtime surface is two ad-hoc protocols:

- forkd's HTTP sandbox API on :9091 (`internal/daemon/sandbox_api.go`): JSON
  request/response for exec and files, `application/x-ndjson` for streaming exec
  and run_code, and a WebSocket for PTY.
- the guest agent's vsock protocol (`internal/vsock`, `guest/agent`):
  newline-delimited JSON `Request`/`Response`, with dedicated connections
  carrying NDJSON frames for streaming exec, run_code, and PTY.

These work (streaming exec, PTY, files, and run_code are live on
raw-forkd). But they are two hand-rolled framings, two error shapes, and no
schema. v2 replaces both with ONE schema-first protocol, expressed once as the
`sandbox.v1.Sandbox` gRPC service, so a single contract serves every transport
and every SDK is generated from it.

## 2. Connect vs plain gRPC

The v2 spec names the runtime protocol "Connect" because the END STATE wants
Connect's HTTP semantics so a browser can stream exec output with no proxy tier
(docs/api/v2-spec.md section 4). The Mitos repo does NOT currently depend on
connect-go: go.mod has `google.golang.org/grpc` and `google.golang.org/protobuf`
and the `make proto` target runs plain `protoc` with `protoc-gen-go` and
`protoc-gen-go-grpc` (no buf, no `connectrpc.com/connect`). The existing
`proto/forkd.proto` is generated the same way.

Therefore the generated stubs are PLAIN gRPC (`sandbox.pb.go`,
`sandbox_grpc.pb.go`) to match the repo's existing toolchain exactly (same
plugin versions as the forkd stubs: protoc-gen-go v1.36.11, protoc-gen-go-grpc
v1.6.2). The Connect transport binding is a deliberate follow-up: Connect speaks
the same proto service, so adding `protoc-gen-connect-go` (or buf) later emits an
additional handler set from the SAME `sandbox.proto` without changing the
contract. The browser transport (section 3) is where Connect earns its place;
the vsock and cluster-internal transports are plain gRPC either way.

## 3. Three transports, one service

The same `sandbox.v1.Sandbox` service rides three transports. The service
definition is transport-agnostic; only the dialing and the credential differ.

| Transport | Path | Who dials | Credential | Status |
| --- | --- | --- | --- | --- |
| vsock (in-guest) | host forkd <-> guest agent over vsock (CID 3, port 53) | forkd | none (vsock is the in-VM trust boundary) | gRPC on vsock port 53 is the SOLE runtime protocol; the legacy JSON-lines protocol and the Go agent were removed (#310) |
| cluster-internal | SDK / controller <-> forkd :9091 | in-cluster client | per-sandbox bearer token (today), attenuated capability token (planned) | the HTTP sandbox API is live; the gRPC/Connect binding is a follow-up |
| browser | Paperclip UI <-> forkd edge | browser | scoped token | Connect HTTP semantics, follow-up; no current equivalent |

forkd is the bridge: it terminates the cluster-internal and browser transports at
its edge and relays to the guest agent over vsock. The vsock hop already speaks
the one `Sandbox` service end to end: forkd relays to the Rust guest agent's gRPC
server on vsock port 53 (`internal/daemon/sandbox_api.go`), and the
cluster-internal and browser bindings are the remaining follow-ups.

## 4. Endpoint mapping (current surface -> v2 RPC)

Every current runtime endpoint maps onto one `sandbox.v1.Sandbox` RPC. This table
is the migration's normative correspondence; each row is a unit the follow-up
slices port.

### HTTP sandbox API (forkd :9091)

| Current endpoint | v2 RPC | Notes |
| --- | --- | --- |
| `POST /v1/exec` | `Exec` (bidi, no stdin, read to exit) | one-shot exec is a degenerate Exec stream |
| `POST /v1/exec/stream` (x-ndjson) | `Exec` (bidi) | stdout/stderr chunks + exit map onto `ExecResponse` |
| `GET /v1/pty` (WebSocket) | `Exec` with `open.pty` set | input/resize/output/exit frames map onto `ExecRequest.stdin`/`resize` and `ExecResponse.stdout`/`exit` |
| `POST /v1/run_code/stream` | `Exec` (follow-up: a run_code-specific extension or a kernel-mode `ExecOpen`) | the stateful-kernel result/error frames are richer than plain exec; tracked as a follow-up shape decision, see section 7 |
| `POST /v1/files/read` | `ReadFile` -> stream `Chunk` | streamed instead of buffered |
| `POST /v1/files/write` | `WriteFile` (stream) | streamed instead of buffered |
| `POST /v1/files/list` | `List` | gains AIP-158 `page_size`/`page_token`/`filter` |
| `POST /v1/files/mkdir` | (covered by `WriteFile` semantics / a follow-up `Mkdir` shape) | not in the v2 spec's RPC list; modeled as a follow-up if kept distinct |
| `POST /v1/files/remove` | `Signal`-adjacent? no: a follow-up `Remove` shape | not in the v2 spec's RPC list; tracked as a follow-up |
| `GET /v1/metering` | (control-plane, not runtime) | stays on the node metering surface, out of scope for `Sandbox` |

### vsock guest-agent protocol (`internal/vsock`)

| Current message type | v2 RPC | Notes |
| --- | --- | --- |
| `exec` / `exec_stream` | `Exec` | merged into the one bidi Exec |
| `pty` | `Exec` with `open.pty` | one stream carries the interactive terminal |
| `run_code` | `Exec` (follow-up kernel mode) | see section 7 |
| `read_file` | `ReadFile` | |
| `write_file` | `WriteFile` | |
| `list_dir` | `List` | |
| `mkdir` / `remove` | follow-up `Mkdir`/`Remove` shapes | not in the v2 spec RPC list |
| `tar_dir` | `Archive` (DOWNLOAD) | |
| `untar_dir` | `Archive` (UNTAR upload, follow-up streaming RPC) | section 7 |
| `configure` | (control-plane: claim-time env/secrets) | stays on the fork/configure path, not a runtime RPC |
| `notify_forked` | (control-plane: fork repair) | stays on the fork path, not a runtime RPC |
| `ping` | (transport health) | gRPC has its own keepalive; no app-level RPC |

### New surface in v2 (no current equivalent)

`Stat`, `Watch`, `Processes`, `Signal`, `PortForward`, `Vitals`, and the
budget-gated `Fork`, `Checkpoint`, `ExtendLifetime`, `Budget`. These are
specified in the proto now and implemented in follow-up slices. `Signal` to pid 1
is how `me.exit()` is modeled (docs/api/v2-spec.md section 4: "me.exit()
terminates only the caller").

## 5. Budget-gated self-service (documented, not yet implemented)

`Fork`, `Checkpoint`, `ExtendLifetime`, and `Budget` are the self-service RPCs a
sandbox runs on its OWN lineage (docs/api/v2-spec.md section 3). They reference
the capability-budget shapes. The runtime accepts the call, debits a
capability budget, and MATERIALIZES a declarative object the controller
reconciles; the RPC returns a handle (`Operation`, `Revision`, `Lease`), not a
finished result. This preserves the one v2 rule: anything that creates,
multiplies, or destroys infrastructure still becomes a declarative object the
controller owns; the agent gets agency, the ledger stays complete.

These RPCs are contract-only today. The proto defines their requests,
their handle responses, and the `BudgetStatus`/`Allowance` shapes; the
controller-materializes-objects behavior is documented here and implemented when
the capability budgets and attenuated tokens land.

## 6. Versioning policy

The runtime protocol versions INDEPENDENTLY of the forkd control-plane proto
(`proto/forkd.proto`, package `forkd`). The runtime proto lives under a versioned
package, `sandbox.v1` (`proto/sandbox/v1/sandbox.proto`), so a breaking change
becomes `sandbox.v2` served alongside `sandbox.v1`.

- Within a major version, only backward-compatible changes are allowed: new
  fields, new RPCs, new enum values. Field numbers and meanings are never reused.
- A breaking change bumps the package major (`sandbox.v2`) and is served
  CONCURRENTLY with the previous major for a one-major-version compatibility
  window, so an older SDK keeps working while callers migrate. After the window,
  the older major is removed.
- The forkd control-plane proto can move on its own cadence; the two are not
  coupled.

## 7. Migration plan

The contract, the generated stubs, and this design are in place. The wire
migration proceeds in the following stages, each gated by the sequencing rule
(fork-correctness and failure/GC suites green in CI before integration ships to
production tenants):

1. Guest agent gRPC server: the guest agent serves `sandbox.v1.Sandbox` over
   vsock alongside (then instead of) the JSON-lines protocol. Resolve the
   run_code rich-result and `Mkdir`/`Remove`/`Archive(UNTAR)` shapes that the v2
   spec's RPC list does not enumerate, extending the proto compatibly.
2. forkd bridge: forkd terminates the cluster-internal transport as gRPC on its
   edge and relays to the guest agent's gRPC over vsock, replacing the HTTP
   sandbox API relay. The HTTP API stays mounted until SDKs cut over.
3. Browser transport: add the Connect handler set (protoc-gen-connect-go or buf)
   from the same `sandbox.proto` so the Paperclip UI streams Exec/Vitals with no
   proxy tier (section 2, section 3).
4. Budget-gated RPCs: wire `Fork`/`Checkpoint`/`ExtendLifetime`/`Budget` to the
   capability-budget ledger and controller materialization.
5. KVM e2e: real Firecracker exec/PTY/files over the new protocol on the KVM
   runners (kvm-test.yaml), proving the vsock gRPC path end to end.

Each stage keeps the prior transport live until its consumers cut over; nothing
removes or changes the existing HTTP/vsock surface yet.

## 8. Security surface

The runtime protocol does not move the threat surface today: no server is
wired, no new port is opened, no credential path changes. When the follow-up
stages bind a transport, each updates docs/threat-model.md in the same PR (the
forkd edge already gates the runtime surface with the per-sandbox bearer token;
attenuated capability tokens replace it). Secret-carrying fields
in the proto (`ExecOpen.env` values, `ConfigureRequest`-equivalent control-plane
paths) keep the existing rule: values are never logged, only keys and counts.
