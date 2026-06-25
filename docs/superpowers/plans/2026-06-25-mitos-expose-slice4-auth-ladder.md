# Mitos Expose Slice 4: the auth ladder (native OIDC)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Add the full access ladder to the expose proxy: private, org, authenticated, link, public tiers plus composable network (IP allowlist), audience (allowedPrincipals / allowedEmailDomains), and forwardAuth (BYO-IdP) layers, with the proxy as a native OIDC relying party on a central `auth.<expose-domain>` origin issuing per-subdomain `__Host-` session cookies.

**Architecture:** See docs/superpowers/specs/2026-06-25-mitos-expose-slice4-auth-ladder-design.md. The security-critical core (grant signer, session cookie, the authorize pipeline, audience matching, forwardAuth subrequest) is pure Go, fully unit-tested without a live IdP. The OIDC code flow sits behind a `TokenVerifier` seam tested with a fake verifier. The controller stamps the sandbox OrgID onto the route; the proxy resolves verified-email to orgs via a bearer-gated SaaS endpoint.

**Tech Stack:** Go crypto/hmac, net/http, controller-runtime, internal/saas/oidcauth (the existing OIDC verifier), Helm.

## Global Constraints
- Go 1.26; module mitos.run/mitos.
- No em/en dashes anywhere (comments, docs, YAML, commit messages); only `.` `,` `;` `:` and ASCII hyphen.
- Grants, session cookies, the OIDC client secret, the resolve bearer, and all HMAC keys are bearer credentials: never logged, never in an error body or argv; secret-sourced (env/Secret).
- The `allowedEmailDomains` selector matches only a VERIFIED email (email_verified), exact case-folded registrable-domain match, never a suffix.
- The route JSON contract (preview.ClaimState <-> controller.ExposeRoute, no json tags, identical field names/order) MUST stay byte-compatible when fields are added; add fields to BOTH in the same order.
- `make generate manifests` after any api/ change; generated files must be current.
- TDD; DCO sign-off; explicit-path staging; conventional commits. Lint both darwin and GOOS=linux. internal/controller tests need `eval "$(~/go/bin/setup-envtest use 1.31 -p env)"`. Validate chart changes with `helm template --kube-version 1.31.0` and `helm lint`.
- This is a security-sensitive surface; the threat-model delta lands in the same slice (Task 10).

---

### Task 1: route and CRD plumbing (OrgID + audience/network/forwardAuth)

Carry the sandbox owner org and the new policy fields from the CRD through the controller to the proxy route.

**Files:** Modify `api/v1/sandbox_types.go` (SandboxExpose), regenerate deepcopy + CRD; Modify `internal/preview/route.go` (Route + ClaimState); Modify `internal/controller/expose_routes.go` (ExposeRoute + BuildExposeRoutes). Tests: `api/v1/sandbox_types_test.go`, `internal/preview/route_test.go`, `internal/controller/expose_routes_test.go`.

**Interfaces:**
- `SandboxExpose` gains: `Network []string` (CIDRs), `ForwardAuthURL string`, `AllowedPrincipals []string`, `AllowedEmailDomains []string` (all optional, kubebuilder optional).
- `preview.Route` and `preview.ClaimState` and `controller.ExposeRoute` each gain, in IDENTICAL order with NO json tags: `OrgID string`, `Network []string`, `ForwardAuthURL string`, `AllowedPrincipals []string`, `AllowedEmailDomains []string` (appended after the existing fields; for ClaimState/ExposeRoute keep `Ready bool` last).
- `BuildExposeRoutes` populates `OrgID` from `tenant.OrgFromNamespace(sb.Namespace)` (empty string when not an org namespace) and copies the four policy fields from `sb.Spec.Expose`.

- [ ] Step 1: Failing tests. `api`: deepcopy round-trip with the new slice fields. `preview`: a route-table sync carrying OrgID + audience fields round-trips. `controller`: `TestBuildExposeRoutesStampsOrgAndPolicy` asserts a sandbox in namespace `mitos-org-acme` yields a route with `OrgID=="acme"` and the audience/network/forwardAuth fields copied from Spec.Expose.
- [ ] Step 2: Run; confirm fail.
- [ ] Step 3: Implement: add the SandboxExpose fields (with kubebuilder optional markers); `make generate manifests`; add the fields to Route, ClaimState, ExposeRoute in identical order; populate them in BuildExposeRoutes (import internal/tenant). Confirm `preview.ClaimState` and `controller.ExposeRoute` remain field-identical.
- [ ] Step 4: Run all three packages (controller with envtest); `go build ./...`; lint both; confirm `make generate manifests` yields no further diff.
- [ ] Step 5: Commit the api, generated, preview, controller files with `feat(expose): carry sandbox OrgID and audience/network/forwardAuth on the route`.

