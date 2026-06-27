# Workstream 2: front-door proxy + Dex Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Put all of `mitos.run` behind one origin: a Go front-door reverse proxy (`cmd/frontdoor`) that routes by path and slug, validates the session, forks marketing-vs-app, and injects trusted identity headers, fronted by the cluster's Cilium Gateway; plus Dex federating GitHub and Google into the console's existing OIDC. Mirrors paperclip.inc's `cloud-gateway` pattern on the same Cilium cluster.

**Architecture:** The Cilium Gateway terminates TLS for `mitos.run` and sends everything to the `mitos-frontdoor` Service. The front-door (Go, `httputil.ReverseProxy`) resolves the `mitos_session` cookie by calling a new bearer-authed console endpoint `POST /internal/session/resolve`, then: reserved marketing paths and logged-out `/` proxy to the marketing upstream; `/login` `/signup` `/verify` `/auth/*` proxy to the console without requiring a session; `/console/*` `/app/*` and the org slug `/<org>` require a session (302 to `/login?next=` when anon) and proxy to the console with `X-Mitos-Account`/`X-Mitos-Org` injected and any client-set identity headers stripped. Dex runs in-cluster and presents one OIDC issuer to the console; the GitHub/Google connector secrets are k8s Secrets.

**Tech Stack:** Go (net/http, httputil.ReverseProxy, slog), the existing `internal/saas` session layer, the Helm chart (`deploy/charts/mitos`), Cilium Gateway API, cert-manager, external-dns, Dex.

## Global Constraints

- No em (U+2014) or en (U+2013) dashes anywhere (Go, YAML, comments, commits). ASCII hyphen only.
- Go: `fmt.Errorf("ctx: %w", err)`; octal `0o644`; gofmt + `go vet ./...` clean. Secrets, session tokens, and bearer tokens are NEVER logged or placed in errors (log ids/counts only). Reuse the constant-time compare pattern from `identity_resolve.go`.
- DCO: every commit `git commit -s`. Conventional prefixes. Stage explicit paths only.
- Work in the ISOLATED WORKTREE /Users/jannesstubbemann/repos/mitos-run/mitos-ws2 (branch hosted-launch-journey). Run all commands there.
- Go commands from the worktree root. Manifest lint: `helm template deploy/charts/mitos ...` and, if available, `kubeconform`.
- Mirror existing patterns: `internal/saas/identity_resolve.go` (bearer-authed internal handler), `internal/preview/proxy.go` (reverse proxy director/error handler/FlushInterval), `cmd/preview-proxy/main.go` (flags + graceful shutdown), `Dockerfile.gateway`, `deploy/charts/mitos/templates/console.yaml` + `expose-proxy.yaml` + `_helpers.tpl`.
- Cluster facts to mirror (from paperclip gitops, confirmed): GatewayClass `cilium`; cert-manager ClusterIssuer `letsencrypt-prod` (HTTP01 over the gateway); external-dns watches HTTPRoute (Hetzner webhook); Cilium NetworkPolicy is the tenant boundary.

---

### Task 1: Console internal session-resolve endpoint

The front-door needs to turn a `mitos_session` cookie value into account + org. `/internal/identity/resolve` resolves by EMAIL, not session, so add a session-resolver.

**Files:**
- Create: `internal/saas/session_resolve.go` (`NewSessionResolveHandler(sessions *SessionService, token string, log *slog.Logger) http.Handler`)
- Test: `internal/saas/session_resolve_test.go`
- Modify: `cmd/console/main.go` (mount `POST /internal/session/resolve`, reusing the existing `MITOS_IDENTITY_RESOLVE_TOKEN` bearer)

**Interfaces:**
- Consumes: `SessionService.Resolve(ctx, token) (Account, []Organization, error)`, the bearer-token constant-time check pattern from `identity_resolve.go`.
- Produces: `POST /internal/session/resolve` with bearer auth; request `{"session":"<cookie-value>"}`; response `{"accountId":"...","orgId":"...","orgs":[{"id":"...","name":"..."}]}` (orgId is the primary/personal org, or the first org); 401 on bad bearer; 401 with an empty/again-terse body when the session is invalid (do not distinguish unknown from expired).

