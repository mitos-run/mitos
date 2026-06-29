# Per-sandbox egress proxy: unblock live-fork of networked sandboxes (#336)

Date: 2026-06-30
Status: design (brainstorm output). Implementation plan is a follow-up.
Scope: one repo, `mitos-run/mitos`. Security-sensitive path (`internal/fork`,
`internal/daemon`, `guest/agent-rs`); needs a named human reviewer per CLAUDE.md.
Issue: #336 (P1, reliability). Related: #494 (host-side domain egress
allowlist, the policy angle that plugs into this same proxy later), #211
(per-sandbox egress counter), #18 (husk pods / per-VM netns, now closed).

## Summary

`Engine.ForkRunning` (live-fork: checkpoint a running sandbox and spawn a child
from it) fails closed when networking is enabled (`internal/fork/engine.go`
around line 1495, with an error string that still cites the closed #18). This
blocks live-forking any real agent sandbox, because real agents hold outbound
connections (an LLM API keep-alive, tool HTTP pools). This design unblocks it
with a per-sandbox egress proxy and a fork-boundary upstream-reset handshake.

Two distinct failures sit behind the one gate:

1. **Host-side NIC identity collision.** The checkpoint embeds the source's
   baked NIC (tap, MAC, guest IP); a restored child collides with the still
   running source. The cold-fork path already mitigates this by rebinding the
   baked NIC to the fork's own tap via `network_overrides` (Firecracker v1.15,
   `engine.go` around line 1295) plus a fresh per-fork `/30` identity. The live
   path simply does not apply it yet.
2. **Guest-side connection-state divergence.** The snapshot captures the guest's
   in-flight sockets (keep-alive upstream connection, half-open TCP, TLS state).
   Parent and child both hold the same captured 4-tuples pointing at the same
   upstreams. A fresh host IP does not fix captured in-guest socket state.
   `network_overrides` cannot solve this half.

The fix for (1) is to apply the existing cold-fork network rebind on the live
path. The fix for (2) is the per-sandbox egress proxy: the host owns every
upstream socket, so guest memory never holds one and a child cannot inherit one;
the fork boundary then forces a deterministic re-dial.

Phase 1 (this design) scopes to HTTP and HTTPS egress, the dominant agent case,
via `HTTP_PROXY`/`HTTPS_PROXY` in the guest env. Phase 2 (transparent egress for
arbitrary sockets) is explicitly deferred.

## Guiding principles (from CLAUDE.md)

- **No unverified claims** (principle 1): the latency claims (networked
  live-fork vs cold-fork networked, N-way fan-out) are reproducible from `bench/`
  or they are not written.
- **Security findings block features** (principle 2): the proxy sits on
  secret-bearing traffic. `docs/threat-model.md` and `docs/fork-correctness.md`
  each gain a row in the same PR.
- **Boring failure behavior** (principle 4): when networking is enabled but the
  proxy is not active, the gate stays fail-closed with an actionable error that
  points at #336 (not #18).

## Network model (as built today)

Networking on the raw-forkd path is a shared subnet (e.g. `10.200.0.0/16`)
carved into `/30` point-to-point blocks, one per sandbox (`netconf.Allocator`).
Each sandbox gets a distinct tap, MAC, host-side IP (the gateway), and guest IP.
This is not per-VM netns; netns isolation lands with husk pods. The host owns
the tap side of each `/30`, runs a per-tap nftables egress chain
(`internal/netconf` renders it: shared `mitos_egress` table, per-tap dispatch,
metadata drops, allow set populated by the DNS proxy, masquerade SNAT), and a
DNS proxy that attributes queries by source guest IP. The guest's eth0 is
re-addressed per fork by the `NotifyForked` handshake
(`vsock.NotifyForkedNetwork`: guest IP, gateway, prefix, MAC, resolver IP),
which the Rust guest agent applies in `guest/agent-rs/src/fork/network.rs`.

Load-bearing consequence: re-addressing eth0 on fork already invalidates any
captured socket, because the socket is bound to the old guest IP, which no
longer exists on the interface after the flush-and-readdress. We build on that.

## Decisions (locked in brainstorming)

- **Proxy process model: one per-node proxy inside the forkd process**,
  attributing each inbound connection to a sandbox by source guest IP. This
  matches the existing DNS proxy and #211 egress-counter attribution exactly:
  one lifecycle, one blast-radius surface, reuses the per-tap nft attribution
  grain. Rejected: a separate proxy process per sandbox (N processes to
  spawn/reap, more lifecycle surface, a new pattern not used elsewhere).
- **HTTPS handling: CONNECT tunnel pass-through, no TLS interception.** The
  proxy sees only the `host:port` from the `CONNECT` line and then forwards
  opaque bytes. The threat model forbids the proxy from seeing headers, bodies,
  or auth values, so it must not MITM. SNI/domain policy enforcement is #494's
  job and plugs into this same proxy later.
