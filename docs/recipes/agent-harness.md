# Recipe: host an HTTP daemon (coding-agent harness) in a sandbox

Issue #230 asks whether a Mitos sandbox can host a coding-agent harness (a
Rivet `sandbox-agent` style daemon, `opencode` web, or any in-box HTTP server)
and be driven from outside over HTTP. The integration surface such a harness
needs is exactly two things: shell access (Mitos has `exec` over vsock) and a
reachable network port. The port half shipped as the foundation slice of issue
#228 (`docs/ports.md`): a TCP-over-vsock tunnel through the standalone
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

## Follow-ups (not yet shipped)

Tracked under issues #228 and #230:

- Auth on the forward path. The standalone forward inherits the standalone
  server's tokenless trust model; an authenticated forward for the hosted /
  multi-tenant path is a follow-up.
- Kubernetes Service / Ingress routing to the owning node's forwarded port (the
  forkd / cluster path); today the forward is standalone-server only.
- A first-class CRD field to declare exposed guest ports on a SandboxPool.spec.template /
  Sandbox.
- SSE / long-lived streaming specifics for the agent-session use case, and a
  worked Rivet `sandbox-agent` integration.
- For internet-facing, signed, expiring per-sandbox URLs (the E2B
  `get_host(port)` equivalent), see `docs/preview-urls.md`; that is a separate
  mechanism from this plain host-socket tunnel.
