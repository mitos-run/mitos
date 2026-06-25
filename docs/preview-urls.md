# Expose URLs: per-sandbox port exposure via the edge proxy

An expose URL makes a port inside a running sandbox reachable through the Mitos
expose edge proxy. The URL uses a single-label hostname, `<label>.<expose-domain>`,
where `<expose-domain>` is the operator-configured domain and `<label>` is an opaque
routing key assigned when the route is registered. It is the Mitos equivalent of
E2B `sandbox.getHost(port)` and Daytona signed, expiring preview URLs: one
internet-facing entrypoint fronting many ephemeral per-sandbox backends, with the
same per-sandbox token gate as the sandbox API.

This document describes the verifiable core that ships today (the routing, the
signed/expiring URL scheme, the route table and its GC, the reserved-name
blocklist, the admin route-sync endpoint, the controller route-sync loop that
pushes routes from Ready Sandboxes, and the SDK `get_host` call) and the parts
that are deferred to later slices (wildcard and post-quantum TLS are slice 3;
the full sharing ladder including audience selectors is slice 4).

## What ships in slices 2a and 2b

Slice 2a:

- `internal/preview`: the signer (mint + verify signed expiring URLs), the host
  parser, the reserved-name blocklist, the route table with GC, the reverse proxy,
  and the admin route-sync endpoint. The Go package keeps the name `internal/preview`
  (and the binary `cmd/preview-proxy`) for now while the product subsystem is named
  Mitos Expose; the package rename is a deferred cleanup.
- `cmd/preview-proxy`: the standalone proxy binary. One entrypoint that resolves the
  single-label hostname, verifies the signed token, looks up the route, and proxies
  to the owning forkd node.
- `sandbox-server` `POST /v1/preview`: mints a signed expose URL for a sandbox port
  (the signing secret lives on the server, never in the SDK).
- Python SDK `DirectSandbox.get_host(port)` and the E2B shim
  `Sandbox.get_host(port)`: return the signed URL.

Slice 2b (the controller route-sync loop, completing the end-to-end path):

- `Sandbox.spec.expose`: a new CRD field `{port (1-65535), label (single DNS
  label), sharing (private|link|org|authenticated|public, default private)}`.
  Declares that a guest port is reachable at the per-sandbox subdomain
  `<label>.<expose-domain>`.
- `ExposeRouteReconciler` in the controller: watches Sandboxes; on any change it
  lists all sandboxes, selects those that are Ready (`Status.Phase==Ready`) with
  `spec.expose` set and a non-empty `Status.Endpoint`, reads each one's per-sandbox
  bearer from its `<name>-sandbox-token` Secret (key `token`), builds the full route
  set, and POSTs it to `POST /internal/routes` on the proxy admin endpoint.
- Full-set replace semantics: the proxy reaps any sandbox that leaves the
  Ready-and-exposed set. The label for a sandbox that is no longer Ready or no
  longer has `spec.expose` set drops from the next posted set, making it
  unroutable.
- Fail-safe: a missing `<name>-sandbox-token` Secret skips that sandbox and
  requeues after 1 second. A poster error requeues with backoff. The controller
  never crashes on a transient proxy failure.
- Disabled by default: the reconciler is enabled only when `--expose-proxy-admin-url`
  is set on the controller. The admin token is read from `EXPOSE_PROXY_ADMIN_TOKEN`
  (environment variable, never argv, never logged).

Together slices 2a and 2b complete the end-to-end path: a `Sandbox` with
`spec.expose` set and `Status.Phase==Ready` is reachable at
`<label>.<expose-domain>` through the proxy, with routes kept current by the
controller reconciler.

## Request flow

