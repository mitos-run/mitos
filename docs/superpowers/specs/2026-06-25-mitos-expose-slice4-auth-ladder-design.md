# Mitos Expose Slice 4: the auth ladder (native OIDC) design

Status: design, decisions locked by the project owner. Extends the main Mitos
Expose design (docs/superpowers/specs/2026-06-25-mitos-expose-design.md sections
7 and 8). Builds on slices 1, 2a, 2b, 3 (all merged).

## Decisions (locked)

1. Native OIDC in the proxy: the expose proxy is its own OIDC relying party
   (reusing internal/saas/oidcauth), not a forward-auth-only seam.
2. private = owner org: the controller stamps the sandbox's OrgID (from
   tenant.OrgFromNamespace) onto the route; private and org both check that the
   caller's orgs contain the sandbox's org. (private = "reachable within my org".)
3. All tiers in one slice: private, org, authenticated, link, public, plus the
   composable network, audience, and forwardAuth layers.

## Why a central auth origin

Each `<label>.<expose-domain>` is its own browser origin, deliberately, for
isolation. A browser session cookie must therefore be per subdomain, or a
malicious tenant app could read another app's session. Doing the OIDC code flow
on every app subdomain is both noisy and would scatter the OIDC client secret.
The standard resolution (oauth2-proxy, Coder, BeyondCorp) is a single central
auth origin that holds the OIDC relationship, plus short-lived grants that mint
per-app sessions.

`auth.<expose-domain>` is a reserved label (already in the reserved blocklist),
so it is never routed to a tenant; the proxy handles it itself. It is the OIDC
relying party and the central SSO origin. Its SSO cookie is scoped to the
`auth.<expose-domain>` host ONLY (host-only, `__Host-`), so no tenant app can
read it.

## The authentication flow

For a request to `<label>.<expose-domain>` whose tier needs identity (private,
org, authenticated):

1. The app subdomain handler checks for a valid per-app `__Host-` session cookie.
   If present and unexpired, the identity is taken from it; skip to enforcement.
2. No valid cookie: 302 to `https://auth.<expose-domain>/start?rd=<label>&path=<escaped original path>` with a fresh CSRF state.
3. `auth.<expose-domain>/start` checks its own `__Host-` SSO cookie. If absent,
   it runs the OIDC Authorization Code flow with PKCE against the configured
   issuer (internal/saas/oidcauth: discover, redirect to the provider, handle the
   `/auth/callback` with state validation, verify the ID token, extract `sub`,
   `email`, `email_verified`), then sets the auth-origin SSO cookie binding the
   verified identity.
4. The proxy resolves `verified-email -> [orgIDs]` (section "Identity resolution"
   below) and caches it in the SSO session.
5. `auth.<expose-domain>` validates `rd` against the live route table (the label
   must resolve to a real route; this prevents an open-redirect to an attacker
   host), then issues a short-lived single-use HMAC GRANT binding
   (label, sub, email, email_verified, orgIDs, expiry) and 302s to
   `https://<label>.<expose-domain>/__mitos_auth/cb?grant=<grant>&path=<path>`.
6. The app subdomain `__mitos_auth/cb` handler verifies the grant (HMAC,
   unexpired, single-use via a short nonce cache, label matches the host),
   enforces the tier and the audience layer against the grant's identity, sets a
   per-app `__Host-` session cookie (HMAC over the identity, ~1h TTL, host-only,
   Secure, SameSite=Lax, Path=/), and 302s to the original clean path.
7. Subsequent requests validate the local `__Host-` cookie; no round trip to
   `auth.<expose-domain>` until it expires.

The `link` tier keeps the slice-1 signed-URL flow but is hardened with the same
cookie exchange: a valid signed link sets a `__Host-` cookie and 302s to a clean
URL (no token in the address bar). `public` skips all of this. The grant and the
session cookie are bearer credentials, never logged.

## Identity resolution (verified email -> orgs)

Org membership lives in the mitos account database (the SaaS account service
auto-provisions a personal org per email), NOT in the OIDC token, so the proxy
cannot read orgs from the ID token alone. The proxy resolves identity via a new
bearer-gated in-cluster endpoint on the console/saas:

`POST /internal/identity/resolve` with `{ "email": "<verified email>" }` and
`Authorization: Bearer <shared resolve token>` returns
`{ "accountId": "...", "orgIds": ["..."] }`. It is plaintext in-cluster
(ClusterIP only, the documented in-cluster trust model), constant-time bearer
gate, never logs the email-to-org mapping beyond counts. It wraps the existing
account service (FindOrCreateByEmail or a lookup + Organizations).

Self-host without the SaaS account service: the org check falls back to (a) an
OIDC `groups` claim mapped to org ids (configurable claim name), or (b) the
forwardAuth path where an external IdP asserts identity and the proxy treats the
forwardAuth-provided groups as orgs. The resolve endpoint is optional; when
unset, the proxy uses the claim/forwardAuth path.