- [ ] **Step 1: Write the failing test**

Create `session_resolve_test.go`. Read `identity_resolve_test.go` first and mirror its bearer-auth test setup. Cases: (a) missing/wrong bearer -> 401; (b) valid bearer + valid session token -> 200 with the resolved accountId and orgId (seed a session via the SessionService used by the handler); (c) valid bearer + unknown session -> 401 (terse). Assert the JSON shape.

```go
// shape sketch; mirror identity_resolve_test.go exactly for setup
func TestSessionResolveHandler(t *testing.T) {
  // build AccountService + SessionService, create an account + session token
  // POST /internal/session/resolve with Authorization: Bearer <token> and {"session": <raw>}
  // assert 200 and body.accountId / body.orgId
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/saas/ -run TestSessionResolveHandler -v`
Expected: FAIL (handler undefined).

- [ ] **Step 3: Implement the handler**

Create `session_resolve.go` mirroring `identity_resolve.go`: constant-time bearer check (same env token), decode `{"session":...}`, call `sessions.Resolve`, map the result to `{accountId, orgId, orgs}` (pick the personal/first org as `orgId`), return 401 (terse) on `ErrSessionInvalid`. Never log the session value or the bearer. Wrap decode/encode errors with `%w` internally but return terse client messages.

- [ ] **Step 4: Mount it**

In `cmd/console/main.go`, next to the `/internal/identity/resolve` mount (line ~189), add:
```go
mux.Handle("POST /internal/session/resolve", saas.NewSessionResolveHandler(sessions, token, logger))
```
Use the same `token` (`MITOS_IDENTITY_RESOLVE_TOKEN`) and the `sessions` `*SessionService` already constructed in main. Confirm `sessions` is in scope there (it is used by the session middleware).

- [ ] **Step 5: Run the test, build, vet**

Run: `go test ./internal/saas/ -run TestSessionResolveHandler -v && go build ./... && go vet ./internal/saas/ ./cmd/console/`
Expected: PASS, clean.

- [ ] **Step 6: Commit**

```bash
git add internal/saas/session_resolve.go internal/saas/session_resolve_test.go cmd/console/main.go
git commit -s -m "feat(console): internal session-resolve endpoint for the front door"
```

---

### Task 2: Front-door routing + proxy core

The heart: a router that decides, per request, the upstream and whether a session is required, validates the session via Task 1's endpoint, injects/strips identity headers, and forks `/`.

**Files:**
- Create: `internal/frontdoor/router.go` (the pure routing decision)
- Create: `internal/frontdoor/proxy.go` (the reverse proxy + session resolution + header handling)
- Test: `internal/frontdoor/router_test.go`, `internal/frontdoor/proxy_test.go`

**Interfaces:**
- Consumes: `httputil.ReverseProxy` (mirror `internal/preview/proxy.go`), an injectable `SessionResolver` interface (so tests fake the console call), the reserved-names list.
- Produces:
  - `type Decision struct { Upstream string; RequireSession bool; IsRoot bool }` and `func Decide(path string, reserved map[string]bool) Decision` where Upstream is one of `"marketing"`, `"console"`.
  - `type SessionResolver interface { Resolve(ctx context.Context, sessionToken string) (Identity, error) }`, `type Identity struct { AccountID, OrgID string }`, `var ErrNoSession = errors.New(...)`.
  - `type Proxy` with `ServeHTTP`, constructed with marketing+console upstream URLs, a `SessionResolver`, and the reserved set.

- [ ] **Step 1: Write the failing router test**

