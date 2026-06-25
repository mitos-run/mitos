# Mitos Expose: authenticated, named URLs to a sandbox or workspace

Status: design, approved for spec review.
Issues: #230 (host a coding-agent harness in a sandbox and reach it over HTTP),
#312 (workspace claim to a ready environment URL). Depends on the shipped
preview core (#126) and guest port forwarding (#228 / #271). Sequenced behind
the #194 external security review and the #213 abuse-control envelope before any
untrusted public exposure opens.

## 1. Summary

Mitos Expose is one subsystem that turns a port inside a running sandbox or a
durable workspace into an authenticated, TLS-terminated, named URL. It unifies
the two issues:

- #230 wants a coding-agent harness (Rivet `sandbox-agent`, `opencode` web, or
  any in-box HTTP daemon) reachable from outside over HTTP, with auth on the
  path, Kubernetes routing, and end-to-end SSE streaming.
- #312 wants `mitos workspace serve <name>` to return a ready, private
  dev-environment URL in seconds, warm-claimed from a forked golden workspace,
  with SDK parity returning a handle that carries the URL.

Both reduce to the same primitive: expose a guest port at a per-sandbox origin,
behind a chosen access tier, served by a controller-managed reverse proxy with
automatic TLS. The existing `internal/preview` package already implements the
signing, the per-sandbox vhost routing, the route table with GC, the upstream
per-sandbox bearer gate, and the `CertProvider` seam. This design extends that
spine rather than inventing a parallel one.

## 2. Goals and non-goals

Goals:

1. A per-sandbox URL at `<label>.<expose-domain>` that reaches a declared guest
   port, with each sandbox in its own browser origin.
2. A full access ladder, private by default: private, org, authenticated, signed
   link, public, plus composable network (IP allowlist) and forwardAuth
   (bring-your-own identity provider) layers.
3. Automatic TLS with a single wildcard certificate, terminated at a Go edge so
   post-quantum key exchange is negotiated where the client supports it.
4. The Kubernetes data path: the controller feeds the route table from Ready
   sandboxes; forkd proxies HTTP to the guest over the existing vsock tunnel,
   SSE-safe.
5. `mitos workspace serve` and SDK parity returning a handle with `.url`.
6. A worked coding-agent harness recipe end to end, including the fork-fan-out
   swarm where each child gets its own URL.
7. First-class self-host and SaaS multi-tenant operation from the same code.

Non-goals (explicitly deferred, with a seam left in place):

- Customer-brought custom domains (would use on-demand per-hostname TLS with an
  `ask` endpoint; the wildcard path here makes that a later, separate feature).
- Post-quantum certificate signatures / authentication (no publicly trusted CA
  issues ML-DSA certificates yet; tracked, not shipped).
- UDP exposure (TCP and HTTP only, matching the shipped tunnel).
- A payment or tiering layer (stays abstract behind the existing provider seam,
  per the enterprise open-core line).

## 3. Vocabulary

The v2 spec vocabulary rule (docs/api/v2-spec.md) keeps Kubernetes words out of
the developer surface. `claim`, `husk`, and `pool` stay internal. The
developer-facing verb for #312 is `serve`. The umbrella capability is "expose";
the user-visible noun for a created URL is a "served app".

## 4. Architecture overview

Components:

- expose-proxy (`cmd/expose-proxy`, evolves `cmd/preview-proxy`): the single
  internet-facing entrypoint. Terminates TLS, validates the Host against an
  allowlist, resolves the label to a route, enforces the access tier, injects
  the per-sandbox bearer, and reverse-proxies to the owning node.
- expose core (`internal/expose`, evolves `internal/preview`): host parsing,
  route table with GC, the signed-link signer, the access-tier enforcement, and
  the cert provider seam. The existing `internal/preview` types are renamed and
  extended; the signed-link signer is kept verbatim.
- controller wiring (`internal/controller`): a reconciler that watches Ready
  sandboxes and workspaces carrying expose config and syncs the route table.
- forkd expose handler (`internal/daemon`): an SSE-safe HTTP-proxy-to-guest
  endpoint that tunnels over the existing vsock PortForward stream.
- CLI and SDKs: `mitos workspace serve` plus `serve()` / `claim()` returning a
  handle with `.url` across all six SDKs.

