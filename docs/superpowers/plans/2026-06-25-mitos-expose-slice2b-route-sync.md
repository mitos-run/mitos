# Mitos Expose Slice 2b: controller route-sync reconciler

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** A controller reconciler that watches Sandboxes, and whenever the Ready-and-exposed set changes, builds the full expose route set (label, owning forkd node endpoint, sandbox id, guest port, per-sandbox bearer, sharing tier) and POSTs it to the slice-2a proxy admin endpoint `POST /internal/routes`, so a single-label subdomain resolves to a live sandbox end to end.

**Architecture:** A minimal `Sandbox.spec.expose` field declares the guest port, the subdomain label, and the sharing tier. A new `ExposeRouteReconciler` in `internal/controller` lists Ready sandboxes carrying that field, reads each one's `<name>-sandbox-token` Secret, builds the route set, and pushes it (full-set replace, matching the proxy's `RouteTable.Sync` semantics) over an authenticated HTTP poster modeled on the existing `eventfeed.WebhookSink`. The controller is wired with `--expose-proxy-admin-url` (empty disables) and `EXPOSE_PROXY_ADMIN_TOKEN` (env, never argv, never logged).

**Tech Stack:** Go, controller-runtime, api/v1 CRDs, envtest, net/http.

## Global Constraints
- Go 1.24+, module `mitos.run/mitos`.
- No em/en dashes anywhere including comments, docs, commit messages; only `.` `,` `;` `:` and ASCII hyphen.
- The per-sandbox bearer and the admin token are bearer credentials: never logged, never in argv, never in an error body. The admin token is sourced from `os.Getenv("EXPOSE_PROXY_ADMIN_TOKEN")` (never a flag).
- Error wrapping `fmt.Errorf("context: %w", err)`; octal `0o644`.
- `make generate manifests` must be run after any api/ change; CI enforces deepcopy + CRD YAML are current.
- TDD; DCO sign-off (`git commit -s`); explicit-path staging; conventional commits.
- Lint clean both `golangci-lint run --timeout=5m` and `GOOS=linux ...` for touched packages.
- envtest needs setup-envtest assets: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/`.
- `api/` and `internal/controller` touches are reviewed; the reconciler must fail safe (a missing Secret or unreachable proxy must not crash the controller, only requeue).
- CRITICAL contract: the proxy admin endpoint unmarshals `{"routes":[preview.ClaimState]}` where `ClaimState` has NO json tags, so its JSON keys are the exact exported field names `Label, SandboxID, NodeEndpoint, Port, Token, Sharing, Ready`. The controller's `ExposeRoute` DTO MUST use those identical field names with NO json tags so the posted JSON is byte-compatible.

---

### Task 1: minimal Sandbox.spec.expose CRD field

Add `Expose *SandboxExpose` to `SandboxSpec` with `Port`, `Label`, `Sharing`. Regenerate deepcopy and CRD YAML.

**Files:**
- Modify: `api/v1/sandbox_types.go`
- Regenerate: `api/v1/zz_generated.deepcopy.go`, `deploy/crds/mitos.run_sandboxes.yaml` (via make)
- Test: `api/v1/sandbox_types_test.go`

**Interfaces:**
- Produces: `type SandboxExpose struct { Port int32; Label string; Sharing string }` with `Expose *SandboxExpose` on `SandboxSpec`.

- [ ] Step 1: Add the field to `SandboxSpec` (place near `Network`):
```go
	// Expose declares that a guest port should be reachable through the Mitos
	// Expose edge proxy at a per-sandbox subdomain. Optional; absent means the
	// sandbox is not exposed.
	// +optional
	Expose *SandboxExpose `json:"expose,omitempty"`