Create `router_test.go` covering `Decide`:
- reserved marketing paths (`/`, `/pricing`, `/docs`, `/use-cases`, `/compare`, `/blog`, `/about`, `/assets/x.js`) -> Upstream marketing, RequireSession false; `/` also IsRoot true.
- auth paths (`/login`, `/signup`, `/verify`, `/auth/login`, `/auth/callback`) -> Upstream console, RequireSession false.
- app + slug paths (`/console/keys`, `/app`, `/acme`, `/acme/sandboxes`) -> Upstream console, RequireSession true.
- a reserved word as a would-be slug (`/login`) is NOT treated as an org slug (reserved wins).

```go
func TestDecide(t *testing.T) {
  reserved := frontdoor.DefaultReserved()
  // table of {path, wantUpstream, wantRequireSession, wantIsRoot}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/frontdoor/ -run TestDecide -v`
Expected: FAIL (package/func undefined).

- [ ] **Step 3: Implement `router.go`**

Create `internal/frontdoor/router.go`: `DefaultReserved()` returning the set (`pricing, docs, use-cases, compare, blog, about, login, signup, verify, auth, console, onboarding, app, api, settings, new, assets`), and `Decide(path, reserved)`:
- exact `/` -> marketing, IsRoot true, RequireSession false (the proxy decides the fork by session).
- first path segment in the marketing-reserved subset (pricing/docs/use-cases/compare/blog/about/assets and their subpaths) -> marketing, no session.
- first segment in {login,signup,verify,auth} -> console, no session.
- first segment in {console,onboarding,app,api,settings,new} -> console, session required (onboarding stays public though: keep `/onboarding/*` no-session; adjust the set so onboarding is in the no-session group).
- otherwise (a non-reserved first segment) -> treat as an org slug: console, session required.
Document the precedence so reserved always beats slug.

- [ ] **Step 4: Write the failing proxy test**

Create `proxy_test.go` using `httptest`:
- two `httptest.Server` upstreams (marketing echoes "MKT", console echoes the request headers it received).
- a fake `SessionResolver` returning a fixed Identity for token "good", `ErrNoSession` otherwise.
- assert: GET `/pricing` -> proxied to marketing (body "MKT"), no auth call. GET `/console/keys` with cookie `mitos_session=good` -> proxied to console with `X-Mitos-Account`/`X-Mitos-Org` set and any inbound `X-Mitos-Account` from the client STRIPPED (send a forged one and assert it is replaced). GET `/console/keys` with no cookie -> 302 to `/login?next=%2Fconsole%2Fkeys`. GET `/` with good cookie -> console (app); GET `/` with no cookie -> marketing.

- [ ] **Step 5: Run to verify it fails**

Run: `go test ./internal/frontdoor/ -run TestProxy -v`
Expected: FAIL.

- [ ] **Step 6: Implement `proxy.go`**

Create `internal/frontdoor/proxy.go` mirroring `internal/preview/proxy.go`'s director/error-handler/`FlushInterval = -1` pattern. For each request: strip inbound `X-Mitos-*` identity headers first (forge protection). Call `Decide`. If RequireSession or (IsRoot): read the `mitos_session` cookie, call `SessionResolver.Resolve`. On IsRoot: authed -> console upstream, anon -> marketing upstream. On RequireSession: anon -> 302 `/login?next=<escaped path>`; authed -> set `X-Mitos-Account`/`X-Mitos-Org` and proxy to console. Else proxy to the decided upstream. Use a single-host reverse proxy per upstream (build two, reuse). Error handler returns 502 with a terse message and logs label+status only (no secrets). Never log the cookie value.

- [ ] **Step 7: Run tests, build, vet**

Run: `go test ./internal/frontdoor/ -v && go build ./... && go vet ./internal/frontdoor/`
Expected: PASS, clean.

- [ ] **Step 8: Commit**

```bash
git add internal/frontdoor/router.go internal/frontdoor/router_test.go internal/frontdoor/proxy.go internal/frontdoor/proxy_test.go
git commit -s -m "feat(frontdoor): auth+slug routing reverse proxy with session fork and header injection"
```

---

### Task 3: frontdoor binary + Dockerfile + HTTP SessionResolver

