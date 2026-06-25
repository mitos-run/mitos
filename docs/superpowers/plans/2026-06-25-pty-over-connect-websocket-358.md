# PTY over Connect WebSocket (close #358) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move interactive PTY off the legacy `/v1/pty` JSON WebSocket onto the `sandbox.v1` Exec schema carried over a WebSocket transport (Connect-enveloped frames), then remove the now-dead `/v1` runtime routes, closing #358.

**Architecture:** PTY is the last SDK runtime call still on `/v1`; every other call (exec, exec-stream, files, run_code) is already on Connect across all six SDKs. Interactive PTY needs full duplex, which the thin half-duplex-over-HTTP/1.1 Connect clients cannot do against the bidi `Exec` (Connect bidi is HTTP/2 only). So we carry the bidi `Exec` over a WebSocket: each binary ws message is one Connect-enveloped frame (a 5-byte header then the JSON-encoded `sandbox.v1.ExecRequest` from the client / `ExecResponse` from the server). The host already bridges a ws to the guest gRPC `Exec` stream in `internal/daemon/pty.go` (`ExecPTY`, stdin, resize, output); this plan swaps that handler's bespoke `vsock.PtyFrame` codec for the Connect-enveloped `sandbox.v1` codec and mounts it on a Connect-named path. The Python and TypeScript SDKs gain a ws transport that reuses the exact envelope codec already proven for their half-duplex Connect calls; the only delta is the transport is a full-duplex WebSocket instead of an HTTP/1.1 request.

**Tech Stack:** Go (`internal/daemon`, `github.com/coder/websocket` already vendored, connect-go, the `sandbox.v1` protos), Python (`websockets` or the stdlib; mirror `sdk/python/mitos/_connect.py`), TypeScript (browser/node `WebSocket`; mirror `sdk/typescript/src/connect.ts`).

## Global Constraints

- No em (U+2014) or en (U+2013) dashes anywhere: source, comments, docstrings, Markdown, YAML, commit and PR messages. Only `. , ; :` connectors; ASCII hyphen for ranges/compounds.
- Conventional commits; every commit carries `Signed-off-by` (`git commit -s`). Stage explicit paths only; never `git add -A`.
- Go: `fmt.Errorf("context: %w", err)`; octal `0o644`; gofmt + golangci-lint clean (BOTH `golangci-lint run` and `GOOS=linux golangci-lint run`).
- Secret hygiene: token VALUES never logged, never in error/condition messages. Log keys and counts only.
- `internal/daemon` is a security-sensitive path: Tasks 1 and 4 require a named human reviewer before merge and a `docs/threat-model.md` delta in the same PR.
- TDD: the failing test lands in the same commit as the behavior it covers.
- The enveloped frame format is the existing Connect framing: byte 0 is a flags bitfield (`0x02` = end-stream), bytes 1 to 4 are the big-endian uint32 payload length, then the JSON message bytes. This is the same format `sdk/python/mitos/_connect.py` `_encode_frame` / `_iter_frames` and `sdk/typescript/src/connect.ts` already implement; reuse those, do not invent a second framing.

---

## File Structure

- `internal/daemon/exec_ws.go` (new): the Connect-over-WebSocket bidi `Exec` handler. Upgrades to ws, authenticates, decodes client `ExecRequest` envelopes (open/stdin/resize), bridges to the guest `Exec` stream (reusing `ExecPTY`/`vsockGuestConn`), encodes guest output as `ExecResponse` envelopes. Replaces the wire half of `pty.go`.
- `internal/daemon/exec_ws_test.go` (new): ws client tests (open a pty, receive output + exit; stdin round-trip; resize accepted; auth rejects).
- `internal/daemon/sandbox_api.go` (modify `Handler()` ~545-575): mount the new ws Exec route; in Task 4 delete the deprecated `/v1` runtime route registrations.
- `internal/daemon/pty.go` (delete in Task 4 once SDKs are migrated): the bespoke-codec handler.
- `docs/threat-model.md` (modify in Tasks 1 and 4): the PTY transport surface row.
- `docs/api/v2-spec.md` and `docs/api/*` (modify in Task 4): note PTY rides the Connect `Exec` schema over a WebSocket transport.
- `sdk/python/mitos/_connect_ws.py` (new): the ws-Connect transport (sync) reusing `_connect.py` framing; `sdk/python/mitos/pty.py`, `direct.py`, `aio.py`, `sandbox.py` (modify): build the PTY session URL/codec off the ws-Connect transport instead of `/v1/pty`.
- `sdk/typescript/src/connect_ws.ts` (new): the ws-Connect transport reusing `connect.ts` framing; `sdk/typescript/src/pty.ts`, `sandbox.ts` (modify): route PTY through it.