```
caller ──HTTPS──> expose-proxy (one entrypoint)
                    1. parse  <label>.<expose-domain>  -> label
                    2. reject reserved labels (www, app, api, console, admin,
                       auth, login, ...) with 404
                    3. resolve label to a route (NodeEndpoint, SandboxID, Port,
                       Token, Sharing) via the route table; unknown label is 404
                    4. verify the signed expiring token (HMAC, not expired)
                    5. bind token to the sandbox and port in the route
                       (token for a different sandbox or port is 403)
                    6. attach the per-sandbox bearer, strip the expose token from
                       the forwarded query, unconditionally delete any inbound
                       Authorization header, clean dot-segments from the sub-path
                    7. reverse-proxy to
                       http://<NodeEndpoint>/v1/sandboxes/<id>/expose/<port>/<sub-path>
                       (the forkd expose handler, slice 1)
```

The proxy reaches forkd `:9091` over plain HTTP. forkd already serves the sandbox
API in cleartext with per-sandbox bearer auth; this matches the existing SDK path
and is not a new weakening. The cluster network is the trust boundary between the
edge proxy and forkd. The per-sandbox bearer is the guard on forkd; the signed
expose token is the guard at the public edge.

A failure at any step is terse and never echoes the token: a reserved or unknown
label is `404`, a missing or invalid token is `401`, a token for a different
sandbox or port is `403`, an unreachable backend is `502`.

Streaming responses (SSE, chunked bodies) are forwarded with `FlushInterval -1`
so the proxy never buffers output.

## The signed, expiring URL scheme

An expose URL is `https://<label>.<expose-domain>/?token=<token>`. The label is an
opaque routing key (NOT the sandbox id). The token is a detached HMAC over a
compact JSON payload:

```
payload = base64url(json{ "s": sandboxID, "p": port, "e": expiryUnix })
tag     = base64url( HMAC-SHA256( serverSecret, "mitos-preview-v1\0" || payload ) )
token   = payload + "." + tag
```

`Verify` recomputes the tag, compares it in CONSTANT TIME (`crypto/subtle`),
then rejects the token if `now` is after the embedded expiry. The properties
that matter, each unit-tested in `internal/preview/sign_test.go`:

- **Never accept after expiry.** A token one second past its expiry, and a token
  checked far in the future, both fail. The expiry boundary is inclusive.
- **Never accept tampered.** Flipping any byte of the payload or the tag fails
  the constant-time compare; garbage fails to parse.
- **Never accept wrong key.** A token minted under a different server secret
  fails to verify.
- **Bound to one sandbox and one port.** The proxy additionally requires the
  verified token to name the sandbox and port in the route, so a leaked URL
  cannot be replayed against another sandbox or a different port.

### Why not a captoken

Mitos already has macaroon-style attenuated capability tokens
(`internal/captoken`). An expose token needs no attenuation chain,
only a single expiring binding of `(sandbox, port)`, so a focused signer keeps
the scheme small and auditable. It reuses the SAME standard-library HMAC-SHA256
and constant-time-compare core as captoken and the W4 S3 SigV4 signer
(`internal/workspace/s3client.go`); no new crypto is invented.

### Secret handling

The server secret (`MITOS_PREVIEW_SECRET`, at least 16 bytes) and every minted
token are BEARER CREDENTIALS. They are never logged, never put in an error,
condition, or event, and never written to a host path. The proxy logs the
label, sandbox id, and the HTTP status only; the signing path logs nothing. The
proxy strips the `token` query parameter before forwarding, so the forkd expose
handler never sees the expose bearer. Any inbound `Authorization` header is
unconditionally deleted before the upstream request is sent, so the edge proxy
cannot be used to relay an unrelated bearer to forkd.

## Route table and GC

The route table maps a `<label>` (opaque routing key) to a `Route` carrying:
the owning forkd node endpoint (`NodeEndpoint`, the `Sandbox.Status.Endpoint`
`host:port` of forkd `:9091`), the sandbox id, the guest port, the per-sandbox
bearer, and the access tier.

The table is populated exclusively via the admin route-sync endpoint (see below).
`RouteTable.Sync(routes)` reconciles the table to exactly the provided set:
routes not in the new set are REAPED immediately, so a sandbox that leaves the
Ready set has its route removed on the next sync and is unroutable (`404`). No
route means 404, so an expose URL for a terminated sandbox cannot proxy anywhere.