**Files:**
- Create: `cmd/frontdoor/main.go`
- Create: `internal/frontdoor/httpresolver.go` (the real `SessionResolver` that calls `POST /internal/session/resolve`)
- Test: `internal/frontdoor/httpresolver_test.go`
- Create: `Dockerfile.frontdoor`

**Interfaces:**
- Consumes: Task 2's `Proxy` + `SessionResolver`; the console session-resolve endpoint (Task 1).
- Produces: a runnable binary listening on `:8080`, flags/env for the marketing upstream URL, console upstream URL, the session-resolve URL, and the bearer token; an `HTTPSessionResolver` implementing `SessionResolver`.

- [ ] **Step 1: Write the failing resolver test**

`httpresolver_test.go`: an `httptest.Server` standing in for the console session-resolve endpoint (asserts the bearer header and `{"session":...}` body, returns `{accountId,orgId}`); assert `HTTPSessionResolver.Resolve("good")` returns the Identity and that a 401 maps to `ErrNoSession`.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/frontdoor/ -run TestHTTPResolver -v` (FAIL).

- [ ] **Step 3: Implement `httpresolver.go`**

`HTTPSessionResolver{ url, token, client }` POSTs `{"session":token}` with `Authorization: Bearer <token>` to the resolve URL; 200 -> decode Identity; 401 -> `ErrNoSession`; other -> a wrapped error. Never log the session or bearer. Short timeout.

- [ ] **Step 4: Implement the binary**

`cmd/frontdoor/main.go` mirroring `cmd/preview-proxy/main.go`: flags/env `MITOS_FRONTDOOR_ADDR` (`:8080`), `MITOS_FRONTDOOR_MARKETING_URL`, `MITOS_FRONTDOOR_CONSOLE_URL`, `MITOS_FRONTDOOR_SESSION_RESOLVE_URL`, bearer from `MITOS_IDENTITY_RESOLVE_TOKEN`. Build the `HTTPSessionResolver` + `Proxy`, an `http.Server` with read/write/idle timeouts, and the SIGTERM/SIGINT graceful-shutdown loop from preview-proxy. Log startup (URLs, not the token).

- [ ] **Step 5: Dockerfile**

Create `Dockerfile.frontdoor` mirroring `Dockerfile.gateway` (golang builder -> `go build -o frontdoor ./cmd/frontdoor/`, distroless static nonroot, ENTRYPOINT `/frontdoor`).

- [ ] **Step 6: Run tests, build, vet**

Run: `go test ./internal/frontdoor/ -v && go build ./... && go vet ./cmd/frontdoor/ ./internal/frontdoor/`
Expected: PASS, clean. Confirm `go build -o /tmp/frontdoor ./cmd/frontdoor/` produces a binary.

- [ ] **Step 7: Commit**

```bash
git add cmd/frontdoor/main.go internal/frontdoor/httpresolver.go internal/frontdoor/httpresolver_test.go Dockerfile.frontdoor
git commit -s -m "feat(frontdoor): binary, HTTP session resolver, and image"
```

---

### Task 4: Dex deployment + console wiring (CLUSTER-VERIFIED)

Dex federates GitHub + Google into one OIDC issuer for the console. These are deploy manifests; local verification is `helm template` lint only. The live OAuth round trip is verified on the cluster.

**Files:**
- Create: `deploy/charts/mitos/templates/dex.yaml` (Deployment, Service, ConfigMap, gated by `.Values.dex.enabled`)
- Modify: `deploy/charts/mitos/values.yaml` (a `dex` block + wire `console.oidc.issuerURL` default to the in-cluster Dex)
- Create: `deploy/charts/mitos/templates/dex.md` or a values comment documenting the required Secrets (GitHub/Google client id+secret, the console static-client secret)

**Interfaces:**
- Consumes: the console OIDC env (`MITOS_CONSOLE_OIDC_ISSUER` etc. already templated in console.yaml).
- Produces: a Dex issuer at `https://mitos.run/dex` (or an internal issuer URL) the console trusts; GitHub + Google connectors; a static OIDC client for the console.