Cluster data path:

```
browser / client
  -- HTTPS (wildcard cert, PQ KEX) -->  expose-proxy
       1. terminate TLS
       2. validate Host against allowlist; reject reserved names
       3. parse <label>.<expose-domain> -> label
       4. resolve label -> route (node, sandbox id, port, bearer, tier)
       5. enforce access tier (forward-auth / signed link / public / network)
       6. inject the per-sandbox bearer; strip any inbound copy
  -- HTTP -->  forkd :9091 on the owning node
       /v1/sandboxes/{id}/expose/{port}/*  (bearer-gated)
  -- vsock PortForward -->  guest agent
  -- dial 127.0.0.1:<port> -->  the in-guest daemon
```

Standalone path: `sandbox-server` exposes the same forkd expose handler on
loopback for the no-Kubernetes mode, inheriting the standalone tokenless trust
model on loopback only.

## 5. URL and origin model

- A served app lives at `<label>.<expose-domain>`, exactly one label deep. The
  default SaaS `expose-domain` is `mitos.run`, so the mitos website and the
  authenticated mitos app are served at `mitos.run` and the user app is served
  at `<label>.mitos.run`. The `expose-domain` is configurable so a self-hoster
  can point exposed traffic at a separate registrable domain (see section 11 and
  the security note in section 14).
- Multi-port flattens into the single label: `<port>-<name>.<expose-domain>`.
  This keeps everything one label deep so a single wildcard certificate covers
  it.
- Ephemeral sandbox label: the sandbox id, for example
  `sbx-ab12cd.mitos.run`. Always unique. This is the primitive used by the
  fork-fan-out swarm: each forked child is an independent microVM with its own
  sandbox id, hence its own label and URL.
- Stable workspace alias: a user-chosen label, globally unique, validated, not a
  reserved name, for example `myapp.mitos.run`. It always points at the
  workspace's current claim and is re-pointed when the workspace is re-served.
- Reserved-name blocklist: labels such as `www`, `app`, `console`, `api`,
  `gateway`, `admin`, `auth`, `login`, `account`, `mail`, `static`, `assets`,
  `cdn`, and `status` are reserved and rejected at claim time. This prevents a
  user from taking a control-plane hostname for phishing or interception. The
  blocklist is a config-loaded set so an operator can extend it.
- Each label is its own browser origin. The same-origin policy then isolates
  served apps from each other and from the control plane for script, DOM,
  localStorage, and credentialed fetch. Cookies are the one vector that crosses
  a shared registrable domain; section 14 covers the mitigation.

Label rules: a label is a single DNS label (no dots), lowercased, at most 63
characters, matching `[a-z0-9]([a-z0-9-]*[a-z0-9])?`, not in the reserved set,
and globally unique within the `expose-domain`.

## 6. TLS and post-quantum

- One wildcard certificate for `*.<expose-domain>` issued by cert-manager via
  ACME DNS-01. DNS-01 is required for a wildcard. One certificate covers
  unbounded sandboxes, keeps ACME off the sandbox-creation hot path, and sidesteps
  the Let's Encrypt per-registered-domain issuance ceiling (50 certificates per 7
  days). A wildcard counts as one issuance regardless of the number of
  subdomains. DNS-01 credentials are scoped with CNAME delegation of
  `_acme-challenge` to a delegated zone rather than granting root-zone access.
- TLS terminates at the Go expose-proxy. Built on Go 1.24 or newer with
  `tls.Config.CurvePreferences` left nil, `crypto/tls` negotiates the hybrid
  post-quantum group X25519MLKEM768 automatically with clients that support it
  (Chrome 131+, Firefox 132+, recent OpenSSL and BoringSSL). A guardrail unit
  test asserts the server offers `tls.X25519MLKEM768`, so a future TLS-hardening
  change that pins curves cannot silently drop post-quantum support. The operator
  kill switch for the large-ClientHello middlebox failure mode is documented
  (`GODEBUG=tlsmlkem=0`).