---

### Task 2: the grant signer

A short-lived, single-use, HMAC-signed grant the central auth origin issues and the app subdomain redeems.

**Files:** Create `internal/preview/grant.go`, `internal/preview/grant_test.go`.

**Interfaces:**
- `type Identity struct { Sub, Email string; EmailVerified bool; OrgIDs []string }`.
- `type GrantSigner struct{ ... }`, `NewGrantSigner(secret []byte) (*GrantSigner, error)` (>=16 byte secret floor).
- `Mint(label string, id Identity, expiresAt time.Time) (string, error)` returns `base64url(payload).base64url(tag)` with a random nonce in the payload; `Verify(token, label string, now time.Time) (Identity, error)` validates the HMAC (constant time), the label binding, the expiry, and rejects a reused nonce via an in-process single-use cache (a `sync.Map` of nonce->expiry with lazy GC). Domain-tag the HMAC (`mitos-expose-grant-v1\x00`).

- [ ] Step 1: Failing tests: round-trip; expired rejected; tampered rejected (payload + tag); wrong-key rejected; wrong-label rejected; SINGLE-USE (second Verify of the same token fails). 
- [ ] Step 2: Run; fail. Step 3: Implement (mirror the existing `internal/preview/sign.go` HMAC + crypto/subtle pattern; add the nonce single-use cache). Step 4: Run; pass; lint. Step 5: Commit `feat(expose): single-use HMAC grant for the central auth origin`.

---

### Task 3: the per-app session cookie

A `__Host-` HMAC session cookie carrying the identity, minted by the app subdomain after a valid grant or link.

**Files:** Create `internal/preview/session.go`, `internal/preview/session_test.go`.

**Interfaces:**
- `type SessionCodec struct{...}`, `NewSessionCodec(secret []byte) (*SessionCodec, error)`.
- `Encode(id Identity, expiresAt time.Time) (string, error)` and `Decode(value string, now time.Time) (Identity, error)` (HMAC, constant-time, expiry, tamper-reject; domain tag `mitos-expose-session-v1\x00`).
- `const SessionCookieName = "__Host-mitos_expose"` and a helper `NewSessionCookie(value string, ttl time.Duration) *http.Cookie` setting Secure, HttpOnly, SameSite=Lax, Path="/", and NO Domain (so it is host-only, `__Host-` compliant).

- [ ] Steps: failing tests (encode/decode round-trip; expiry; tamper; the cookie has Secure+HttpOnly+Path=/ and no Domain); implement; run; lint; commit `feat(expose): __Host- session cookie codec`.

---

### Task 4: the authorize pipeline

A pure function deciding access for the identity tiers + audience + network, given a route, an optional identity, and the client IP. (link and public are handled by the caller; this function covers network, the identity tiers, and audience.)

**Files:** Create `internal/preview/authz.go`, `internal/preview/authz_test.go`.

**Interfaces:**
- `type Decision int` (`Allow`, `DenyUnauthenticated` (401 / trigger login), `DenyForbidden` (403)).
- `func Authorize(route Route, id *Identity, clientIP net.IP) Decision`:
  - network: if `route.Network` non-empty and clientIP not in any CIDR -> DenyForbidden.
  - tier: public -> Allow (id ignored); authenticated -> id==nil ? DenyUnauthenticated : continue; private/org -> id==nil ? DenyUnauthenticated : (route.OrgID != "" and route.OrgID in id.OrgIDs ? continue : DenyForbidden); link -> the caller handles link separately (treat as Allow here only when id set by a link cookie, else DenyUnauthenticated) [document that link verification is upstream].
  - audience (after the tier passes, when id != nil): if `route.AllowedPrincipals` non-empty and id.Email not in it -> DenyForbidden; if `route.AllowedEmailDomains` non-empty and (not id.EmailVerified or domain(id.Email) not in it) -> DenyForbidden. audience on a public route (id==nil) -> DenyForbidden (misconfiguration: audience requires identity).
- A helper `emailDomain(email string) string` (lowercase, the part after the last '@').