- **e2e verification boundary:** the KVM acceptance test and `bench/` run are
  authored in this PR; mock-engine, unit, lint, and controller suites are
  verified locally; the real KVM acceptance runs in CI (`kvm-test.yaml`) on the
  self-hosted runners. The PR opens once the locally verifiable parts are green.

## Architecture

A new package `internal/egressproxy` holds the pure, platform-independent proxy
logic (request-line parsing, `CONNECT` handling, per-sandbox attribution by
source IP, the redaction boundary), unit-testable on any OS, mirroring the
`internal/netconf` vs `internal/network` split. The Linux exec wiring (listener
bind, conntrack flush) lives behind the existing Linux-tagged network seam.

Components:

1. **Egress proxy (forkd, per node).** Listens on a fixed host endpoint
   reachable from every guest `/30`. Serves HTTP forward-proxy requests and
   `CONNECT` tunnels. For each client connection it resolves the source guest IP
   to a sandbox, opens the upstream socket host-side, and pumps bytes. It holds
   the only reference to every upstream socket. It logs upstream `host:port` and
   byte counts only; never the request line target's query, headers, body, or
   any auth value. The proxy registers/deregisters a sandbox in
   `prepareForkNetwork`/`teardownForkNetwork`, keyed by guest IP, exactly where
   the DNS registry already is.

2. **Fork-stable sentinel endpoint.** The gateway (host side of each `/30`) is
   distinct per fork, so a baked `HTTP_PROXY=<gateway>` would point at the
   source's gateway. Instead a fixed sentinel address (default
   `169.254.169.2:<proxyport>`, a link-local outside the metadata range that is
   already drop-listed) is baked into the template env (`HTTP_PROXY`,
   `HTTPS_PROXY`, plus `NO_PROXY` covering loopback and the metadata address).
   The same value is correct in every fork. Per fork, the per-tap nftables chain
   DNATs `sentinel:proxyport` to this fork's `gateway:proxyport`, so the stable
   baked value routes to this fork's own proxy context. Rule rendering lives in
   `internal/netconf` (a new `RenderProxyDNAT(identity, sentinel, port)` helper)
   so it is unit-tested with the rest of the ruleset.

3. **Live-fork gate unblock (`engine.go`).** Replace the fail-closed block. When
   `networkEnabled()` and the egress proxy is active: run the live fork, calling
   `prepareForkNetwork` so the child acquires a fresh `/30` identity and the
   `network_overrides` NIC rebind (the same path cold fork uses). When the proxy
   is not active, keep the fail-closed error, rewritten to reference #336 and to
   carry actionable remediation text.

4. **`NotifyForked` extension (deterministic reset).** `vsock.NotifyForkedNetwork`
   (and the matching proto in `internal/guestgrpc`) gain two fields:
   `ProxyEndpoint string` (the sentinel `host:port`, config, safe to log) and
   `ResetUpstreams bool`. On the host side of the live fork, after the child is
   restored and before it is served, forkd flushes conntrack for the child's
   source guest IP so any in-flight proxied flow is RST'd authoritatively from
   the host. The guest agent, on `reset_upstreams`, drops stale neighbor/route
   state after re-addressing eth0 and writes `ProxyEndpoint` to a known env file
   for newly spawned processes (already-running processes re-dial via the
   dead-socket path). The existing `ReseededRNG` and clock-step handshake is
   unchanged and stays fail-closed (a guest that did not report `ReseededRNG` is
   left unserved).

5. **`notifyForkedRunning` (`server.go`).** Today it passes `nil` network for
   live forks. It must deliver the live fork's new per-fork identity (new guest
   IP, gateway, MAC) plus the proxy endpoint and reset signal, so the child
   re-addresses eth0 and resets upstreams.

## Data flow (live fork of a networked sandbox)

1. Source sandbox runs with networking: tap, `/30`, nft egress chain, registered
   with the proxy and DNS registry by source guest IP. Its app holds a keep-alive
   to an upstream, terminated at the host proxy; the upstream socket is owned by
   forkd, never in guest memory.
2. `ForkRunning(source, child)`: pause source (if requested), checkpoint, build
   the temporary live template, then `fork(...)` with networking. `fork` calls
   `prepareForkNetwork(child)`: acquire a fresh `/30`, set up the tap + nft chain
   (including the sentinel DNAT), register the child with the proxy/DNS by its new
   guest IP.
3. Load snapshot paused with `network_overrides` rebinding the baked NIC to the
   child's tap; rebind rootfs; resume.