- Honest framing. The defensible claim is post-quantum key exchange:
  hybrid X25519MLKEM768 (NIST FIPS 203 ML-KEM-768 combined with X25519),
  protecting confidentiality against harvest-now-decrypt-later. The claim that is
  NOT made is post-quantum certificates or authentication: certificate signatures
  stay classical (ECDSA or RSA) because no publicly trusted CA issues
  post-quantum TLS certificates yet. README and threat-model wording is scoped to
  confidentiality only. This matches the "quantum secure when possible"
  intention.
- Bare metal, a first-class target, does not require a public CA. Two supported
  paths behind the same `CertProvider` seam: a self-hosted ACME server (for
  example step-ca) that cert-manager or CertMagic points at instead of Let's
  Encrypt; or an operator-provided `*.<expose-domain>` certificate loaded via
  `tls.LoadX509KeyPair`.
- Wildcard key blast radius is a consciously accepted trade-off: one private key
  serves all subdomains. Mitigation: short certificate lifetimes with automatic
  rotation, the key confined to the terminating expose-proxy, and per-sandbox L7
  isolation behind it. This is a threat-model delta (section 14).
- The expose-proxy edge holds the wildcard but routes reserved and control-plane
  hostnames (the apex, `app`, `api`, `www`, `console`) to the control-plane
  services, not to the guest data path. A request whose Host is reserved is never
  proxied to a sandbox.

## 7. Access ladder (sharing levels)

The expose config carries a `sharing` tier, default `private`, plus two
composable layers, `network` and `forwardAuth`. Tiers:

- private (default): only the owner identity. The proxy requires an
  authenticated mitos identity whose subject matches the sandbox or workspace
  owner. Deny by default.
- org: any authenticated identity in the same `mitos.run/org` as the resource.
- authenticated: any authenticated mitos identity.
- link: a signed, expiring URL. The existing HMAC-SHA256 detached-tag signer
  (kept verbatim from `internal/preview/sign.go`) binds the label and port with
  an absolute expiry. Hardened for browsers: on first hit the proxy exchanges the
  URL token for a `__Host-` session cookie and 302-redirects to a clean URL, so
  the bearer does not persist in the address bar, history, logs, or the Referer
  header. The signed link rotates on fork (section 14).
- public: no authentication. Allowed only as an explicit opt-in, served from the
  per-sandbox origin with a strict Content-Security-Policy, and auto-reverts to
  private on fork or restart (the Codespaces precedent).

Composable layers, applied in addition to the tier:

- network: an IP or CIDR allowlist, evaluated on every request (not only at
  login), so a network constraint cannot be outlived by a session.
- audience: identity allowlists that narrow which authenticated principals pass,
  the Cloudflare Access "Include" model (emails, email domains, groups). Two
  selectors, either or both:
  - `allowedPrincipals`: an explicit list of user identities (for example
    `alice@x.com`, `bob@y.com`); only a listed principal passes.
  - `allowedEmailDomains`: a list of email domains (for example `acme.com`); any
    authenticated identity whose email matches a listed domain passes (the
    "anyone at company.com" share). The match is on the VERIFIED email only: an
    identity whose IdP did not assert `email_verified` (or the SaaS console
    equivalent) is rejected, never trusted on a raw or unverified email claim.
    Trusting an unverified email domain is a known account-takeover vector and is
    a recorded threat-model item. Domain comparison is an exact, case-folded
    match on the registrable email domain, not a suffix match (so `evilacme.com`
    does not match `acme.com`).
  The audience layer requires an authenticated identity, so it composes with the
  org, authenticated, and forwardAuth paths (it resolves the principal email from
  the mitos console identity on the SaaS path and from the OIDC claims on the
  self-host/forwardAuth path); it is meaningless on the public tier (no identity)
  and is rejected as a misconfiguration there.
- forwardAuth: a bring-your-own identity-provider seam. The proxy implements the
  standard forward-auth subrequest contract (a 2xx from the auth service grants
  and its identity headers are copied onto the forwarded request; any other
  status, including a redirect to login, is returned to the client). This makes
  the proxy compatible with oauth2-proxy, Pomerium, Authelia, Traefik
  ForwardAuth, and Cloudflare Access style policies, which is how a self-hoster
  fronts exposed apps with their own SSO.