## Task ordering and PR slices

Each task is its own PR. Task 1 (host transport) lands first so the SDKs have a server to speak to. Tasks 2 and 3 (Python, TS) are independent of each other and can run in parallel once Task 1 is merged. Task 4 (remove dead `/v1` routes + old PTY handler) lands only after Tasks 2 and 3 ship, so no SDK is left calling a removed route. Task 5 closes the issue.

---

## Task 1: Host Connect-over-WebSocket bidi Exec endpoint

**Files:**
- Create: `internal/daemon/exec_ws.go`
- Test: `internal/daemon/exec_ws_test.go`
- Modify: `internal/daemon/sandbox_api.go` (`Handler()`, add the route)
- Modify: `docs/threat-model.md` (PTY transport surface row)

**Interfaces:**
- Consumes: `(*SandboxAPI).ptyAuth(w, r) (string, bool)`; `(*SandboxAPI).resolveSandboxID`, `.checkSandboxRegistered`, `.touch`, `.auditor.Record`; `newVsockGuestConn(api, sandbox).(*vsockGuestConn)` and its `ExecPTY(ctx, *sandboxv1.ExecOpen) (*grpcExecStream, error)` returning a handle with `.stream.Send(*sandboxv1.ExecRequest)`, `.Recv() (mappedFrame, error)`, `.Close()`, `.cc.Close()` (all already used by `pty.go`); `sandboxv1.ExecRequest`/`ExecResponse`/`ExecOpen`/`PtyOptions`/`WindowSize`.
- Produces: `(*SandboxAPI).handleExecWS(w http.ResponseWriter, r *http.Request)` mounted at `GET /sandbox.v1.Sandbox/Exec` with a WebSocket upgrade; ws subprotocol constant `execWSSubprotocol = "connect.sandbox.v1"`; envelope helpers `encodeEnvelope(flags byte, msg proto.Message) ([]byte, error)` and `decodeEnvelope([]byte) (flags byte, payload []byte, err error)` using the framing in Global Constraints. The client sends binary ws messages each holding one enveloped `ExecRequest`; the first MUST carry the `open` oneof (with `pty` set for an interactive terminal), subsequent messages carry `stdin` or `resize`. The server sends binary ws messages each holding one enveloped `ExecResponse`; the terminal frame sets the end-stream flag and carries the `exit` oneof.

- [ ] **Step 1: Write the failing test** for a pty session round-trip: a ws client connects with the bearer token and `X-Sandbox-Id`/`?sandbox=`, sends an enveloped `ExecRequest{open:{pty:{size:{cols,rows}}}}`, and receives at least one enveloped `ExecResponse` stdout frame then a terminal `ExecResponse{exit:{code:0}}` with the end-stream flag, from a fake guest (reuse the `recordingExecGuest`/`gatedExecStream` pattern in `internal/sandboxrpc/execstream_test.go` and the ws dial pattern in `internal/daemon/pty_test.go`). Assert the auth path: a missing/wrong token yields a pre-upgrade 401, never a post-upgrade close.

- [ ] **Step 2: Run it, confirm it fails** (handler undefined). Run: `go test ./internal/daemon/ -run TestExecWS -v`.

