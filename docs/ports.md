# Guest port forwarding (issue #228)

This document describes the FOUNDATION slice of issue #228: making a TCP port
inside a running sandbox reachable from the host, through the standalone
sandbox-server, by tunneling raw bytes over the existing vsock channel.

It is the standalone-only, first slice. The Kubernetes Service/Ingress routing
and the CRD template/claim port-declaration fields are EXPLICIT follow-ups of
issue #228 and are NOT built here (see "Follow-ups" below). For the
internet-facing, signed-URL, per-sandbox reverse-proxy exposure (the E2B
`get_host(port)` equivalent), see `docs/preview-urls.md`; that is a separate
mechanism. This document is about a plain host TCP socket bridged to a guest
port for local use.

## What ships in this slice

A TCP-over-vsock tunnel that makes a guest port reachable through the standalone
sandbox-server:

```
host TCP client ──▶ host listener (127.0.0.1:N) ──▶ vsock tunnel ──▶ guest agent ──▶ guest 127.0.0.1:<port>
```

- `vsock.TypeTunnel` (`internal/vsock`): a streaming frame that, like
  `exec_stream`, uses a DEDICATED vsock connection. The host sends one
  `TunnelRequest{port}`; the guest agent dials `127.0.0.1:<port>` inside the VM,
  replies with one `TunnelAck`, and on success the connection becomes a raw
  bidirectional byte pipe to the guest TCP socket until either side closes. One
  tunnel carries exactly one TCP connection (no multiplexing).
- Guest agent tunnel handler (`guest/agent/tunnel.go`): dials LOOPBACK only,
  refuses a non-loopback or out-of-range port and a port with no listener with a
  clean ack error (never a hang), and `io.Copy`s both directions with full
  teardown on either close.
- Host proxy (`internal/daemon` `SandboxAPI.ForwardPort` /
  `forward.go`): opens a host TCP listener on `127.0.0.1:0` and bridges every
  accepted connection over a fresh vsock tunnel to the guest port. Listeners and
  their in-flight tunnels are tracked per sandbox and closed on
  `UnregisterSandbox` (terminate), so nothing outlives the sandbox.
- sandbox-server `POST /v1/sandboxes/{id}/forward` (`cmd/sandbox-server`):
  opens a forward and returns the host address to dial.

## The endpoint

`POST /v1/sandboxes/{id}/forward`

Request body:

```json
{ "guest_port": 8000 }
```

Response (200):

```json
{ "host": "127.0.0.1:53652", "guest_port": 8000 }
```

The caller dials the returned `host` address with a plain TCP client; bytes are
piped to and from the guest's `127.0.0.1:8000`.

Error responses use the standard LLM-legible envelope
(`docs/api/errors.md`):

- `501` in mock mode: forwarding bridges a real guest TCP socket, which mock
  mode does not have. Run sandbox-server in real mode (a KVM-backed engine).
- `404` for an unknown sandbox, or a sandbox whose guest agent is not connected.
- `400` for a `guest_port` outside 1-65535.

A guest port with no listener does not fail the endpoint (the listener is
already open by then); instead each connection to the host address is closed
promptly when the guest refuses the tunnel, so a client sees a closed connection
rather than a hang.

## Security properties

These are recorded as a row in `docs/threat-model.md` (section 3); the summary:

- **Loopback only, both ends.** The host listener binds `127.0.0.1` (reachable
  only from the host running the server), and the guest agent forces the dial to
  `127.0.0.1` (the host carries only a bare port; the tunnel cannot be steered to
  another guest interface or back out to the host network).
- **Per-sandbox concurrency cap.** The number of concurrent forwards per sandbox
  is bounded (default 16, `SetMaxForwardsPerSandbox`, mirroring the
  streaming-exec ceiling) so one sandbox cannot exhaust host sockets. Each
  forward and all its tunnels are closed on terminate.
- **No auth on the host listener.** This is the SAME tokenless trust model as the
  rest of the standalone sandbox-server (a single-tenant local server that runs
  with `AllowTokenless`), not a new weakening. Auth on the forward path is a
  follow-up; do not expose the standalone server's forward listeners to an
  untrusted network.
- **No secret logging.** The tunnel bytes are application traffic and are never
  logged; only the sandbox id, host address, and guest port are logged.

## Follow-ups (NOT in this slice)

These are tracked under issue #228 and are explicitly out of scope here:

- Kubernetes Service/Ingress routing for the forkd path (this slice is the
  standalone sandbox-server only).
- CRD template/sandbox port-declaration fields (declaring exposed ports on
  `SandboxPool.spec.template` / `Sandbox`).
- Auth on the forward path (a token gate on the host listener).
- UDP forwarding (this slice is TCP only).