Enforcement is a single ordered pipeline so the matrix is testable: resolve Host
and label, look up the route and its tier, evaluate network layer, evaluate
forwardAuth layer if configured, then evaluate the tier (identity check, or
signed-link verify-and-exchange, or public pass-through), then evaluate the
audience layer against the resolved identity (verified email for the domain
selector), then inject the per-sandbox bearer and proxy. Any failure is terse and
never echoes a token. The audience selectors and the verified-email requirement
are built in the slice 4 auth ladder.

## 8. Identity backend

SaaS reuses the existing console identity as the authentication backend for the
private, org, and authenticated tiers. The proxy validates the console session
or API credential and resolves the subject and org. No new identity system is
introduced for the managed service.

Self-host uses the forwardAuth layer to delegate to the operator's own identity
provider, so a self-hosted cluster gates exposed apps with its existing SSO
without depending on the managed console. The two paths share the same
enforcement pipeline; only the identity source differs.

## 9. Cluster data path

- Route source: a controller reconciler watches sandboxes and workspaces that
  carry expose config and are Ready (`Status.Phase==Ready`, `Status.Endpoint`
  set). It maps each onto the route-table `ClaimState` seam the preview code
  already defines: label, backend (the owning forkd node plus sandbox id and
  port), the per-sandbox bearer (from the existing `<name>-sandbox-token`
  Secret), the tier, and Ready. `RouteTable.Sync` reconciles the table to exactly
  the current Ready set, so a sandbox that leaves Ready has its route reaped on
  the next sync and is immediately unroutable (404).
- forkd expose handler: a new endpoint `/v1/sandboxes/{id}/expose/{port}/*` on
  the forkd HTTP API (:9091), gated by the per-sandbox bearer (the same
  `requireBearer` middleware as exec and files). It opens or reuses a vsock
  PortForward to the guest and proxies the HTTP request and response through it,
  dialing the guest's `127.0.0.1:<port>` (the guest agent's existing hardcoded
  loopback dial). The handler is SSE-safe: it sets the reverse-proxy
  `FlushInterval` to -1, disables response buffering, and applies no idle timeout
  to a streaming response, so a long-lived SSE session against an in-guest agent
  streams token by token.
- The expose-proxy reverse-proxies to `https://<node>:9091/v1/sandboxes/{id}/expose/{port}/`
  with the per-sandbox bearer attached and the inbound Authorization stripped, so
  the caller cannot supply or override the upstream credential (confused-deputy
  defense, section 14).

## 10. Standalone path

`sandbox-server` mounts the same forkd expose handler on loopback. The standalone
forward keeps the documented tokenless loopback trust model for local use; the
authenticated, internet-facing path is the Kubernetes data path above. The
existing `POST /v1/sandboxes/{id}/forward` plain tunnel stays for raw socket use;
the new handler adds the HTTP, bearer-gated, SSE-safe path.

## 11. Claim-to-URL developer experience (#312)

CLI:

```
mitos workspace serve <workspace> [--port P] [--sharing private|org|authenticated|link|public] [--as <label>] [--ttl D]
```

It warm-claims a forked sandbox from the workspace's golden pool, binds the
sandbox to the workspace, waits for Ready, registers the expose route (the stable
alias if `--as` is given, otherwise the sandbox label), and prints the URL. The
default sharing tier is private. Output marks honestly which parts run on the
husk default versus the engine path, per the issue's "honest status" requirement.

SDK parity: every SDK gains a method that claims and serves in one call,
returning a handle that carries `.url` (and `.sharing`, `.expires`). The internal
verb is `claim`; the public method name follows each SDK's idiom while exposing
the URL on the returned handle. Parity is required across Python, TypeScript, Go,
Ruby, Rust, and Java per the SDK parity goal.

Docs: a new `docs/recipes/dev-environment.md` walks claim to ready URL against a
real cluster, and the README gains a capability row (claim to ready URL) under
the durable-state or agent-DX section, per the issue's README follow-up.

## 12. Harness recipe completion (#230)

`docs/recipes/agent-harness.md` is extended from "what works today" to a worked
end-to-end flow over the authenticated expose path: install and start the Rivet
`sandbox-agent` style daemon in the guest, expose its port at a private URL, and
stream an SSE coding-agent session end to end through the proxy. The fork-fan-out
section becomes concrete: warm one sandbox with the harness and dependencies in
place, fork it N ways, and each child is reachable at its own label and URL. The
recipe states the fork semantics explicitly: in-flight sessions do not transfer
across a fork, and the exposure token rotates per child (section 14).