- [ ] **Step 3: Implement `handleExecWS`** by adapting `handlePty`: keep the auth, `checkSandboxRegistered`, pre-upgrade `ExecPTY` slot acquisition, and the two-pump bridge verbatim; replace the read pump's `vsock.PtyFrame` JSON decode with `decodeEnvelope` then `proto`-unmarshal to `ExecRequest`, switching on the oneof (`open` is the first frame and already consumed to build `ExecOpen`; `stdin` -> `stream.Send(ExecRequest_Stdin)`; `resize` -> `stream.Send(ExecRequest_Resize)`); replace the write pump's `PtyFrame` JSON encode with `encodeEnvelope` of an `ExecResponse` (`stdout`/`stderr` chunk; terminal `exit` with end-stream flag). Take cols/rows from the first `ExecOpen.pty.size` rather than the query string. Use binary ws messages (`websocket.MessageBinary`).

- [ ] **Step 4: Run the test, confirm it passes.** Run: `go test ./internal/daemon/ -run TestExecWS -v`.

- [ ] **Step 5: Mount the route** in `Handler()`: `outerMux.Handle("GET /sandbox.v1.Sandbox/Exec", http.HandlerFunc(api.handleExecWS))` on the same outer mux as the Connect service (outside `requireBearer`, like the PTY route today). Keep the existing `/v1/pty` route untouched in this task so nothing breaks mid-migration.

- [ ] **Step 6: Update `docs/threat-model.md`**: revise the PTY/interactive-exec row to describe the ws transport carrying the `sandbox.v1` Exec schema, same per-sandbox bearer auth, same concurrent-stream cap acquired pre-upgrade; note input bytes and resize are the only client-controlled fields and no command is taken from the client (guest defaults to `/bin/sh`), matching today.

- [ ] **Step 7: Lint + full daemon tests.** Run: `gofmt -l internal/daemon`, `golangci-lint run --timeout=5m`, `GOOS=linux golangci-lint run --timeout=5m`, `go test ./internal/daemon/`.

- [ ] **Step 8: Commit.** `git add internal/daemon/exec_ws.go internal/daemon/exec_ws_test.go internal/daemon/sandbox_api.go docs/threat-model.md` then `git commit -s -m "feat(daemon): serve bidi Exec (PTY) over a Connect WebSocket transport (#358)"`. Open the PR; request the named daemon reviewer.

---

## Task 2: Python SDK PTY over the ws-Connect transport

**Files:**
- Create: `sdk/python/mitos/_connect_ws.py`
- Modify: `sdk/python/mitos/pty.py`, `direct.py`, `aio.py`, `sandbox.py`
- Test: `sdk/python/tests/test_pty_connect.py` (new) plus the existing pty tests

**Interfaces:**
- Consumes: the framing helpers in `_connect.py` (`_encode_frame`, `_iter_frames`/`FrameReader`, `_FLAG_END_STREAM`, the `_path` builder) and the `sandbox.v1` message dicts; the host route `GET /sandbox.v1.Sandbox/Exec` (ws) from Task 1.
- Produces: a `PtyConnectSession` exposing the same public surface the current `/v1/pty` session exposes (the methods `pty.py` already offers: send input bytes, send resize, iterate output, read exit code), so `pty.py`/`direct.py`/`aio.py`/`sandbox.py` swap their transport without changing their public method signatures.

- [ ] **Step 1: Write the failing test**: a fake ws server that speaks the enveloped `ExecRequest`/`ExecResponse` protocol (accept an `open{pty}` frame, echo a stdout frame, then a terminal `exit` frame). Assert the session yields the stdout bytes and the exit code, and that an input write is delivered as an enveloped `ExecRequest{stdin}`. Run: `cd sdk/python && PYTHONPATH=. python3 -m pytest tests/test_pty_connect.py -v` and confirm it fails.

- [ ] **Step 2: Implement `_connect_ws.py`**: open a WebSocket to `{ws_base}/sandbox.v1.Sandbox/Exec` with `Authorization: Bearer` and the sandbox id header (the SDK can set ws headers; mirror the auth the current `/v1/pty` client sends), subprotocol `connect.sandbox.v1`; send the first frame `_encode_frame(json(ExecRequest{open:{pty:{size}}}))`, then pump input/resize as enveloped `ExecRequest` frames and decode server `ExecResponse` frames with the `_connect.py` reader. Keep it dependency-light (prefer the `websockets` package already used by the legacy PTY client; reuse whatever `pty.py` imports today).