- [ ] **Step 1: Author the Dex ConfigMap + Deployment + Service**

`dex.yaml`: a Dex `ConfigMap` (issuer, storage: kubernetes or memory for now, the `staticClients` entry for the console with `redirectURIs: [https://mitos.run/auth/callback]`, and `connectors` for `github` and `google` reading client id/secret from env via `$GITHUB_CLIENT_ID` style with `envVarsFromSecret`), a Deployment (image `ghcr.io/dexidp/dex:<pinned>`, mounts the config, reads connector secrets from a k8s Secret), and a Service. Pin the Dex image tag. No secret values in the template; only `secretKeyRef`. Follow `_helpers.tpl` naming.

- [ ] **Step 2: Wire values**

In `values.yaml` add:
```yaml
dex:
  enabled: false
  image: ghcr.io/dexidp/dex:v2.40.0
  issuer: https://mitos.run/dex
  connectorsSecret: ""   # Secret with github-client-id, github-client-secret, google-client-id, google-client-secret
  consoleClientSecret: "" # Secret with the static client secret shared with console.oidc.clientSecretRef
```
Document that when `dex.enabled`, `console.oidc.issuerURL` should be the Dex issuer and `console.oidc.clientID` the static client id.

- [ ] **Step 3: Lint**

Run: `helm template deploy/charts/mitos --set dex.enabled=true --set dex.connectorsSecret=foo --set dex.consoleClientSecret=bar > /tmp/dex-render.yaml && head -5 /tmp/dex-render.yaml`
Expected: renders without template errors; the Dex Deployment/Service/ConfigMap appear; no secret values inlined. If `kubeconform` is available, run it on the rendered output. NOTE in the report: the live OAuth flow (GitHub/Google app registration, the actual redirect) is verified on the cluster, not here.

- [ ] **Step 4: Commit**

```bash
git add deploy/charts/mitos/templates/dex.yaml deploy/charts/mitos/values.yaml
git commit -s -m "feat(deploy): Dex federation for GitHub and Google (cluster-verified)"
```

---

### Task 5: Cilium Gateway + front-door + marketing manifests (CLUSTER-VERIFIED)

The single-origin edge. Deploy manifests; local verification is `helm template` lint only.

**Files:**
- Create: `deploy/charts/mitos/templates/frontdoor.yaml` (frontdoor Deployment + Service, gated by `.Values.frontdoor.enabled`)
- Create: `deploy/charts/mitos/templates/gateway.yaml` (Cilium `Gateway`, `HTTPRoute` mitos.run -> frontdoor Service, cert-manager `Certificate`, `ReferenceGrant` if cross-namespace, `CiliumNetworkPolicy`)
- Create: `deploy/charts/mitos/templates/marketing.yaml` (a small static container Service serving the Astro dist, gated; or a documented placeholder)
- Modify: `deploy/charts/mitos/values.yaml` (`frontdoor`, `gateway`, `marketing` blocks)

**Interfaces:**
- Consumes: the frontdoor image (Task 3), the console Service, the marketing Service, the cert-manager ClusterIssuer (`letsencrypt-prod`), GatewayClass `cilium`.
- Produces: `mitos.run` served end to end through the gateway -> frontdoor -> {marketing, console}.

- [ ] **Step 1: frontdoor Deployment + Service**

`frontdoor.yaml`: Deployment (the `mitos.image` frontdoor image, env `MITOS_FRONTDOOR_MARKETING_URL`/`CONSOLE_URL`/`SESSION_RESOLVE_URL` pointing at the in-cluster Services, `MITOS_IDENTITY_RESOLVE_TOKEN` from the shared Secret), container `:8080`, Service port 80. Mirror console.yaml.

- [ ] **Step 2: Gateway + HTTPRoute + Certificate + NetworkPolicy**

