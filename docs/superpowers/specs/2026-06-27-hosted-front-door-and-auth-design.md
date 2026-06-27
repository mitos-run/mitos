# Hosted front door and auth (workstream 2) design

Date: 2026-06-27
Status: design (brainstorm output). Implementation plan is a follow-up.
Parent: docs/superpowers/specs/2026-06-27-hosted-launch-journey-design.md (workstream 2).
Tracks: hosted-SaaS epic (#208), onboarding (#341 abuse posture overlaps workstream 6).

## Summary

Workstream 2 builds the front door and identity for the hosted launch: one origin (`mitos.run`) routed by auth and slug like GitHub, native on-brand auth pages, GitHub and Google sign-in over the existing Go-native auth, and the onboarding funnel wired end to end (no card, $5 credit, durable). The first-success "aha" inside the console is workstream 3; this workstream gets a stranger to an authenticated, provisioned account and drops them into the app.

The backend spine mostly exists (onboarding service, accounts/keys, OIDC handlers, session, guardrails with a deny-egress 2-concurrent free tier, a Postgres store for accounts/keys). The gaps are concentrated: no signup/login/first-run UI, no identity provider wired, no single-origin edge routing, in-memory sessions and credit ledger, and no default spend cap.

## Decisions locked (this brainstorm)

- Topology: single origin `mitos.run`, GitHub model. Marketing and app on one domain. Org/user slug at root (`mitos.run/<org>`). Logged-out `/` shows marketing, logged-in `/` forwards to the app. Reserved-names list protects marketing and app paths from slug collision.
- Routing: Cilium Gateway API (the cluster's controller) routes `mitos.run` to a Go front-door reverse proxy (`cmd/frontdoor`), which is the auth+slug boundary and forks marketing vs app in code, mirroring paperclip.inc's `cloud-gateway`. Marketing, app, and auth are separate upstreams; marketing stays directly reachable. Not gateway ext_authz (Cilium does not use it; paperclip does auth in the reverse proxy).
- Identity: native-built auth pages in the Fluorescence brand, served on `mitos.run`, with "Continue with GitHub", "Continue with Google", and email. Backed by the existing Go-native auth. No hosted-IdP login screen, no external redirect, no vendor in the brand-critical path. Self-hosters keep the generic-OIDC path.
- Credit: $5 signup credit (`DefaultSignupCredit` -> `USD(5)`), no card.
- Durability: sessions, the credit ledger, and the onboarding pending store become Postgres-backed so a real signup survives a restart.

Correction recorded during design: the paperclip operator HTTPRoute (`~/repos/paperclip/paperclip-operator/internal/resources/httproute.go`) is intentionally minimal (a single catch-all rule, auth handled app-level, no gateway forward-auth). There is no paperclip forward-auth pattern to copy. Its `HTTPRouteSpec` CRD shape is a useful base only. This workstream builds the auth+slug gateway routing.

## Component 1: the edge (Cilium Gateway + front-door reverse proxy)

Goal: one origin, route by path and slug, fork by auth, marketing always reachable. This mirrors the proven paperclip.inc setup on the same Cilium cluster (its `cloud-gateway`), confirmed by reading the paperclip gitops.

- Cluster reality (confirmed): Cilium is the CNI and the Gateway API controller (GatewayClass `cilium`, controller `io.cilium/gateway-controller`, kube-hetzner). TLS via cert-manager + Let's Encrypt (`letsencrypt-prod` ClusterIssuer, HTTP01 solved over the gateway). DNS via external-dns watching HTTPRoute (Hetzner webhook). Auth is NOT done at the gateway (no ext_authz, no CiliumEnvoyConfig for auth); a reverse-proxy service is the auth+slug boundary, and the CiliumNetworkPolicy is the real tenant boundary.
- Gateway: `mitos-gateway` in a `gateway` namespace, `gatewayClassName: cilium`, HTTP + HTTPS listeners, TLS secret `mitos-run-tls` from a cert-manager `Certificate` (`mitos.run`, `www.mitos.run`). external-dns upserts the A record from the HTTPRoute. Copy the paperclip `gateway.yaml` / `certificate.yaml` / `cluster-issuer.yaml` with names changed.
- One HTTPRoute: `mitos.run` (and `www`) -> the front-door Service `mitos-frontdoor:8080`, with security response headers (HSTS, CSP `frame-ancestors 'none'`, X-Frame-Options DENY) set at the route, exactly like paperclip's marketing HTTPRoute. Plus the HTTP->HTTPS and www->apex redirect routes. ReferenceGrant if the route and Service are cross-namespace.
- Front-door reverse proxy (`cmd/frontdoor`, new Go, the `cloud-gateway` equivalent and the real auth+slug boundary). Upstreams are separate in-cluster Services: `mitos-marketing` (the static Astro build in a small container) and `mitos-console` (the app, `cmd/console`). It:
  - validates the session by calling the console's internal session endpoint (the existing `/internal/identity/resolve` / session middleware) with the request cookie, like paperclip's `cloud-gateway -> paperclip-id /internal/session`,
  - routes by path and slug: reserved marketing paths (`/`, `/pricing`, `/docs`, `/use-cases`, `/compare`, `/blog`, `/about`, assets) -> `mitos-marketing`, no auth; `/login`, `/signup`, `/verify`, `/auth/*` -> `mitos-console` auth surface, no session required; `/console/*`, `/onboarding/*`, `/app/*`, and the org slug `/<org>` and below -> `mitos-console`, session required (302 to `/login?next=<path>` when anon),
  - does the `/` fork in code: logged-out -> proxy `mitos-marketing` landing; logged-in -> proxy the app dashboard,
  - injects trusted identity headers to the app upstream (`X-Mitos-Account`, `X-Mitos-Org`, `X-Mitos-Stack-Role`, the shared server token), and strips those headers from inbound client requests so a client cannot forge them; the console trusts them only from the front door via the existing `WithCaller`,
  - resolves the org slug to a tenant before proxying app routes.
- Reserved names: a list (`pricing, docs, use-cases, compare, blog, about, login, signup, verify, auth, console, onboarding, app, api, settings, new, assets`) that org-slug creation rejects (GitHub-style); the slug match is lowest priority so reserved paths win.
- Network policy: CiliumNetworkPolicy default-deny ingress on the front door except from the `gateway` namespace (and the Cilium `ingress` entity), and scoped egress to DNS plus the `mitos-marketing` and `mitos-console` upstreams, mirroring paperclip's `cloud-gateway` netpols.

What exists vs build: the Cilium Gateway, cert-manager, external-dns, and the reverse-proxy-as-auth-boundary pattern EXIST in the paperclip gitops and are copied with names changed (paperclip's `cloud-gateway` is a Node/Fastify service; ours is a new Go `cmd/frontdoor` that calls the Go console). Build: `cmd/frontdoor` (session-validate, path/slug routing, marketing/app/auth proxying, trusted-header injection and stripping, reserved names), its Deployment/Service, the HTTPRoute + Certificate + ReferenceGrant + CiliumNetworkPolicy manifests, and an in-cluster `mitos-marketing` static container for the Astro build.

## Component 2: native auth pages and federation

Goal: login, signup, and verify that feel 100 percent native, on `mitos.run`, in brand.

- Pages (in the console SPA, pre-auth routes, or a small dedicated pre-auth bundle): `/login`, `/signup`, `/verify`. Rendered in the Fluorescence brand via `@mitos/brand`. Buttons: "Continue with GitHub", "Continue with Google", "Continue with email". No third-party login UI.
- Federation (decided: Dex): front GitHub + Google + email with self-hosted Dex (Go, CNCF), which presents one OIDC issuer to the existing console handlers. Least new console code, one issuer, self-hostable (consistent with the no-lock-in story). Google is OIDC natively; GitHub is OAuth2 and Dex's GitHub connector handles the userinfo path so the console never special-cases it.
- Email (decided: Resend): magic-link via the existing `SMTPEmailSender` (or Resend API) seam. Resend is GDPR-usable with a signed DPA + SCCs; before launch, sign the DPA and confirm data-residency acceptability (Resend infra is US-based); optionally run the privacy-legal DPA review on it. Raw tokens never logged.
- Session: on success, `LoginManager.SignIn` issues a session; the callback sets an HttpOnly, Secure, SameSite=Lax cookie. The edge forward-auth reads it. Add a sensible session TTL (sliding) once sessions are durable.
- Wiring: mount `/auth/*` (exists, gated on issuer config) and the pre-auth pages. The marketing `SIGNUP_BASE` (workstream 1, `website/src/data/site.mjs`) flips from `/docs/quickstart` to `/signup`, carrying the `?uc=` seed.

What exists vs build: OIDC handlers, `LoginManager`, `SessionStore`, session middleware EXIST. Build: the auth-page UI, the GitHub path (Dex or direct), provider config, and the SPA pre-auth routing.

## Component 3: onboarding funnel wired end to end

Goal: signup to a provisioned account in minutes, no card, $5 credit, seeded by use-case.

- Flow (exists in `internal/saas/onboarding`): `SignUp(email)` -> verify email -> `Verify(token)` provisions personal org, grants credit, issues first key (shown once), records funnel events. Flip `Mode` to `ModeOpen` only when workstream 6 gates clear; ship behind `MITOS_CONSOLE_SIGNUP` until then.
- $5 credit: change `DefaultSignupCredit()` to `USD(5)` (`internal/saas/billing/ledger.go:147`), or set `onboarding.WithSignupCredit(billing.USD(5))` at startup. One line.
- Seeded context: the `?uc=<slug>` from the marketing CTA is carried through signup and stored on the session/account so the console first-run (workstream 3) is use-case-shaped. Add a `uc` field to the signup request and persist it.
- Funnel metric: median time-to-first-sandbox is already instrumented (`EventRecorder`); wire a real analytics sink (follow-up) and surface the funnel in the console later.

What exists vs build: the funnel, credit grant, key issue, events EXIST and are tested. Build: the `$5` change, the `uc` seed plumbing, the real email provider, the SPA screens that call `/onboarding/*`.

## Component 4: durability

Goal: a real signup survives a restart.

- Postgres exists for accounts/orgs/keys (`internal/saas/pgstore`, migration `0001_init.sql`). Extend it (new migration `0002`) with durable implementations of:
  - `SessionStore` (so a console restart does not log everyone out),
  - `CreditLedger` (so the $5 balance and drawdowns persist; in-memory means balances reset, which is unacceptable for real billing),
  - onboarding `PendingStore` (so a verify link works across a restart).
- Keep the same interfaces; this is a store swap behind existing seams, not a model change. SpendCapStore, StatusStore, and SuspensionStore durability are workstream 4/6 follow-ups, noted as dependencies.

What exists vs build: pgstore + accounts/keys schema EXIST. Build: migration 0002 and the three durable store impls behind existing interfaces.

## Component 5: free-tier guardrails (happy path never feels them)

Goal: the abuse posture (also workstream 6 / #341) that lets "open and instant" be safe.

- Tiers exist (`internal/saas/quota`): Free is 2 concurrent sandboxes, deny-by-default egress, creation and API rate caps, abuse-port block. Confirm these are the launch Free defaults.
- Add a default hard spend cap for new orgs (none exists today) so a runaway swarm cannot make an unbounded bill against the $5 credit; wire it at provisioning.
- Idle TTL: automatic idle-sandbox reaping is a controller/daemon concern, not in this slice; note it as a dependency for cost control at launch.

What exists vs build: enforcer, tiers, kill-switch, rate limits, abuse ports EXIST. Build: the default-spend-cap-on-provision wiring; confirm tier values; track idle TTL as a dependency.

## Sequencing within workstream 2

1. Durable stores (sessions, credit ledger, pending) behind existing interfaces. Foundation; everything real depends on it.
2. Auth pages + federation (Dex or direct) + session cookie + `/auth` wiring.
3. The edge: Gateway + HTTPRoutes + forward-auth service + reserved names. Depends on sessions being resolvable.
4. Onboarding wiring: $5 credit, `uc` seed, real email, the SPA signup screens calling `/onboarding/*`.
5. Default spend cap on provision; confirm Free tier.
6. Flip the marketing `SIGNUP_BASE` to `/signup`.

Each gets its own implementation-plan task group; the build runs subagent-driven with review gates like workstream 1.

## Success metrics

- Median time-to-first-sandbox (signup -> first_exec).
- Funnel conversion per step.
- Login success rate; auth-page bounce.
- Zero forged-identity-header paths (the edge strips client-set identity headers; tested).

## Non-goals

- The in-console first-success aha (workstream 3).
- Real Stripe charge wiring and box/packaged pricing (workstream 4).
- Resolving the multitenancy isolation gate and the full #341 legal/abuse surface (workstream 6); this workstream wires the free-tier guardrails that already exist.
- A new tenancy or session model; durability is a store swap behind existing interfaces.

## Resolved (this brainstorm)

- Gateway controller: Cilium (confirmed by reading the paperclip gitops; GatewayClass `cilium`). Reuse the cluster's cert-manager + external-dns. Auth via the `cmd/frontdoor` reverse proxy, not gateway ext_authz.
- Federation: Dex (GitHub + Google + email behind one OIDC issuer).
- Email: Resend (sign the DPA, confirm data residency before launch).

## Open questions

- Where the pre-auth pages live: extra routes in the existing SPA bundle vs a small separate pre-auth bundle (affects first-paint and bundle size on the login page).
- Default hard spend cap value for the free tier (small, above the $5 credit headroom).
- Marketing in-cluster: serve the Astro build from a small static container (`mitos-marketing` Service) vs keep an external static host and proxy to it from the front door. Lean in-cluster static container for one-origin simplicity and Cilium netpol coverage.
- Resend DPA and data-residency sign-off (counsel / privacy-legal review).

## Risks

- The edge is new surface in the auth path; the identity-header forge vector must be closed at the gateway and tested. High care.
- Durable credit ledger is the difference between real money and a demo; it gates opening signup, alongside workstream 6.
- Dex adds an operational component; if that is unwanted, the direct-OAuth2 path is the fallback (more code, no extra service).