- [ ] **Step 3: Re-point `pty.py`/`direct.py`/`aio.py`/`sandbox.py`** PTY entry points at `PtyConnectSession`; delete the `/v1/pty` URL builders (`return f"{ws_base}/v1/pty?..."`).

- [ ] **Step 4: Run the new test and the full suite.** Run: `cd sdk/python && PYTHONPATH=. python3 -m pytest tests/ -q`. Expected: all pass (the README claims 247; this must not regress).

- [ ] **Step 5: Commit.** `git add sdk/python/mitos/_connect_ws.py sdk/python/mitos/pty.py sdk/python/mitos/direct.py sdk/python/mitos/aio.py sdk/python/mitos/sandbox.py sdk/python/tests/test_pty_connect.py` then `git commit -s -m "feat(sdk/python): run PTY over the Connect WebSocket transport, retire /v1/pty (#358)"`.

---

## Task 3: TypeScript SDK PTY over the ws-Connect transport

**Files:**
- Create: `sdk/typescript/src/connect_ws.ts`
- Modify: `sdk/typescript/src/pty.ts`, `sandbox.ts`
- Test: `sdk/typescript/test/pty_connect.test.ts` (new) plus existing pty tests

**Interfaces:**
- Consumes: the framing in `connect.ts` (the `FrameReader` and envelope encoder, `application/connect+json` codec) and the `sandbox.v1` message types; the host ws route from Task 1.
- Produces: a `PtyConnectSession` matching the current `pty.ts` public surface (input write, resize, output callback/iterator, exit promise) so `sandbox.ts` swaps transport without an API change.

- [ ] **Step 1: Write the failing test** with a fake ws server (node `ws`) speaking the enveloped protocol; assert stdout bytes, exit code, and that input is sent as an enveloped `ExecRequest{stdin}`. Run: `cd sdk/typescript && npm test -- pty_connect` and confirm it fails.

- [ ] **Step 2: Implement `connect_ws.ts`**: open a `WebSocket` to `${wsBase}/sandbox.v1.Sandbox/Exec` (subprotocol `connect.sandbox.v1`), send the enveloped `ExecRequest{open:{pty}}` first, pump input/resize, decode `ExecResponse` frames with the `connect.ts` `FrameReader`. Use binary frames. In the browser, the token rides the subprotocol or query as `pty.ts` does today (browsers cannot set ws headers); in node, set the `Authorization` header. Keep it dependency-free (global `WebSocket` in node 22+/browser; fall back to the `ws` package only in tests).

- [ ] **Step 3: Re-point `pty.ts`/`sandbox.ts`** at `PtyConnectSession`; delete the `/v1/pty` URL builder (`sandbox.ts:213`).

- [ ] **Step 4: Run the new test and the full suite.** Run: `cd sdk/typescript && npm test`. Expected: all pass (README claims 94; no regression).

- [ ] **Step 5: Commit.** `git add sdk/typescript/src/connect_ws.ts sdk/typescript/src/pty.ts sdk/typescript/src/sandbox.ts sdk/typescript/test/pty_connect.test.ts` then `git commit -s -m "feat(sdk/typescript): run PTY over the Connect WebSocket transport, retire /v1/pty (#358)"`.

---

## Task 4: Remove the dead /v1 runtime routes and the legacy PTY handler

**Files:**
- Modify: `internal/daemon/sandbox_api.go` (`Handler()`): delete the nine `deprecatedRuntimeNote(...)` runtime registrations (`/v1/exec`, `/v1/exec/stream`, `/v1/run_code/stream`, `/v1/files/{read,write,list,mkdir,remove}`, `/v1/vitals`) and the `/v1/pty` route. Keep the lifecycle routes (`/v1/set_timeout`, `/v1/pause`, `/v1/resume`) and `/v1/metering`, `/metrics`, `/healthz`, `/v1/vitals/node`.
- Delete: `internal/daemon/pty.go` and its now-dead handlers in `sandbox_api.go` (`handleExec`, `handleExecStream`, `handleRunCodeStream`, `handleReadFile`, `handleWriteFile`, `handleListDir`, `handleMkdir`, `handleRemove`, `handleVitals`) plus `deprecatedRuntimeNote` if unused; remove now-orphaned tests.
- Modify: `docs/threat-model.md` (drop the legacy `/v1` JSON-runtime row), `docs/api/v2-spec.md` and any `docs/api/*` that document `/v1/exec`, `/v1/files`, `/v1/pty`.

