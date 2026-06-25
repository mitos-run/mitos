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
pushes routes from Ready Sandboxes, the SDK `get_host` call, and the wildcard
plus post-quantum TLS that ships in slice 3) and the parts that are deferred to
later slices (the full sharing ladder including audience selectors is slice 4).

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

Known limitation, label uniqueness is not yet enforced. The route table keys by
`label`, and `label` is the public subdomain, so it must be globally unique by
construction. If two sandboxes declare the same `spec.expose.label` (even in
different org namespaces) they collide in the posted set and the last one written
wins non-deterministically. This is an availability and squatting concern, not a
credential leak: each route still carries only its own sandbox's bearer (the token
map is namespace-scoped, regression-tested). A global label-allocation registry
with reserved-name enforcement across tenants is a later slice; until then label
uniqueness is operator-owned.

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

TLS terminates at the Go proxy (`cmd/preview-proxy`). The proxy selects a
certificate provider at startup based on its flags.

### Wildcard certificate (operator-provided or cert-manager ACME DNS-01)

Pass `--tls-cert` and `--tls-key` (both required together) to load an
operator-provided wildcard `*.<expose-domain>` certificate and key from disk via
`tls.LoadX509KeyPair` (`WildcardProvider` in `internal/preview/cert.go`). The
proxy serves the same certificate for every SNI host; the TLS client verifies that
the wildcard covers the requested hostname. A missing or unparseable file is a
startup-time fatal error (fails closed, never serves a broken handshake).

In the Helm chart (`deploy/charts/mitos/templates/expose-proxy.yaml`), when
`expose.enabled: true` the `--tls-cert` and `--tls-key` flags are wired to
`/tls/tls.crt` and `/tls/tls.key` from the `expose.tls.secretName` Secret. When
`expose.tls.certManager.enabled: true` the chart also renders a cert-manager
`Certificate` resource for `*.<expose-domain>` against the named `Issuer` or
`ClusterIssuer`, so cert-manager can handle ACME DNS-01 issuance and automatic
rotation. When cert-manager is not in use, the operator mounts the wildcard cert
Secret directly.

### Self-signed fallback

When `--tls-cert` is not set, the proxy generates a per-SNI self-signed
certificate at runtime (`SelfSignedProvider`). A self-signed certificate is NOT
trusted by browsers and is NOT a production substitute. The default Helm chart
deployment always passes `--tls-cert` and `--tls-key`, so self-signed is the
local-dev and bare-metal-without-cert fallback only.

### Post-quantum key exchange (hybrid X25519MLKEM768)