4. `notifyForkedRunning(child)` sends `NotifyForked` with the child's new network
   identity, `ProxyEndpoint`, and `ResetUpstreams=true`; host flushes conntrack
   for the child's guest IP.
5. Guest agent re-addresses eth0 to the child's new guest IP and drops stale
   route/neighbor state. The captured app->proxy socket is bound to the old guest
   IP (now gone) and is dead; the HTTP client re-dials `sentinel:proxyport`,
   which DNATs to the child's gateway and reaches the child's proxy context. The
   proxy opens a fresh upstream. Parent and child now have independent egress;
   no tap/MAC/IP/4-tuple collision; the captured upstream socket is never reused.

## Error handling and failure behavior

- Proxy not active but networking enabled: `ForkRunning` fails closed with a
  #336-referencing, remediation-carrying error.
- Upstream dial failure in the proxy: returned to the guest as a normal proxy
  error (HTTP 502 for forward requests, connection close for `CONNECT`); logged
  as `host:port` + failure, never the payload.
- Conntrack flush best-effort: a flush error is logged (guest IP only) and the
  fork proceeds, because the dead-socket re-addressing path still forces a
  re-dial; the flush only makes it faster and deterministic.
- Proxy registration failure during `prepareForkNetwork`: release the identity
  and fail the fork (same fail-closed posture as the existing DNS registration
  and `netMgr.Setup` failures).
- Subnet exhaustion on the child identity: surfaces the existing
  `ErrSubnetExhausted` remediation.

## Security: threat-model and fork-correctness rows (same PR)

- `docs/threat-model.md`: new row for the egress proxy. It sits on
  secret-bearing traffic (API keys in `Authorization` headers). Mitigation:
  CONNECT pass-through (no TLS interception, so headers/bodies are never visible
  to the host), logs `host:port` and byte counts only, per-sandbox attribution
  by source guest IP, composes with (does not replace) the nft allowlist and the
  unconditional metadata drops. Residual: a plain-HTTP forward request's target
  path is visible to the proxy; mitigation is that the proxy logs only the host
  and port, never the path/query, and agents are steered to HTTPS.
- `docs/fork-correctness.md`: new row. Hazard: a fork inheriting a live upstream
  socket (shared 4-tuple/seq/TLS state across parent and child). Invariant: a
  fork must not inherit a live upstream socket; the child re-dials through the
  per-sandbox egress proxy. Mechanism: host-owned upstream sockets +
  per-fork eth0 re-address + fork-boundary conntrack flush + `ResetUpstreams`.
  Verification: the KVM acceptance test below.

## Testing

- **Unit (any OS):** proxy request-line and `CONNECT` parsing; source-IP to
  sandbox attribution; the redaction boundary (a test asserts no header, body,
  query, or auth value can reach a log sink); `RenderProxyDNAT` rule rendering in
  `netconf`; `NotifyForked` proto/vsock round-trip carrying `ProxyEndpoint` and
  `ResetUpstreams`. Rust unit test for the reset path in `fork/network.rs`.
- **Mock-engine:** `ForkRunning` with networking enabled now succeeds and
  delivers a fresh identity; the old fail-closed error is gone and no source
  string references #18.
- **Controller/envtest:** unchanged contract; a regression guard that a live
  fork of a networked sandbox is no longer rejected at the engine boundary.
- **KVM acceptance (`kvm-test.yaml`, self-hosted runners):** boot a networked
  sandbox holding an open keep-alive to a local upstream stub; live-fork it;
  assert (a) parent and child both have working independent egress, (b) no
  tap/MAC/IP or socket 4-tuple collision, (c) a fresh upstream connection is
  observed at the stub for the child (the captured upstream socket is not
  reused), (d) the fork-correctness handshake (`ReseededRNG`, clock step) still
  passes.
- **bench/:** networked live-fork latency vs the cold-fork networked path, and
  N-way fan-out, written so the numbers are reproducible (no-unverified-claims).

## Scope

In scope: HTTP and HTTPS egress, the host-side per-node proxy, the sentinel
endpoint + per-fork DNAT, the `ForkRunning` gate unblock, the `NotifyForked`
upstream-reset handshake, the threat-model and fork-correctness rows, the unit
and mock tests, the KVM acceptance test, and the bench run.

Out of scope (per the issue): UDP/QUIC upstreams, inbound connection
preservation, cross-node live-fork, transparent egress for arbitrary sockets
(Phase 2), and TLS interception / SNI domain allowlist (that is #494, which
plugs policy into this same proxy later).

## Open questions

- Sentinel address choice: `169.254.169.2` is proposed (link-local, distinct
  from the `169.254.169.254` metadata address that is unconditionally dropped).
  Confirm no existing rule or template assumes that address.
- Proxy port default and whether it is operator-configurable on the forkd flag
  surface alongside the existing `--dns-resolver` style flags.
