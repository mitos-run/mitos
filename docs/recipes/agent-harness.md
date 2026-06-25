# Recipe: host an HTTP daemon (coding-agent harness) in a sandbox

A Mitos sandbox can host a coding-agent harness (a
Rivet `sandbox-agent` style daemon, `opencode` web, or any in-box HTTP server)
and be driven from outside over HTTP. The integration surface such a harness
needs is exactly two things: shell access (Mitos has `exec` over vsock) and a
reachable network port. The port half ships today (`docs/ports.md`): a
TCP-over-vsock tunnel through the standalone
`sandbox-server`. This recipe shows the end-to-end flow using what ships today,
and is explicit about what is still a follow-up.

## What works today (standalone sandbox-server, real mode)

```
your client ──HTTP──▶ host listener (127.0.0.1:N) ──vsock tunnel──▶ guest agent ──▶ guest 127.0.0.1:<port>
```

1. Start the real-mode server on a KVM host:

   ```bash
   sandbox-server --kernel <vmlinux> --rootfs <rootfs> --agent-bin <agent> --data-dir <dir> --addr :8080
   ```

2. Create a template and fork a sandbox (any SDK or curl):

   ```bash
   curl -fsS -X POST localhost:8080/v1/templates -d '{"id":"harness"}'
   curl -fsS -X POST localhost:8080/v1/fork      -d '{"template":"harness","id":"sbx-1"}'
   ```

3. Start your HTTP daemon inside the guest with `exec` (it must listen on
   loopback, the tunnel dials `127.0.0.1:<port>`):

   ```bash
   curl -fsS -X POST localhost:8080/v1/exec \
     -d '{"sandbox":"sbx-1","command":"nohup my-agent-daemon --port 8000 >/tmp/d.log 2>&1 &"}'
   ```

4. Open a host forward to the guest port and dial it:

   ```bash
   # returns {"host":"127.0.0.1:NNNNN","guest_port":8000}
   curl -fsS -X POST localhost:8080/v1/sandboxes/sbx-1/forward -d '{"guest_port":8000}'
   curl http://127.0.0.1:NNNNN/        # reaches the daemon inside the guest
   ```

The forward is per sandbox, loopback-only on the host, bounded by a per-sandbox
concurrency cap, and torn down automatically when the sandbox terminates (see
`docs/ports.md`).

## The Mitos fork angle

The distinguishing property: you can warm one sandbox with the daemon installed
and the harness's dependencies in place, then `fork(n)` it into a swarm. Each
fork is an independent microVM restored from the snapshot, so each child has its
OWN copy of the running daemon and its own loopback listener. Open one forward
per child to reach each. In-flight HTTP sessions do not transfer across a fork:
the child resumes the snapshot's process state, not the parent's live TCP
connections, so treat a fork as a fresh, independently-reachable instance.

## Driving the in-guest daemon over HTTP (Mitos Expose)

The `/expose` path lets you send HTTP traffic, including streaming SSE sessions,
directly to the guest's in-box daemon without opening a separate host listener.
It is available on forkd (authenticated) and on the standalone sandbox-server
(tokenless, same trust model as the rest of the server).

5. Start the guest daemon as in step 3 above, then send HTTP requests directly
   through the expose route:

   ```bash
   # On forkd, supply the per-sandbox bearer token:
   curl -N -H "Authorization: Bearer <token>" \
     http://<forkd-node>:9091/v1/sandboxes/sbx-1/expose/8000/stream

   # On the standalone sandbox-server (tokenless loopback):
   curl -N http://localhost:8080/v1/sandboxes/sbx-1/expose/8000/stream
   ```

   The `-N` flag disables curl's output buffering so SSE frames print as they
   arrive. The route accepts any HTTP method: `GET` for SSE or `POST` for agent
   RPC calls. A trailing slash is also accepted
   (`/v1/sandboxes/sbx-1/expose/8000/`).

   The response is streamed immediately (`FlushInterval -1`, no buffering) so an
   in-guest daemon that writes `data: ...\n\n` SSE frames sees them forwarded to
   the caller as each frame is produced, not batched. Each request opens its own
   vsock tunnel and guest TCP connection and tears it down on close.

   The guest dial is forced to loopback by the guest agent, so the host never
   steers the tunnel to a non-loopback interface; port must be in 1-65535.

## Follow-ups (not yet shipped)

- Kubernetes Service / Ingress routing to the owning node's forwarded port (the
  forkd / cluster path); today the forward and expose paths are standalone-server
  and raw-forkd only.
- A first-class CRD field to declare exposed guest ports on a SandboxPool.spec.template /
  Sandbox.
- Internet-facing, subdomain-routed, TLS-terminated exposure (the
  `<label>.<expose-domain>` scheme, slice 2). For signed, expiring per-sandbox
  preview URLs see `docs/preview-urls.md`; that is a separate mechanism.
- A KVM end-to-end CI phase that starts a real in-guest SSE daemon and streams
  through the expose route on a KVM runner.