`gateway.yaml` mirroring the paperclip gitops with names changed: a `Gateway` (gatewayClassName `cilium`, HTTP + HTTPS listeners, TLS secret `mitos-run-tls`), a cert-manager `Certificate` for `mitos.run`/`www.mitos.run` via the ClusterIssuer, an `HTTPRoute` (`mitos.run`, `www`) -> the `mitos-frontdoor` Service with the security response headers (HSTS, CSP frame-ancestors none, X-Frame-Options DENY) and the HTTP->HTTPS + www->apex redirects, a `ReferenceGrant` if the route and Service are in different namespaces, and a `CiliumNetworkPolicy` default-deny on the frontdoor except from the gateway with scoped egress to marketing + console. Gate everything on `.Values.gateway.enabled`.

- [ ] **Step 3: marketing static**

`marketing.yaml`: a minimal static-file Deployment+Service (e.g. `nginxinc/nginx-unprivileged` or a tiny caddy) serving the Astro dist from a mounted volume or a baked image, gated by `.Values.marketing.enabled`. If baking the Astro build into an image is out of scope for this slice, ship a documented placeholder Service and note that the marketing image build (from the website repo) is the integration follow-up.

- [ ] **Step 4: values + lint**

Add `frontdoor`, `gateway` (className, hostname `mitos.run`, tls.issuerRef), and `marketing` blocks to `values.yaml`. Run:
```bash
helm template deploy/charts/mitos --set frontdoor.enabled=true --set gateway.enabled=true --set gateway.hostname=mitos.run --set gateway.tls.issuerRef.name=letsencrypt-prod > /tmp/edge-render.yaml && grep -c "kind: HTTPRoute\|kind: Gateway\|kind: Certificate" /tmp/edge-render.yaml
```
Expected: renders cleanly; Gateway + HTTPRoute + Certificate present; if `kubeconform` is available run it (note that Gateway API + Cilium CRDs may need their schemas; a schema-miss is not a failure of the manifest). REPORT that the live gateway, DNS, and TLS are verified on the cluster via the paperclip deploy, not locally.

- [ ] **Step 5: Commit**

```bash
git add deploy/charts/mitos/templates/frontdoor.yaml deploy/charts/mitos/templates/gateway.yaml deploy/charts/mitos/templates/marketing.yaml deploy/charts/mitos/values.yaml
git commit -s -m "feat(deploy): Cilium gateway, front-door, and marketing for single-origin mitos.run (cluster-verified)"
```

---

## Self-Review

**1. Spec coverage (WS2 Component 1, the edge):** single-origin routing by auth+slug via a Go front-door reverse proxy (Tasks 2, 3) fronted by Cilium Gateway (Task 5), session resolution (Task 1), the marketing-vs-app fork at `/` (Task 2), forge-proof identity headers (Task 2), reserved names (Task 2), Dex federation (Task 4). Marketing-static-image-from-the-website-repo is flagged as an integration follow-up (Task 5 Step 3). The verify-sets-session and the marketing `SIGNUP_BASE` flip are in the onboarding-wiring slice, not here.

**2. Placeholder scan:** The Go tasks (1-3) have complete behavior specs, exact endpoints, signatures, and test cases mirroring named existing files. The manifest tasks (4-5) specify the resources, the values blocks, the paperclip patterns to copy, and the lint commands; they are explicitly CLUSTER-VERIFIED (the live OAuth/gateway/DNS/TLS cannot be verified locally), which is stated, not hidden.

**3. Consistency:** `MITOS_IDENTITY_RESOLVE_TOKEN` is the shared bearer across Task 1 (endpoint), Task 3 (resolver), and Task 5 (frontdoor env). `Identity{AccountID,OrgID}` and `SessionResolver` (Task 2) are implemented by `HTTPSessionResolver` (Task 3). The `mitos_session` cookie name matches `console.SessionCookieName`. The reserved-names set is shared between `Decide` and the slug logic.

Note for the executor: Tasks 1-3 are locally TDD-verifiable (Go + httptest). Tasks 4-5 are deploy manifests verified by `helm template` lint locally and by the actual Cilium cluster (via the paperclip deploy) for the live flow. Do not claim the live OAuth/gateway/TLS works from a local lint; report exactly what was verified.