`preview.ServerTLSConfig` (the proxy's server TLS config) deliberately leaves
`CurvePreferences` nil. On Go 1.24 and newer, Go's default key-exchange
preference list leads with the hybrid post-quantum group X25519MLKEM768 (FIPS 203
ML-KEM-768 combined with X25519). The negotiated group is post-quantum when the
client supports it; the server never forces a downgrade.

This protects the confidentiality of sandbox traffic against harvest-now-decrypt-later
attacks: an attacker who records ciphertext today cannot decrypt it later even with
a future large-scale quantum computer.

Honest scope: this is post-quantum KEY EXCHANGE for CONFIDENTIALITY only. The
certificate signature remains classical (ECDSA or RSA): no post-quantum certificate
authority exists in the public PKI today, so there is no post-quantum authentication
claim and none is made here. The PQ protection is for the session key, not for
identity.

A guardrail test (`internal/preview/tls_pq_test.go`,
`TestServerTLSConfigNegotiatesPostQuantum`) asserts that a PQ-only TLS 1.3 client
(offering only `X25519MLKEM768`) completes the handshake and that the negotiated
`CurveID` is `X25519MLKEM768`. It also asserts `cfg.CurvePreferences == nil`,
because pinning any curve list silently removes X25519MLKEM768 from Go's defaults.
A future PR that inadvertently sets `CurvePreferences` on the server config will
break this test, preventing a silent regression.

### On-demand CertMagic ACME (documented seam, not compiled)

`internal/preview/cert.go` documents the adapter shape for CertMagic on-demand
TLS (`CertMagicProvider`). This code is NOT compiled with a real certmagic
dependency in this slice: real ACME issuance needs a public domain, a DNS record
for `*.<expose-domain>`, and a publicly reachable endpoint, none of which exist in
CI. The adapter is a thin follow-up; the routing and signing core are independent of
it. The wildcard cert path above is the production and bare-metal path.

### Bare metal

Bare metal is a first-class target. Pass `--tls-cert` and `--tls-key` pointing at
an operator-provided wildcard cert (from a self-hosted ACME such as step-ca, or
from any CA that issues wildcard certificates). The proxy binary has no external
dependency for TLS; it uses the standard-library `crypto/tls` stack.

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

# With an operator-provided wildcard cert (production and bare metal):
expose-proxy --domain example.com --addr :8443 \
  --tls-cert /path/to/wildcard.crt --tls-key /path/to/wildcard.key

# Without a cert (self-signed, local dev only, not browser-trusted):
expose-proxy --domain example.com --addr :8443
```

The proxy listens on `--addr` (HTTPS). The route table starts empty and is
populated by `POST /internal/routes` from the controller `ExposeRouteReconciler`.
Pass `--http-addr` to also listen on a plaintext port (for health checks behind an
L4 load balancer that does its own TLS; this is NOT the expose traffic path).

## Auth ladder (slice 4)

The auth ladder ships in slice 4. Every expose route carries a `Sharing` tier
plus optional composable layers. Enforcement runs as a strict ordered pipeline
on every request.

### Access tiers

| Tier | Who can reach it |
|---|---|
| `public` | Anyone with the URL. No identity required. |
| `link` | Anyone with a valid signed expose URL. A first-hit cookie exchange replaces the token in the address bar with a `__Host-` session cookie. |
| `authenticated` | Any identity that successfully completes the OIDC flow at `auth.<expose-domain>`. |
| `org` | An authenticated caller whose org IDs (resolved from their verified email) include the sandbox's org. |
| `private` | Same as `org`. Both `private` and `org` check the sandbox OrgID; `private` signals owner-org intent. |

### Composable layers

Applied after the tier check, in order:

1. **Network**: an optional CIDR allowlist on the route. Evaluated against the
   caller IP on every request, before identity. A caller outside the list is
   rejected 403 regardless of their session.
2. **Audience**: `allowedPrincipals` (exact email match) and
   `allowedEmailDomains` (registrable domain, case-insensitive, verified email
   only). Both lists may coexist; a caller must satisfy at least one entry in
   whichever lists are non-empty. Unverified email is always rejected here.
3. **forwardAuth**: an optional BYO-IdP subrequest. The proxy calls the
   operator-configured URL with `X-Forwarded-Method`, `X-Forwarded-Uri`,
   `X-Forwarded-Host`, `X-Forwarded-For`, and `Cookie`. A non-2xx response is
   returned to the caller. `X-Auth-Request-*` headers sent by the CLIENT are
   stripped BEFORE the subrequest and before upstream proxying, so a client
   cannot inject a forged identity. The network layer is evaluated before the
   forwardAuth subrequest.

### Central auth origin: `auth.<expose-domain>`

Each `<label>.<expose-domain>` is its own browser origin. To avoid scattering
the OIDC client secret and multiplying OIDC code flows, a single central origin
handles authentication.

`auth.<expose-domain>` is a permanently reserved label (in the reserved-name
blocklist) and is never routed to a tenant sandbox. The proxy handles it as the
OIDC relying party.

**Flow for a request to a tier that requires identity:**

1. The app subdomain checks for a valid per-app `__Host-` session cookie. If
   present and unexpired, identity is taken from it.
2. No valid cookie: 302 to `https://auth.<expose-domain>/start?rd=<label>&path=<path>`.
3. `auth.<expose-domain>/start` checks its own `__Host-` SSO cookie. If absent,
   it starts the OIDC Authorization Code flow against the configured issuer,
   verifies the ID token, extracts `sub`, `email`, `email_verified`, and sets
   the SSO cookie (scoped to `auth.<expose-domain>` only via `__Host-`).
4. The proxy resolves the verified email to org IDs (see "Identity resolution"
   below) and records them in the SSO session.
5. `auth.<expose-domain>` validates the `rd` redirect target against the live
   route table (the label must name a real route; prevents open redirect).
   It issues a short-lived single-use HMAC grant bound to
   (label, sub, email, email_verified, orgIDs, expiry) and 302s to
   `https://<label>.<expose-domain>/__mitos_auth/cb?grant=<grant>&path=<path>`.
6. The app subdomain `/__mitos_auth/cb` handler verifies the grant (HMAC,
   unexpired, single-use nonce, label matches), enforces the tier and audience
   layers, sets a per-app `__Host-` session cookie (HMAC over the identity,
   ~1h TTL, `Secure`, `HttpOnly`, `SameSite=Lax`, `Path=/`, no `Domain`), and
   302s to the original path.
7. Subsequent requests validate the local cookie. No round trip to
   `auth.<expose-domain>` until the cookie expires.

**Cookie isolation**: the `auth.<expose-domain>` SSO cookie is `__Host-` and
therefore host-only; a tenant app running at `<label>.<expose-domain>` cannot
read it. Per-app session cookies are also `__Host-` and host-only, isolated to
their subdomain origin. Cross-subdomain session fixation is not possible.

### Identity resolution

Org membership lives in the Mitos account database, not in the OIDC ID token,
so the proxy resolves identity via the console service:

```
POST /internal/identity/resolve
Authorization: Bearer <shared resolve token>
Content-Type: application/json

{"email": "<verified email>"}
```

returns `{"accountId": "...", "orgIds": [...]}`. The resolve token is a shared
secret between the proxy (`MITOS_EXPOSE_RESOLVE_TOKEN`) and the console
(`MITOS_IDENTITY_RESOLVE_TOKEN`); both read it from the same Secret key. The
endpoint is ClusterIP-only and never logged beyond org count. When
`expose.identityResolve.resolveURL` is empty the resolve hop is disabled and
the proxy falls back to OIDC claims or forwardAuth-supplied identity.

### Self-host without the SaaS account service

For a self-hosted deployment without the full account service:

- Omit `expose.identityResolve.resolveURL` to disable the resolve hop.
- Use the `forwardAuth` layer: configure an external IdP (oauth2-proxy,
  Authelia, Keycloak) to serve an auth subrequest. The proxy treats the
  `X-Auth-Request-Email`, `X-Auth-Request-User`, and `X-Auth-Request-Groups`
  headers from the subrequest response as the caller identity (client-supplied
  values with those names are stripped before the subrequest).
- The `org` and `private` tiers fall back to the OIDC `groups` claim when no
  resolve endpoint is configured.

### OIDC honest scope

The OIDC code flow (discovery, redirect, callback, ID token verify) is tested
with a fake token verifier in unit CI. The live-IdP path is
maintainer-verified. PKCE is a documented follow-up. SAML and SCIM are
deferred. This surface stays gated behind the external security review (issue
#194) and the abuse-control envelope (issue #213) before serving untrusted
tenants.

## Deploying with the Helm chart

The Helm chart (`deploy/charts/mitos`) deploys the proxy when `expose.enabled:
true` (default false). When enabled, the chart renders the proxy Deployment and
Service, mounts the signing secret and admin token from `expose.secretName`, and
mounts the wildcard cert from `expose.tls.secretName`. The controller
`--expose-proxy-admin-url` and `EXPOSE_PROXY_ADMIN_TOKEN` are wired to the proxy
Service so the `ExposeRouteReconciler` can push routes. An optional Ingress is
rendered when `expose.ingress.enabled: true`. An optional cert-manager Certificate
for `*.<expose-domain>` is rendered when `expose.tls.certManager.enabled: true`.

### OIDC and identity-resolve Helm wiring (slice 4)

To enable the auth ladder set `expose.oidc.issuer` and `expose.oidc.clientID`.
The chart then passes `--oidc-issuer`, `--oidc-client-id`, and `--resolve-url`
to the proxy and mounts the HMAC secrets (`grant-secret`, `session-secret`,
`sso-secret`, `state-secret`), the OIDC client secret (`oidc-client-secret`),
and the shared resolve token (`resolve-token`) from `expose.secretName`.

When `expose.enabled: true` the console Deployment receives
`MITOS_IDENTITY_RESOLVE_TOKEN` from the same `expose.secretName` Secret key
(`resolve-token`), activating the `POST /internal/identity/resolve` endpoint.
The OIDC secret values are NEVER inlined in the chart; all are mounted via
`secretKeyRef`. Populate the Secret out of band before enabling.

```yaml
expose:
  enabled: true
  domain: mitos.app
  oidc:
    issuer: https://dex.example.com
    clientID: mitos-expose
  # identityResolve.resolveURL defaults to the in-cluster console Service.
```

## Production gate

This ingress adds a public attack surface. It is NOT cleared for production
tenants until the external security review (issue #194) covers it. Edge rate
limiting and an SNI/connection cap are documented follow-ups, sequenced with the
#213 abuse-control envelope. See docs/threat-model.md section 7c.