- [ ] Step 1: Failing tests, the matrix: public allow; authenticated allow-with-id / deny-without; private+org allow when route.OrgID in id.OrgIDs, forbidden when not, unauthenticated when no id; network allow in-CIDR / forbid out; allowedPrincipals allow/deny; allowedEmailDomains allow only on verified + exact domain, deny unverified, deny suffix-trick (`evilacme.com` vs `acme.com`); audience-on-public -> forbidden.
- [ ] Steps 2-5: implement; run; lint; commit `feat(expose): the authorize pipeline (tiers, network, audience)`.

---

### Task 5: the forwardAuth subrequest

The BYO-IdP seam: a subrequest to the configured auth service; 2xx allows and copies identity headers; non-2xx is returned to the client; client-supplied identity headers are stripped first.

**Files:** Create `internal/preview/forwardauth.go`, `internal/preview/forwardauth_test.go`.

**Interfaces:** `func ForwardAuth(ctx, client *http.Client, authURL string, r *http.Request) (allow bool, identity *Identity, copyHeaders http.Header, status int, err error)`. It sends a GET to authURL with `X-Forwarded-Method/Uri/Host` set and the original cookies, NOT the body; on 2xx reads identity from `X-Auth-Request-Email`/`-User`/`-Groups` (groups -> OrgIDs); the proxy must strip any inbound `X-Auth-Request-*` before forwarding upstream.

- [ ] Steps: failing tests (2xx allow + identity from headers; 401 deny returns the status; inbound X-Auth-Request-* on the original request is not trusted); implement with an httptest auth server; run; lint; commit `feat(expose): forwardAuth subrequest seam for BYO-IdP`.

---

### Task 6: identity resolution (proxy client + SaaS endpoint)

The proxy resolves a verified email to orgIDs via a bearer-gated SaaS endpoint.

**Files:** Create `internal/preview/resolve.go` (+ test); add the SaaS handler in `internal/saas` (a new `IdentityResolveHandler`) and mount it (cmd/console or the saas admin mux) (+ test).

**Interfaces:**
- proxy: `type Resolver struct{ URL, Token string; Client *http.Client }`, `Resolve(ctx, email string) (accountID string, orgIDs []string, err error)` POSTing `{"email":...}` with the bearer; a nil/empty-URL Resolver returns a typed "resolution disabled" so the proxy falls back to the OIDC-claim/forwardAuth path.
- saas: `POST /internal/identity/resolve` constant-time bearer gate (from a shared secret), body `{"email"}`, response `{"accountId","orgIds"}`, wrapping the account service (FindOrCreateByEmail + Organizations). Never logs the email->org mapping beyond counts.

- [ ] Steps: failing tests (proxy client posts bearer + parses response; empty-URL no-op; saas handler bearer gate 401, resolves email->orgs via a fake/real account service); implement; run (saas tests); lint; commit `feat(expose): verified-email to org resolution (proxy client + saas endpoint)`.

---

### Task 7: the OIDC relying-party flow (behind a verifier seam)

The `auth.<domain>` handlers: `/start`, `/auth/callback`, grant issuance, the SSO cookie. The OIDC token verification is an injectable seam so the handlers are tested with a fake verifier.

**Files:** Create `internal/preview/oidc.go`, `internal/preview/oidc_test.go`.

**Interfaces:**
- `type TokenVerifier interface { Verify(ctx, rawIDToken string) (Identity, error) }` (the real impl wraps internal/saas/oidcauth; identity from sub/email/email_verified + a configurable groups claim -> OrgIDs).
- `type AuthOrigin struct { Verifier TokenVerifier; Exchanger <oauth2 code exchanger seam>; Grants *GrantSigner; SSO *SessionCodec; Resolver *Resolver; Routes *RouteTable; ... }` with `ServeStart`, `ServeCallback` handlers. `/start` validates `rd` against the route table (open-redirect defense), sets CSRF state + PKCE, redirects to the provider (or, if the SSO cookie is valid, straight to grant issuance). `/auth/callback` validates state, exchanges the code, verifies the ID token (Verifier), resolves orgs (Resolver, else the groups claim), sets the SSO cookie, issues a grant, and 302s to `<label>.<domain>/__mitos_auth/cb?grant=...`.

