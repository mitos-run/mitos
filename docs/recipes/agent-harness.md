# Recipe: host an HTTP daemon (coding-agent harness) in a sandbox

A Mitos sandbox can host a coding-agent harness (a
Rivet `sandbox-agent` style daemon, `opencode` web, or any in-box HTTP server)
and be driven from outside over HTTP. The integration surface such a harness
needs is exactly two things: shell access (Mitos has `exec` over vsock) and a
reachable network port. Both halves ship today, at three levels: a
TCP-over-vsock tunnel through the standalone `sandbox-server` (`docs/ports.md`),
a direct authenticated `/expose` HTTP path on a single node, and an
internet-facing authenticated `https://<label>.<expose-domain>/` URL through the
Mitos Expose edge proxy (`docs/preview-urls.md`). This recipe walks all three,
ending with a fork fan-out swarm, and is explicit about what is still a
follow-up.

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

## End to end: a Rivet harness on an authenticated URL

The raw `/expose` path above reaches a single node directly. For an
internet-facing, TLS-terminated, authenticated URL, run the harness in a
workspace-bound sandbox and let the Mitos Expose edge proxy route to it. This is
the path `mitos workspace serve` sets up.

Prerequisites: the expose proxy is deployed (the `expose.enabled` Helm values), a
wildcard `*.<expose-domain>` certificate is issued, `*.<expose-domain>` DNS
resolves to the proxy, and for the private default tier the proxy's OIDC relying
party is configured. With those in place:

1. Create a durable workspace, backed by a warm pool whose template has the Rivet
   `sandbox-agent` daemon and its dependencies installed:

   ```bash
   mitos workspace create harness
   ```

2. Serve the workspace. This claims a forked sandbox from the pool, binds it to
   the workspace, sets `spec.expose`, waits until it is Ready, and prints the
   URL:

   ```bash
   mitos workspace serve harness --pool rivet --port 8000 --expose-domain mitos.app
   # https://harness-7f3a.mitos.app/
   ```

   By default the URL is private: a caller authenticates through the proxy's OIDC
   flow against the owner's org. Pass `--sharing link` for a signed shareable
   link, `--sharing authenticated` for any logged-in user, or `--sharing public`
   for an open URL. The composable layers (a network IP allowlist, an audience of
   allowed principals or verified email domains, and a forwardAuth bring-your-own
   identity provider) are documented in `docs/preview-urls.md`.

3. Start the daemon inside the guest with `exec` (as in step 3 of the first
   section), then stream its SSE session from the authenticated URL:

   ```bash
   curl -N https://harness-7f3a.mitos.app/stream
   ```

   The proxy enforces the access tier, terminates TLS (post-quantum
   X25519MLKEM768 key exchange when the client supports it; this protects the
   transport, not the certificate), and forwards the request to the owning node's
   expose handler, which streams each `data: ...` frame back without buffering
   (`FlushInterval -1`). A `POST` to the same URL drives the harness's agent RPC.

The SDKs return the same URL as a handle. With the Go SDK:

   ```go
   served, err := ws.Serve(ctx, mitos.WithServePool("rivet"),
       mitos.WithServeExposeDomain("mitos.app"))
   // served.URL == "https://harness-7f3a.mitos.app/"
   ```

   The Python, TypeScript, Ruby, Rust, and Java SDKs expose the same `serve` verb
   returning a handle with a `url`.

## Fork fan-out: a swarm of harnesses, each its own URL

The fork angle and the authenticated URL compose. Warm one sandbox with the
harness installed, fork it into a swarm, and give each child its own label so
each is reachable at its own authenticated URL:

1. Serve N children, each with a distinct label, from the same warm pool:

   ```bash
   for i in $(seq 1 8); do
     mitos workspace serve harness --pool rivet --as agent-$i --port 8000 \
       --expose-domain mitos.app
   done
   # https://agent-1.mitos.app/ ... https://agent-8.mitos.app/
   ```

   Each child is an independent microVM restored from the snapshot, with its own
   copy of the running daemon, its own `spec.expose` label, and its own access
   tier. A fan-out orchestrator can hand each agent session its own URL and stream
   all of them concurrently.

2. The labels share one wildcard certificate and one proxy, so adding a child
   needs no new certificate issuance or DNS record. A label must be a single DNS
   label; a small set of names (for example `auth`, `api`, `console`) is
   reserved, and the owner is responsible for label uniqueness within a domain.

In-flight HTTP sessions do not transfer across a fork (a child resumes the
snapshot's process state, not the parent's live TCP connections), so treat each
forked URL as a fresh, independently-reachable harness.

## Follow-ups (not yet shipped)

- A KVM end-to-end CI phase that starts a real in-guest SSE daemon and streams
  through the authenticated expose URL on a KVM runner. The expose path is
  covered by unit and mock tests today; the live in-guest SSE stream is
  maintainer-verified.
- Minting a signed `--sharing link` token from inside the cluster. The CLI and
  SDKs construct the private and identity-tier URLs directly; link-token minting
  in the cluster path is still being wired.
- Deriving org membership from an OIDC groups claim for self-hosted identity
  providers. The forwardAuth path and the hosted identity resolver are the
  supported org sources today.