## 13. CRD changes

- `Sandbox.spec` and `SandboxPool.spec.template`: add `exposedPorts` (the guest
  ports a sandbox declares as exposable) and `expose` (the per-port sharing tier,
  optional stable alias, optional network allowlist, optional forwardAuth
  reference, and the optional audience selectors `allowedPrincipals` and
  `allowedEmailDomains`, section 7).
- `Workspace.spec`: add a default `expose` block so `serve` has a tier and
  optional alias to apply without per-call flags.
- `Sandbox.status`: surface the exposed URL or URLs once Ready, so the controller
  and SDK read the URL from status rather than reconstructing it.
- `make generate manifests` regenerates deepcopy and CRD YAML; the api change
  carries its envtest coverage.

## 14. Multi-tenancy, self-host, and security

Multi-tenancy:

- SaaS: per-org namespaces (`mitos-org-<id>`) and the `mitos.run/org` label model
  already in place. The #213 quota and abuse-control envelope (per-org and per-IP
  rate buckets, concurrency and aggregate caps, deny-by-default egress on the
  tightest tier, suspend and kill-switch) is the hard gate before public
  exposure opens to untrusted self-serve tenants. A global registry enforces
  label uniqueness and reserved names across tenants on the shared
  `expose-domain`.
- Self-host: the same code path with the `expose-domain`, the identity source
  (forwardAuth to the operator IdP), and the TLS source (self-hosted ACME or a
  provided wildcard) all configurable. Open-core: no feature gating; the hosted
  service operates the managed version, it does not unlock features, consistent
  with the enterprise open-core line.

Security invariants (each lands with its threat-model delta in the same PR, per
the operating principles; touches to `internal/fork`, `internal/daemon`, and
`guest/agent-rs` get a named human reviewer):

1. Per-sandbox origin isolation. Each served app is its own subdomain, hence its
   own browser origin. Served responses carry a strict Content-Security-Policy
   including `frame-ancestors`, and `X-Frame-Options`, against clickjacking and
   cross-app script access.
2. Cookie scoping on a shared registrable domain. The control-plane app uses
   `__Host-` cookies (Secure, host-only, no Domain attribute, Path=/) and rejects
   any non-`__Host-` session cookie, so a user app at `<label>.mitos.run` cannot
   toss a `Domain=mitos.run` cookie that the app would honor. API CORS never
   wildcards the tenant subdomain space. Residual risk and the separate-domain
   escape hatch are documented below.
3. Host-header allowlist. The proxy validates the inbound Host against the
   allowlist of valid `<label>.<expose-domain>` names and rejects reserved and
   unknown hosts, defending against DNS rebinding and host-header injection even
   though the upstream dial target is fixed.
4. SSRF allowlist-of-one. The guest agent dials only `127.0.0.1` on the requested
   port; the upstream destination is never derived from request input at any hop.
   This invariant is preserved and asserted.
5. Confused-deputy defense. The proxy authorizes the caller for the specific
   backend before attaching the per-sandbox bearer, strips any inbound
   Authorization, and binds the signed link to the named sandbox so a token for
   one sandbox cannot be replayed against another (CWE-441).
6. Token-leak defense. Signed links are short-TTL, exchanged for a `__Host-`
   cookie on first hit, then redirected to a clean URL; programmatic callers use
   the Authorization header rather than the query string.
7. Token rotation on fork. The per-sandbox bearer and any exposure or signed-link
   secret are reseeded on fork, so a forked child never inherits the parent's
   exposure credential. This is a fork-correctness secret-inheritance hazard; it
   gets a fork-correctness suite test and a docs/fork-correctness.md entry.
8. Subdomain-takeover discipline. Teardown order is route reaped (immediate 404),
   then DNS reconciled, then resource decommissioned, never the reverse. The
   route GC already makes a terminated sandbox unroutable.
9. Reserved-name enforcement at claim time (section 5).
10. Verified-email audience rule (section 7). The `allowedEmailDomains` selector
    matches only an identity whose email the IdP asserted as verified
    (`email_verified`, or the SaaS console equivalent); an unverified or raw email
    claim is never honored for a domain rule. Domain comparison is an exact,
    case-folded match on the registrable email domain, not a suffix match. The
    audience layer is rejected as a misconfiguration on the identity-less public
    tier.