**Interfaces:**
- Consumes: the green CI of Tasks 2 and 3 (no SDK calls these routes anymore).
- Produces: a daemon whose only runtime surface is the Connect `sandbox.v1.Sandbox` service (HTTP for half-duplex, ws for bidi Exec); the `/v1` namespace holds only lifecycle/operational routes.

- [ ] **Step 1: Write/adjust the failing test**: assert `POST /v1/exec` and `GET /v1/pty` now return 404 from `Handler()`, while `/sandbox.v1.Sandbox/Exec` (ws) and `POST /v1/pause` still work. Run: `go test ./internal/daemon/ -run TestRuntimeRoutesRemoved -v` and confirm it fails.

- [ ] **Step 2: Delete the routes and handlers** listed above; run `go build ./...` and fix references (the conformance suite and any internal callers should already use Connect).

- [ ] **Step 3: Run the test + full suite + lint.** Run: `go test ./internal/daemon/ ./internal/sandboxrpc/`, `golangci-lint run --timeout=5m`, `GOOS=linux golangci-lint run --timeout=5m`.

- [ ] **Step 4: Update docs + threat model**, removing references to the deleted routes; confirm no doc still tells a user to call `/v1/exec` or `/v1/pty`.

- [ ] **Step 5: Commit.** `git add internal/daemon/ docs/threat-model.md docs/api/` then `git commit -s -m "refactor(daemon): remove the superseded /v1 runtime routes now that every SDK is on Connect (#358)"`. Request the named daemon reviewer.

---

## Task 5: Close #358

- [ ] **Step 1:** Confirm the per-SDK runtime surface is 100% Connect (re-run the grep from the gap analysis: no `/v1/(exec|files|run_code|pty|vitals)` literal remains in any SDK `src`).
- [ ] **Step 2:** Confirm CI is green on main after Task 4 merges (sdk-conformance, facade-conformance, the per-language SDK jobs, kind-e2e, firecracker-test).
- [ ] **Step 3:** Comment on #358 summarizing the migration (exec/files/run_code earlier; PTY now over the Connect ws transport; `/v1` runtime routes removed) and close it. Note any deliberately deferred proto RPCs (Stat/Watch/Archive/Processes/Signal/PortForward/Vitals are not exposed by any SDK and were never on `/v1`; if wanted, track them as a separate feature issue, not as a `/v1` retirement blocker).

---

## Self-Review

- **Spec coverage:** #358's close criterion is "every SDK execs over Connect, then the `/v1` runtime routes are removed" (from the deprecation commit `de0a161`). Tasks 1 to 3 put the last runtime call (PTY) on Connect; Task 4 removes the routes; Task 5 closes. The non-`/v1` proto RPCs (Stat/Watch/etc.) are explicitly scoped out as a separate feature, not a retirement blocker, matching the issue's "Out of scope" note.
- **Type consistency:** `ExecRequest`/`ExecResponse`/`ExecOpen`/`PtyOptions`/`WindowSize` are the existing `sandbox.v1` messages used by `pty.go` and `internal/sandboxrpc/service.go`. `PtyConnectSession` is the one new public type, defined identically (by surface) in Tasks 2 and 3. The envelope framing is the single existing format reused from `_connect.py`/`connect.ts`, not a new one.
- **Security:** Tasks 1 and 4 touch `internal/daemon`, carry a `docs/threat-model.md` delta, and are flagged for the named human reviewer. Auth, the concurrent-stream cap, and the "no client command" property are preserved from the existing `handlePty`.