### Reserved-name blocklist

A fixed set of labels is permanently blocked and never routable regardless of what
the route table contains: `www`, `app`, `api`, `console`, `admin`, `auth`,
`login`, and others. A request whose `<label>` matches any reserved name receives
`404`. This is enforced by `IsReservedLabel` in the host parser before route
table lookup, so the blocklist cannot be bypassed by registering a route under a
reserved name.

### Dot-segment cleaning

The URL sub-path after `/` is cleaned with `path.Clean` before it is appended to
the upstream path `http://<NodeEndpoint>/v1/sandboxes/<id>/expose/<port>/`. This
removes `.` and `..` segments so a traversal cannot escape the expose path prefix
and reach unintended forkd routes.

## Admin route-sync endpoint

The proxy exposes a single authenticated admin endpoint for the controller
reconciler (slice 2b) to push the current route set:

```
POST /internal/routes
Authorization: Bearer <MITOS_EXPOSE_ADMIN_TOKEN>
Content-Type: application/json

[ { "label": "...", "nodeEndpoint": "...", "sandboxID": "...",
    "port": 8080, "token": "...", "sharing": "private" }, ... ]
```

The admin bearer is read from `MITOS_EXPOSE_ADMIN_TOKEN` at startup and compared
with CONSTANT TIME on every request. The token VALUE is never logged and never
appears in an error body. An empty `MITOS_EXPOSE_ADMIN_TOKEN` disables the endpoint
entirely (returns `404` for all `POST /internal/routes` requests); it does NOT
default to open. The controller reconciler that reads Ready Sandboxes and their
`<name>-sandbox-token` Secrets and POSTs the route set is the slice-2b
`ExposeRouteReconciler` described in the section above.

## Sub-path and dot-segment behavior

The proxy appends the request path after the label hostname to the upstream expose
path. Before appending, it calls `path.Clean` on the sub-path so traversal
sequences (`../`, `./`) are resolved to their canonical form and cannot escape the
`/v1/sandboxes/<id>/expose/<port>/` prefix. An empty sub-path becomes `/`.

## TLS

This slice does not enable TLS. The proxy can be run behind a separate TLS
terminator. Wildcard certificate provisioning and post-quantum TLS are slice 3.

### Bare metal

Bare metal is a first-class target. A maintainer can terminate TLS in front of the
proxy (Caddy, nginx, or any ACME-capable reverse proxy with a wildcard cert for
`*.<expose-domain>`) without any change to the proxy binary.

## SDK usage

```python
import mitos

sb = mitos.create("python")
url = sb.get_host(8080)          # signed, expiring expose URL for port 8080
# url == "https://<label>.<expose-domain>/?token=..."
```

`get_host(port)` asks the server to mint the URL (`POST /v1/preview`) and
returns it; the signing secret never leaves the server. A server that does not
expose the proxy returns a typed `501`. The E2B compatibility shim
(`mitos.e2b.Sandbox.get_host`) delegates to this same method, so an
E2B script's `sandbox.get_host(port)` works unchanged.

## Operating the proxy

```bash
export MITOS_PREVIEW_SECRET=<at least 16 random bytes, kept secret>
export MITOS_EXPOSE_ADMIN_TOKEN=<at least 16 random bytes, kept secret>
expose-proxy --domain example.com --addr :8080
```

The proxy listens on `--addr` (HTTP). Run it behind a TLS terminator for
production. The route table starts empty and is populated by `POST /internal/routes`
from the controller `ExposeRouteReconciler`.

## Sharing ladder

The `Sharing` field on a route carries the access tier (`private`, `org`,
`audience`). The full sharing ladder with audience selectors and org-scoped access
is slice 4 and NOT implemented in this slice. All routes in this slice are treated
as private (the signed token is the sole gate).

## Production gate

This ingress adds a public attack surface. It is NOT cleared for production
tenants until the external security review covers it. Edge rate limiting and an
SNI/connection cap are documented follow-ups. See docs/threat-model.md section 7c.