11. Wildcard-key blast radius (section 6) and the shared-domain residual risk
    (below) are recorded threat-model items.

Shared-domain residual risk, recorded honestly: serving user apps on
`*.mitos.run`, the same registrable domain as the authenticated app, forgoes the
browser-level Public Suffix List backstop that a separate registrable domain
(the GitHub `github.dev` model) would provide, and gives a user app at, for
example, `secure-login.mitos.run` some phishing affordance from the trusted
brand. Mitigations in place: `__Host-` cookies, reserved-name blocklist, strict
CSP, per-sandbox origin. Escape hatch: the `expose-domain` is configurable, so an
operator who wants full isolation points exposed traffic at a separate
registrable domain and registers it on the Public Suffix List. SaaS defaults to
`*.mitos.run` by product decision; this risk and its mitigations are documented
in the threat model.

## 15. Testing strategy

TDD throughout: a failing test lands in the same commit as each behavior change.

- Unit: Host allowlist and reserved-name rejection; the access-tier enforcement
  matrix (every tier and the network and forwardAuth layers, allow and deny); the
  forward-auth subrequest contract; the signed-link verify-and-exchange and clean
  redirect; the post-quantum curve guardrail; the SSE flush behavior; label
  validation. The existing signer and route-table GC tests are kept.
- Integration: controller route sync against envtest (add-on-ready,
  remove-on-terminate, alias re-point); the forkd expose handler over a real
  vsock tunnel and an end-to-end SSE stream against an in-guest daemon on the KVM
  runners; the fork-fan-out token-rotation test proving a child does not inherit
  the parent's exposure credential.
- The fork-correctness suite gains the exposure-token rotation case and must be
  green in CI before this surface ships to production tenants, per the sequencing
  gates.

## 16. Implementation sequencing

Each slice is a separate, shippable PR with its tests and docs in the same PR.

1. Substrate. Rename and extend `internal/preview` to `internal/expose`;
   port-in-label host scheme; Host allowlist and reserved names; configurable
   `expose-domain`. forkd SSE-safe HTTP-proxy-to-guest handler. Controller route
   sync from Ready sandboxes. Closes the #230 routing and SSE criteria with the
   private and link tiers.
2. TLS. cert-manager wildcard via DNS-01 and the Gateway or LoadBalancer Service;
   the post-quantum guardrail test; the bare-metal self-hosted-ACME and
   provided-wildcard paths.
3. Access ladder. The `sharing` tier CRD field and the org, authenticated,
   public, network, and forwardAuth tiers; the signed-link cookie exchange.
4. Claim-to-URL. `mitos workspace serve` and SDK `.url` parity across all six
   SDKs; the dev-environment recipe and README row. Closes #312.
5. Harness. The Rivet `sandbox-agent` SSE recipe and the fork-fan-out swarm.
   Closes the remaining #230 criteria.
6. Threat-model deltas and docs accompany every slice; the cross-cutting
   fork-correctness token-rotation test lands with slice 1's forkd change.

## 17. Open risks

- The shared-registrable-domain decision (section 14) is a product choice with a
  recorded residual risk; the separate-domain config path is the mitigation for
  operators who want full isolation.
- Public exposure to untrusted self-serve tenants is gated on #194 and #213; this
  design ships the mechanism and the private and link tiers first, and does not
  open untrusted public exposure until those gates clear.
- The expose-proxy adds a public ingress attack surface; edge rate limiting and a
  connection or SNI cap are sequenced with the #213 envelope.

## 18. References

- Shipped preview core: internal/preview, cmd/preview-proxy, docs/preview-urls.md
  (#126).
- Guest port forwarding: internal/daemon/forward.go,
  internal/sandboxrpc/portforward.go, guest/agent-rs/src/service/portforward.rs,
  docs/ports.md (#228 / #271).
- Harness recipe: docs/recipes/agent-harness.md (#279).
- Threat model section 7c (preview ingress) and the #213 abuse envelope row.
- Tenant model: internal/tenant/tenant.go (#172).
- v2 spec vocabulary rule: docs/api/v2-spec.md.