- [ ] Step 1: Failing tests with a FAKE TokenVerifier and a fake exchanger: a valid callback issues a grant and sets the SSO cookie; an invalid state is rejected; an `rd` that does not resolve to a route is rejected (no open redirect); a valid SSO cookie on /start skips the provider and issues a grant directly.
- [ ] Steps 2-5: implement (the real verifier wrapper is thin and built but exercised only via the fake in unit tests; note the live-IdP path is maintainer-verified); run; lint; commit `feat(expose): OIDC relying-party flow on the central auth origin`.

---

### Task 8: proxy integration

Wire host routing and the enforcement pipeline into the proxy request path.

**Files:** Modify `internal/preview/proxy.go` (+ test), `cmd/preview-proxy/main.go` (config wiring).

- The proxy distinguishes the `auth.<domain>` host (-> AuthOrigin handlers), the `__mitos_auth/cb` path on an app host (-> grant redemption + session cookie + 302), and normal app traffic. Normal app traffic: parse label/route; evaluate network; evaluate forwardAuth if set; for public -> proxy; for link -> existing signed-link-or-cookie; for private/org/authenticated -> read the `__Host-` session cookie (Decode); if absent/expired -> 302 to `auth.<domain>/start`; if present -> `Authorize(route, id, clientIP)`; on Allow inject the per-sandbox bearer and proxy; on DenyForbidden 403; on DenyUnauthenticated 302 to login. Apply audience in Authorize.
- main.go: flags/env for the OIDC issuer/clientID/clientSecret/redirect, the auth domain (default `auth.<domain>`), the grant + session HMAC secrets, the resolve URL + token. Construct the AuthOrigin and wire it.

- [ ] Step 1: Failing integration tests (httptest, fake verifier + a stub forkd backend): a private route with no cookie 302s to auth.<domain>/start; with a valid session cookie whose OrgIDs contain the route OrgID, the request proxies to the backend; with a cookie whose orgs do not match, 403; a public route proxies with no cookie; the `__mitos_auth/cb` path redeems a grant, sets the `__Host-` cookie, and 302s to the clean path.
- [ ] Steps 2-5: implement; run; build; lint; commit `feat(expose): wire the auth ladder into the proxy request path`.

---

### Task 9: Helm wiring

**Files:** Modify `deploy/charts/mitos/values.yaml`, `deploy/charts/mitos/templates/expose-proxy.yaml`, and the saas/console template that serves the resolve endpoint.

- `expose.oidc` (issuer, clientID, clientSecretRef, groupsClaim, authDomain default `auth.<domain>`), `expose.identityResolve` (url default the saas admin svc, tokenRef), the grant/session HMAC secrets in the `expose.secretName` Secret. The proxy Deployment gets these as env/flags. The console/saas Deployment serves `/internal/identity/resolve` on its ClusterIP admin port with the shared resolve token. All gated by `expose.enabled` + `expose.oidc.issuer` non-empty.
- [ ] Steps: edit; `helm template --kube-version 1.31.0 --set expose.enabled=true --set expose.domain=mitos.app --set expose.oidc.issuer=https://dex.example.com` renders the OIDC env + the resolve wiring; disabled renders none; `helm lint` passes; commit `feat(deploy): wire the expose proxy OIDC and identity-resolve config`.

---

### Task 10: docs and threat-model

**Files:** Modify `docs/preview-urls.md`, `docs/threat-model.md`.
- Document the auth ladder (tiers + composable layers), the central auth origin flow, the per-subdomain cookie isolation, the verified-email audience rule, the resolve endpoint, and the self-host forwardAuth path. Add the threat-model rows: auth-origin cookie isolation, single-use grant, CSRF+PKCE+rd-validation (open-redirect defense), verified-email, forwardAuth header stripping, the in-cluster resolve hop. Honest scope: gated behind #194/#213; SAML/SCIM deferred. No em/en dashes.
- [ ] Steps: edit; dash check; commit `docs(expose): the auth ladder, central auth origin, and threat-model delta`.

---

## Self-review notes
- The security-critical core (grant, session, authorize, audience, forwardAuth) is pure and fully unit-tested without a live IdP (Tasks 2-6). The OIDC flow (Task 7) is tested behind a fake verifier; the live-IdP path is maintainer-verified and documented.
- Route JSON contract: Task 1 adds fields to ClaimState AND ExposeRoute in identical order (no json tags) so the admin POST stays byte-compatible; the reviewer must confirm.
- private == org under the locked decision (both check route.OrgID in id.OrgIDs); documented, not a bug.
- Deferred: SAML/SCIM, device posture, on-demand CertMagic, per-org rate limiting (#213). Surface stays gated behind #194/#213.