## The access tiers and the enforcement pipeline

A route carries a `Sharing` tier and the new `OrgID`. Enforcement is one ordered
pipeline so the matrix is testable:

1. Resolve host and label; reject reserved/unknown (404). `auth.<domain>` and the
   `__mitos_auth/*` and `__health` paths are handled before tier logic.
2. Evaluate the network layer (IP/CIDR allowlist) if set; reject 403 on miss
   (evaluated every request, not just at login).
3. Evaluate forwardAuth if configured (subrequest; non-2xx is returned to the
   client; on 2xx the identity headers are taken as the caller identity).
4. Evaluate the tier:
   - public: pass (no identity).
   - link: verify the signed URL or the link `__Host-` cookie; bind to the
     route's sandbox and port.
   - private, org: require a session (cookie or, first hit, the grant flow);
     require the caller's orgs to contain the route's OrgID.
   - authenticated: require a valid session (any identity).
5. Evaluate the audience layer against the resolved identity:
   - allowedPrincipals: the caller's email must be in the list.
   - allowedEmailDomains: the caller's VERIFIED email domain (exact, case-folded,
     registrable domain, not a suffix match) must be in the list; an unverified
     email is rejected.
   - audience is rejected as a misconfiguration on the public tier (no identity).
6. Inject the per-sandbox bearer and proxy to the forkd expose backend (slice 2a).

Any failure is terse and never echoes a token, grant, or cookie value.

## CRD and route changes

- `Sandbox.spec.expose` gains optional `network` (CIDR list), `forwardAuth` (a
  reference: URL), `allowedPrincipals` (string list), `allowedEmailDomains`
  (string list). `sharing` already exists.
- The controller stamps the sandbox's `OrgID` (tenant.OrgFromNamespace(namespace),
  empty for non-org namespaces) and the audience/network/forwardAuth fields onto
  the route DTO/ClaimState and the proxy Route.
- `internal/preview` Route/ClaimState gain `OrgID`, `Network []string`,
  `ForwardAuthURL string`, `AllowedPrincipals []string`, `AllowedEmailDomains
  []string`.

## Helm wiring

- expose.oidc: issuer, clientID, clientSecret (Secret ref), the auth domain
  (defaults to `auth.<expose-domain>`), the redirect URL.
- expose.identityResolve: the saas resolve endpoint URL + the shared bearer token
  (Secret ref). The console/saas Deployment serves `/internal/identity/resolve`
  on its admin/ClusterIP port.
- The proxy serves `auth.<expose-domain>` (the wildcard cert already covers it).

## Security invariants (each with a threat-model delta)

1. Auth-origin cookie isolation: the `auth.<domain>` SSO cookie is `__Host-`
   (host-only), so a tenant app cannot read it; per-app session cookies are
   `__Host-` and host-only, isolated per subdomain.
2. Grant is short-lived (seconds), single-use (nonce cache), HMAC-signed, and
   bound to the label; a leaked grant cannot be replayed or used for another app.
3. CSRF state on the OIDC flow; PKCE; the `rd` redirect target is validated
   against the live route table (no open redirect to an attacker host).
4. Verified email only for the domain audience selector; unverified email
   rejected (account-takeover defense).
5. Identity resolution and the OIDC client secret and the session/grant HMAC keys
   are bearer credentials: never logged, env or Secret sourced, never argv.
6. The forwardAuth subrequest strips any client-supplied identity headers before
   adding the proxy-trusted ones (no header spoofing).
7. The session cookie carries a signed identity with a bounded TTL; tier and
   audience are re-checked on cookie validation (a cookie minted for one tier is
   bound to its route).
8. network and audience layers are evaluated on every request, so a constraint
   cannot be outlived by a session.

## Testing strategy

The OIDC code flow against a real issuer cannot run in CI, so the testable units
are isolated from the live IdP:
- The grant signer (HMAC mint/verify, expiry, single-use, label binding).
- The per-app session cookie (mint/verify, `__Host-` attributes, TTL, tamper).
- The enforcement pipeline matrix (every tier and the network/audience/forwardAuth
  layers, allow and deny), driven by an injected identity (no live OIDC).
- The audience matching (principals, verified-domain exact match, unverified
  reject, public-tier rejection).
- The resolve-endpoint contract (bearer gate, request/response shape) with an
  httptest server.
- The forwardAuth subrequest contract (2xx allow + header copy, non-2xx deny,
  client-header stripping).
- The OIDC verification is wrapped behind an interface (a `TokenVerifier` seam)
  so the flow handlers are tested with a fake verifier; the real verifier (the
  oidcauth wrapper) is exercised only at integration time with a mock OIDC server
  if feasible, otherwise documented as the maintainer-verified path.

## What is deferred

SAML/SCIM (the console B3e/f track); device posture; the on-demand CertMagic
path; per-org rate limiting (the #213 envelope). This surface stays gated behind
the #194 review and the #213 abuse envelope before untrusted public exposure.