```
And define the type near the other spec sub-types:
```go
// SandboxExpose configures the per-sandbox expose route (Mitos Expose slice 2b).
type SandboxExpose struct {
	// Port is the guest TCP port to expose.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
	// Label is the single subdomain label the route is served at (for example
	// "openclaw" in openclaw.<expose-domain>). Must be a single DNS label.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	Label string `json:"label"`
	// Sharing is the access tier. Slice 2b carries the value through to the
	// proxy as an opaque string (the proxy enforces "link" today; the full
	// ladder is slice 4). Defaults to private.
	// +kubebuilder:validation:Enum=private;link;org;authenticated;public
	// +kubebuilder:default=private
	// +optional
	Sharing string `json:"sharing,omitempty"`
}
```
- [ ] Step 2: Run `make generate manifests`. Confirm `api/v1/zz_generated.deepcopy.go` gains `SandboxExpose` DeepCopy and `deploy/crds/mitos.run_sandboxes.yaml` gains the `expose` schema. `go build ./api/...`.
- [ ] Step 3: Write `TestSandboxExposeDeepCopy`: construct a `Sandbox` with `Spec.Expose=&SandboxExpose{Port:8080,Label:"openclaw",Sharing:"private"}`, `DeepCopy()`, assert deep equality and that mutating the copy does not affect the original.
- [ ] Step 4: `go test ./api/...`; lint both.
- [ ] Step 5: Commit `api/v1/sandbox_types.go api/v1/zz_generated.deepcopy.go deploy/crds/mitos.run_sandboxes.yaml api/v1/sandbox_types_test.go` with `feat(api): add Sandbox.spec.expose (port, label, sharing) for the expose route`.

---

### Task 2: the pure route-set builder

A pure function maps Sandboxes (+ their token values) to the route DTO set, independent of Kubernetes.

**Files:**
- Create: `internal/controller/expose_routes.go`
- Test: `internal/controller/expose_routes_test.go`

**Interfaces:**
- Produces: `type ExposeRoute struct { Label string; SandboxID string; NodeEndpoint string; Port int; Token string; Sharing string; Ready bool }` (NO json tags, so JSON keys match `preview.ClaimState` exactly).
- Produces: `func BuildExposeRoutes(sandboxes []v1.Sandbox, tokenFor func(sb v1.Sandbox) (string, bool)) []ExposeRoute`. Includes only sandboxes that are Ready (`Status.Phase==SandboxReady`), have `Spec.Expose != nil`, a non-empty `Status.Endpoint`, and a resolvable token; everything else is skipped. `Ready` is always true for included routes (the proxy keeps only Ready routes). An empty `Sharing` defaults to `"private"`.

- [ ] Step 1: Failing test `TestBuildExposeRoutes`: given one Ready+Expose+Endpoint sandbox with a token, one Ready+Expose sandbox whose token is missing (skipped), one not-Ready sandbox (skipped), and one Ready sandbox with no Expose (skipped), assert exactly one route with the expected fields (Label from Spec.Expose.Label, NodeEndpoint from Status.Endpoint, SandboxID from Status.SandboxID, Port from Spec.Expose.Port as int, Sharing from Spec.Expose.Sharing, Ready true). Add a case asserting empty Sharing defaults to "private".
- [ ] Step 2: Run, confirm fail (`undefined: BuildExposeRoutes`).
- [ ] Step 3: Implement per the interface.
- [ ] Step 4: Run, confirm pass; lint both.
- [ ] Step 5: Commit `internal/controller/expose_routes.go internal/controller/expose_routes_test.go` with `feat(controller): pure builder mapping Ready exposed sandboxes to route DTOs`.

---

### Task 3: the authenticated route poster

An HTTP client posts the full route set to the proxy admin endpoint with the admin bearer, retrying on transport/5xx, modeled on `eventfeed.WebhookSink`. The token is never logged. Before implementing, open `internal/preview/admin.go` and `internal/preview/route.go` and confirm the posted JSON shape `{"routes":[{Label,SandboxID,NodeEndpoint,Port,Token,Sharing,Ready}]}` matches `ExposeRoute` field-for-field.

**Files:**
- Create: `internal/controller/expose_poster.go`
- Test: `internal/controller/expose_poster_test.go`

**Interfaces:**
- Produces: `type ExposePoster struct { URL string; Token string; Client *http.Client; MaxAttempts int; Backoff time.Duration }`, `func NewExposePoster(url, token string) *ExposePoster` (nil-safe: empty-URL poster is a no-op), and `func (p *ExposePoster) Sync(ctx context.Context, routes []ExposeRoute) error` POSTing `{"routes":routes}` to `URL` with `Authorization: Bearer <Token>`; 2xx success; 5xx/transport retried up to MaxAttempts with Backoff; 4xx terminal (no retry). Token never logged.

- [ ] Step 1: Failing tests: `TestExposePosterSyncPostsWithBearer` (httptest server captures Authorization + decoded `{"routes":[...]}`; assert `Authorization: Bearer admin-secret`, body round-trips, returns nil on 204), `TestExposePosterRetriesOn5xx` (503 then 204; two attempts), `TestExposePosterNoRetryOn4xx` (400 returns error, one attempt), `TestExposePosterEmptyURLNoop` (no request, nil).
- [ ] Step 2: Run, confirm fail.
- [ ] Step 3: Implement, reusing the `eventfeed/sink.go` retry shape. Token set on the header only, never logged.
- [ ] Step 4: Run, confirm pass; lint both.
- [ ] Step 5: Commit `internal/controller/expose_poster.go internal/controller/expose_poster_test.go` with `feat(controller): authenticated retrying poster for the expose proxy route-sync`.

---

### Task 4: the ExposeRouteReconciler

The reconciler lists Sandboxes, builds the route set (Task 2) with token lookup from the per-sandbox Secret, and posts it (Task 3). It fails safe.

**Files:**
- Create: `internal/controller/expose_controller.go`
- Test: `internal/controller/expose_controller_test.go` (envtest)

**Interfaces:**
- Produces: `type ExposeRouteReconciler struct { client.Client; Scheme *runtime.Scheme; Poster *ExposePoster }`, `Reconcile(ctx, req) (ctrl.Result, error)`, `SetupWithManager(mgr) error` (`For(&v1.Sandbox{})`, optional `.Named()` for test isolation).
- Consumes: `BuildExposeRoutes` (Task 2), `ExposePoster.Sync` (Task 3), the `<name>-sandbox-token` Secret (key `token`, the `<sandbox-name>` + `-sandbox-token` suffix from `token_secret.go`).

- [ ] Step 1: Failing envtest `TestExposeReconcilerSyncsReadyRoutes` (mirror `internal/controller/suite_test.go` harness; register the reconciler with a Poster pointed at an httptest server that records the last posted route set under a mutex): create a Ready Sandbox with `Spec.Expose` + `Status.Endpoint` + `Status.SandboxID`, create its `<name>-sandbox-token` Secret with key `token`, wait for the fake proxy to receive exactly one route with the expected label/endpoint/port/token. Then set the sandbox not-Ready (or delete it) and assert the next posted set no longer contains it. Use `Eventually`-style polling (the existing suite uses a 1s cache-sync sleep; poll the recorder with a timeout).
- [ ] Step 2: Run with envtest, confirm fail (`undefined: ExposeRouteReconciler`).
- [ ] Step 3: Implement Reconcile: on any Sandbox event, `List` all Sandboxes, resolve each token via `r.Get` on `<name>-sandbox-token` (a Ready+Expose sandbox missing its Secret returns `ctrl.Result{RequeueAfter: time.Second}` rather than an error), call `BuildExposeRoutes`, then `Poster.Sync`. A `Poster.Sync` error returns `ctrl.Result{}, err` (requeue with backoff). `SetupWithManager` does `For(&v1.Sandbox{})`. Token values never logged (log counts, labels, sandbox ids only).
- [ ] Step 4: Run envtest, confirm pass; lint both.
- [ ] Step 5: Commit `internal/controller/expose_controller.go internal/controller/expose_controller_test.go` with `feat(controller): ExposeRouteReconciler pushes the Ready route set to the proxy`.

---

### Task 5: controller wiring (flag + env + registration)

Wire the reconciler behind `--expose-proxy-admin-url` (empty disables) and `EXPOSE_PROXY_ADMIN_TOKEN` (env, never argv/logged).

**Files:**
- Modify: `cmd/controller/main.go`

- [ ] Step 1: Add `flag.StringVar(&exposeProxyAdminURL, "expose-proxy-admin-url", "", "Expose proxy admin endpoint base URL for route-sync; empty disables")` and read `exposeProxyAdminToken := os.Getenv("EXPOSE_PROXY_ADMIN_TOKEN")` (with a comment: sourced from env so the token is never in the process argv, never logged). After the other reconcilers, register only when the URL is non-empty:
```go
if exposeProxyAdminURL != "" {
	er := &controller.ExposeRouteReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Poster: controller.NewExposePoster(exposeProxyAdminURL, exposeProxyAdminToken),
	}
	if err := er.SetupWithManager(mgr); err != nil {
		logger.Error(err, "unable to create controller", "controller", "ExposeRoute")
		os.Exit(1)
	}
	logger.Info("expose route-sync enabled", "proxy", exposeProxyAdminURL)
} else {
	logger.Info("expose route-sync disabled (set --expose-proxy-admin-url to enable)")
}
```
- [ ] Step 2: `go build ./cmd/controller/...`; lint both.
- [ ] Step 3: Commit `cmd/controller/main.go` with `feat(controller): wire ExposeRouteReconciler behind --expose-proxy-admin-url`.

---

### Task 6: docs and threat-model delta

Document the end-to-end route-sync path and the threat-model row.

**Files:**
- Modify: `docs/preview-urls.md`
- Modify: `docs/threat-model.md`

- [ ] Step 1: docs/preview-urls.md: describe the controller watching Ready exposed sandboxes (`Sandbox.spec.expose`) and pushing the full route set to the proxy admin endpoint; the admin token shared via a Secret/env; the fail-safe requeue behavior; that this completes the end-to-end resolution of a single-label subdomain to a live sandbox.
- [ ] Step 2: threat-model row: the route-sync control loop is controller-to-proxy, authenticated by the shared admin bearer (constant-time, never logged); the POST body carries per-sandbox bearers over the in-cluster hop in cleartext (same trust model as the :9091 hop; in-cluster TLS a recorded follow-up); fail-safe (missing Secret or unreachable proxy requeues, never crashes the controller); the proxy reaps a sandbox that leaves the Ready set. No em/en dashes.
- [ ] Step 3: dash check both docs; commit `docs/preview-urls.md docs/threat-model.md` with `docs(expose): document the controller route-sync loop and its threat-model delta`.

---

## Self-review notes
- Coverage: minimal expose CRD field (Task 1), pure mapping (Task 2), authenticated retrying poster (Task 3), the reconciler with fail-safe token lookup and reap-on-not-Ready (Task 4), controller wiring disabled-by-default (Task 5), docs + threat-model (Task 6). End to end: a single-label subdomain now resolves to a live Ready sandbox.
- Deferred: wildcard+PQ TLS (slice 3), the full sharing ladder + audience selectors + cookie exchange (slice 4), `mitos workspace serve` + SDK parity (slice 5), the harness recipe (slice 6), and the per-sandbox expose concurrency cap.
- Type consistency: `ExposeRoute` field names (Label, SandboxID, NodeEndpoint, Port, Token, Sharing, Ready), NO json tags, byte-identical to `preview.ClaimState` (Task 3 verifies). The reconciler posts the FULL set each reconcile; the proxy's Sync reaps absent labels. A poster debounce is a documented follow-up, not built here.
