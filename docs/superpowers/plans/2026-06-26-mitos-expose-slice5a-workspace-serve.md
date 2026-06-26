# Mitos Expose Slice 5a: mitos workspace serve (CLI + Go SDK + docs)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** `mitos workspace serve <ws> --pool <pool> [--port N] [--sharing private|link|org|authenticated|public] [--as <label>]` warm-claims a forked sandbox bound to the workspace, sets `spec.expose`, and prints the ready dev-environment URL. Plus the Go SDK reference `Workspace.Serve()` returning a handle with `.URL`, and the docs. Closes the CLI half of #312. (The other five SDKs are slice 5b.)

**Architecture:** Builds entirely on merged machinery: the `Sandbox.spec.expose` CRD field (slices 2b/4), the controller route-sync (2b), the edge proxy (2a/3/4). `serve` creates a Sandbox from `--pool` with `spec.workspaceRef` + `spec.expose`, waits Ready, and constructs the URL `https://<label>.<expose-domain>/` (private and identity tiers, OIDC-gated, no token) or a server-minted signed link for `--sharing link`. The URL resolution requires the expose proxy deployed and `*.<expose-domain>` DNS; the command prints the URL with an honest note about that (per #312's "honest status").

**Tech Stack:** Go, internal/agentcli, controller-runtime client, sdk/go.

## Global Constraints
- Go 1.26; module mitos.run/mitos. No em/en dashes anywhere.
- Per-sandbox bearer / preview secret never logged. The expose-domain is config, not secret.
- Label rules: a single DNS label `[a-z0-9]([a-z0-9-]*[a-z0-9])?`, max 63, not a reserved name. Default label = the sandbox id (always unique) unless `--as` is given; `--as` must be validated and is the operator's responsibility for global uniqueness (documented limitation).
- TDD; DCO sign-off; explicit-path staging. internal/agentcli/clusterbackend tests use envtest (`eval "$(~/go/bin/setup-envtest use 1.31 -p env)"`). Lint both darwin and GOOS=linux.

---

### Task 1: expose-domain config + URL construction helper (pure)

**Files:** Create `internal/agentcli/exposeurl.go` + `exposeurl_test.go`.

**Interfaces:**
- `func BuildExposeURL(label, exposeDomain string) (string, error)`: validates label (single DNS label, <=63, not reserved) and exposeDomain (non-empty), returns `https://<label>.<exposeDomain>/`. Errors on empty/invalid.
- `func DefaultExposeDomain() string`: reads `MITOS_EXPOSE_DOMAIN` env (empty if unset).
- A reserved-label check reused from the proxy semantics (www, app, api, console, admin, auth, login, ...); if importing internal/preview.IsReservedLabel is clean, reuse it; else a local copy with a comment.

- [ ] Step 1: Failing tests: `BuildExposeURL("openclaw","mitos.app")` -> `https://openclaw.mitos.app/`; empty label/domain -> error; reserved label "api" -> error; a label with a dot or uppercase -> error (or lowercased per the proxy rule, match the proxy's ParseHost expectation); `DefaultExposeDomain` reads the env.
- [ ] Steps 2-5: implement; run; lint; commit `feat(cli): expose URL construction and domain config`.

---

### Task 2: WorkspaceBackend.Serve (cluster backend, envtest)

**Files:** Modify `internal/agentcli/workspace_backend.go` (add `Serve` to the interface + a `ServeOptions` + `ServeResult`), `internal/agentcli/clusterbackend.go` (impl), the mock/other backends (stub returning a clear unsupported error if a non-cluster backend exists). Test: `internal/agentcli/clusterbackend_test.go`.

**Interfaces:**
- `type ServeOptions struct { Pool string; Port int; Sharing string; Label string }` and `type ServeResult struct { SandboxName string; Label string; URL string; Sharing string }`.
- `Serve(ctx, workspace string, exposeDomain string, opts ServeOptions) (ServeResult, error)`: create a Sandbox with `spec.source.poolRef={name: opts.Pool}`, `spec.workspaceRef={name: workspace}`, and `spec.expose={port: opts.Port (default 8080), label: opts.Label (default the generated sandbox name), sharing: opts.Sharing (default private)}`; wait Ready (reuse `waitSandboxReady`); compute the effective label (opts.Label or the sandbox name); `URL = BuildExposeURL(label, exposeDomain)`. For `sharing=="link"`, additionally mint a signed link via the existing preview path (the sandbox-server `/v1/preview` or the per-sandbox-token forkd preview mint) and append `?token=...`; if link minting is not reachable in the cluster path, return the clean URL with a documented note that link tokens require the preview mint endpoint (do NOT block the private path on it).
- Validate `opts.Pool != ""` (required), `exposeDomain != ""` (required; from `--expose-domain`/`MITOS_EXPOSE_DOMAIN`).

- [ ] Step 1: Failing envtest `TestServeCreatesExposedSandbox` (mirror the existing clusterbackend envtest rig): call Serve with a workspace, pool, port; assert a Sandbox was created with `spec.workspaceRef`, `spec.expose{Port,Label,Sharing}` set, and that once the test marks it Ready (Status update) Serve returns a ServeResult whose URL is `https://<label>.<exposeDomain>/`. Add `TestServeRequiresPoolAndDomain` (empty pool or domain -> error).
- [ ] Steps 2-5: implement; run with envtest; lint; commit `feat(cli): WorkspaceBackend.Serve claims an exposed workspace sandbox`.

---

### Task 3: the `mitos workspace serve` CLI command

**Files:** Modify `internal/agentcli/workspace_cmd.go` (the `runWs` switch + a `cmdServe`), `internal/agentcli/cli.go` (usage string if it enumerates subcommands). Test: `internal/agentcli/workspace_cmd_test.go`.

**Interfaces:** `serve <workspace> --pool P [--port N] [--sharing S] [--as L] [--expose-domain D]` (expose-domain defaults to `MITOS_EXPOSE_DOMAIN`). Parses flags, calls `backend.Serve`, prints the URL and an honest one-line note (the URL is reachable once the expose proxy is deployed and `*.<expose-domain>` DNS resolves to it).

- [ ] Step 1: Failing test (using a fake WorkspaceBackend implementing Serve): `serve myws --pool python --expose-domain mitos.app` prints `https://...mitos.app/`; missing `--pool` -> a clear usage error; missing expose-domain (env unset, no flag) -> a clear error.
- [ ] Steps 2-5: implement; run; lint; commit `feat(cli): mitos workspace serve command`.

---

### Task 4: Go SDK Workspace.Serve()

**Files:** Modify `sdk/go/cluster.go` (the `Workspace` type + a `ServedWorkspace` handle). Test: `sdk/go/cluster_test.go` (or a new test) using the SDK's existing fake/k8s test rig.

**Interfaces:**
- `func (w *Workspace) Serve(ctx context.Context, opts ...ServeOption) (*ServedWorkspace, error)` where `ServeOption` sets pool/port/sharing/label/exposeDomain (exposeDomain defaults to an AgentRun option or `MITOS_EXPOSE_DOMAIN`).
- `type ServedWorkspace struct { ... }` with an exported `URL string` field (and `SandboxName`, `Sharing`). It claims a sandbox bound to the workspace with `spec.expose` set, waits Ready, and sets `URL`.
- Reuse the Go SDK's existing sandbox-claim path (`AgentRun.Sandbox`/`Create` with `WithPool` + `WithWorkspace`), extended to set `spec.expose`.

- [ ] Step 1: Failing test with the SDK's k8s test rig: `ws.Serve(ctx, WithPool("python"), WithExposeDomain("mitos.app"))` returns a `*ServedWorkspace` whose `.URL` is `https://<label>.mitos.app/` and the created Sandbox has `spec.expose` set. (If the SDK lacks an envtest rig, assert the request the SDK would send sets spec.expose and the URL is constructed; match the SDK's existing test style.)
- [ ] Steps 2-5: implement; run `cd sdk/go && go test ./...`; lint; commit `feat(sdk/go): Workspace.Serve returns a handle with .URL`.

---

### Task 5: docs

**Files:** Create `docs/recipes/dev-environment.md`; modify `README.md` (a capability row under Durable state or Agent DX).

- [ ] Step 1: `docs/recipes/dev-environment.md`: overview (serve a durable workspace as a ready dev-environment URL), the CLI walkthrough (`ws create` then `ws serve --pool`), the Go SDK example, the honest prerequisites (expose proxy deployed, `*.<expose-domain>` DNS, OIDC for the private default), the warm-fork angle, and the deferred items (the other SDKs in 5b, link-token minting specifics, label-uniqueness limitation). Match `docs/recipes/agent-harness.md` structure. No em/en dashes.
- [ ] Step 2: README: add a row, e.g. under Durable state: `mitos workspace serve <ws>` claims a warm forked sandbox bound to the workspace and returns a ready URL; link to the recipe. Keep claims honest (no unverified numbers).
- [ ] Step 3: dash check; commit `docs(expose): dev-environment serve recipe and README row`.

---

## Self-review notes
- 5a delivers `mitos workspace serve` + the Go SDK reference + docs. 5b adds Python/TypeScript/Ruby/Rust/Java parity.
- Honest status: the URL is constructed and the CRD is set; actual reachability needs the deployed proxy + DNS + (for private) OIDC. The command/docs say so.
- Deferred: link-token minting in the cluster path if not trivially reachable (note it); the global label-uniqueness registry (documented limitation); memory-snapshot resumable heads (separate roadmap item).
- The ServeResult/ServedWorkspace `.URL` is the #312 deliverable (a handle that carries the URL).
