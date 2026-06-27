# Design: deepen Agent Sandbox API conformance, migrate to v1beta1, prove predicate-level on KVM (#357)

Date: 2026-06-27
Issue: #357 (Deepen Agent Sandbox API conformance and upstream positioning)
Builds on: #19 (facade foundation), #18 (husk pods, CLOSED), #323 (integrations epic)

## Goal

Close #357 honestly by satisfying all three of its "Done when" criteria:

1. The facade tracks the CURRENT upstream version. Upstream `sigs.k8s.io/agent-sandbox`
   is now `v0.5.0`, which graduated the API from `v1alpha1` to `v1beta1`. We migrate
   the facade to `v1beta1`.
2. The conformance matrix is predicate-level green on bare-metal KVM. We add a permanent
   CI job that boots a real Firecracker VMM inside a husk pod and asserts the UPSTREAM
   Sandbox reaches the in-VM Ready predicate, driven through the facade with the upstream
   manifest applied unchanged.
3. mitos is documented as a conformant fork-native backend. The conformance doc and matrix
   are rewritten to the new API and the new proven status.

Two items the conformance doc lists as OPEN stay explicit, scoped FOLLOW-UP issues so the
close is honest, not silent:

- the latest-two-minors CI matrix (we pin the single current minor v0.5.0);
- running upstream's own `test/e2e` Go suite green end to end (needs their in-tree
  controller running alongside ours).

ChromeReady (the Chrome-CDP-specific in-VM predicate) stays honestly `NEEDS-BARE-METAL`
unless we run that exact workload; out of scope here. We prove the GENERIC Ready predicate
("Pod is Ready; Service Exists" analog) plus exec-through-the-bridged-sandbox.

## Upstream truth (v0.5.0, breaking changes that touch the facade)

Confirmed by reading v0.5.0 sources:

- API graduation `v1alpha1` -> `v1beta1` for both `agents.x-k8s.io` and
  `extensions.agents.x-k8s.io`. `v1alpha1` packages still ship (deprecated, kept alive by
  a conversion webhook), but the stable surface is `v1beta1`.
- `Sandbox.spec.replicas` (0 or 1) REMOVED, replaced by
  `Sandbox.spec.operatingMode` (enum `Running` | `Suspended`, default `Running`). The
  status gains a `Suspended` condition.
- `SandboxClaim.spec.templateRef` + the `warmpool` policy field (`none`/`default`/`named`)
  REMOVED, replaced by `SandboxClaim.spec.warmPoolRef` (required reference to a
  `SandboxWarmPool` by name). Cold-start is expressed by referencing a warm pool with
  `replicas: 0`. `SandboxClaim.spec.env` and `spec.volumeClaimTemplates` force a cold start.
- `SandboxWarmPool.spec` keeps `replicas` + `sandboxTemplateRef`. `SandboxTemplate` is
  largely unchanged in the fields we map.
- `Lifecycle.shutdownPolicy` enum narrows to `Delete` | `Retain` (the v0.4.6
  `DeleteForeground` is gone on the core Sandbox; the claim keeps its own lifecycle).

The facade imports the upstream Go types directly (`internal/facade/*` imports
`sigs.k8s.io/agent-sandbox/api/v1alpha1` and `.../extensions/api/v1alpha1`), so the
migration is an import + field-shape change, not a hand-rolled-type rewrite.

## Workstream A: migrate the facade to v0.5.0 / v1beta1

### A0. Dependency + vendor + toolchain gate (do first)

- `go get sigs.k8s.io/agent-sandbox@v0.5.0`; reconcile the `go` directive if the module
  bumps it (v0.5.0 declares `go 1.26`; we are already on `go 1.26` per the facade ADR).
- Re-vendor `third_party/agent-sandbox/` VERBATIM from the v0.5.0 module cache: `crds/`,
  `examples/`, `extensions/`, `test/`, `README.md` (version bumped to v0.5.0), LICENSE.
  Do NOT edit their manifests (apply-unchanged is the point).
- GATE: `go build ./...`, `make test-unit`, and BOTH lint invocations
  (`golangci-lint run --timeout=5m` AND `GOOS=linux golangci-lint run --timeout=5m`) must
  pass on the bare bump before touching reconcilers. If the toolchain blocks (the ADR's
  documented go-version / golangci-lint friction), fall back to hand-defined
  `internal/facade/apis/v1beta1` types derived from the vendored CRDs, recorded in the ADR.

### A1. Reconcilers (TDD: failing test first per behavior)

- Imports `api/v1alpha1` -> `api/v1beta1`, `extensions/api/v1alpha1` -> `extensions/api/v1beta1`
  across `internal/facade/*` and the scheme registration in `suite_test.go`.
- `reconciler.go` (core Sandbox):
  - the pause/resume mapping changes from `spec.replicas` 0<->1 to
    `spec.operatingMode` `Suspended`<->`Running`. `Suspended` RELEASES the bridged husk
    sandbox to the warm pool (clears serving observables); `Running` after `Suspended`
    RE-ACTIVATES it (the fast husk path).
  - mirror the upstream `Suspended` status condition honestly alongside Ready.
- `claim_reconciler.go` (SandboxClaim):
  - `spec.warmPoolRef.name` -> bind from OUR pool of that name (the pool our
    `warmpool_reconciler.go` created under the same name; the bridge). The
    `none`/`default`/`named` policy exception COLLAPSES and is deleted.
  - new documented exception: `spec.env` and `spec.volumeClaimTemplates` force a cold start
    upstream; record how our engine treats them (env maps through; volumeClaimTemplates
    remain the unmapped storage exception). `additionalPodMetadata` propagation preserved.
  - lifecycle (`shutdownTime`, `ttlSecondsAfterFinished`, `shutdownPolicy`) mapping preserved.
- `template_reconciler.go` / `warmpool_reconciler.go`: re-point types; `sandboxTemplateRef`
  and `replicas` field names re-verified against v1beta1.

### A2. Object-level CI (`facade-conformance` kind job)

- Install the v1beta1 CRDs from the re-vendored tree (keep v1alpha1 CRD only if a test still
  needs it; default to v1beta1-only since examples are v1beta1).
- Assertions (e)/(f): the replicas 1->0->1 toggle becomes operatingMode
  Running->Suspended->Running. Assertions (a)-(d), (g)-(j) re-pointed to v1beta1 kinds.
- The upstream example manifests vendored at v0.5.0 are now v1beta1 and apply unchanged.

### A3. Docs + bench + threat model

- `docs/facade-conformance.md`: pinned version v0.5.0; rewrite exception 2 (operatingMode)
  and exception 5 (warmPoolRef, drop the none/default/named text, add the cold-start note);
  re-point every matrix row to the v1beta1 test/example; update "What is PROVEN" / "What is
  OPEN".
- `docs/adr/0001-facade-and-naming.md`: record the v1beta1 migration + the toolchain path taken.
- `bench/facade/` + `BENCHMARKS.md`: replicas-toggle language -> operatingMode.
- `docs/threat-model.md`: a delta row, since the facade API surface moved (new API version,
  dropped policy field, new operatingMode/warmPoolRef inputs). Operating-principle #2.

## Workstream B: permanent predicate-level KVM conformance job

### Grounding

`test/cluster-e2e/husk-e2e.sh` ALREADY proves the in-VM Ready predicate on kind + `/dev/kvm`
(GitHub-hosted `ubuntu-latest` has `/dev/kvm`; `kind-e2e-husk` boots real dormant VMMs in
husk pods there): stage 1 warms dormant husk pods, stage 2 activates a claim to Ready and
`sb.exec("echo mitos-e2e-ok")` returns expected stdout over the pod network. That is the
in-VM predicate, proven through OUR API. What is missing is the SAME proof driven through
the UPSTREAM facade with the upstream manifest applied unchanged.

### B1. Script + job

- New `test/cluster-e2e/facade-conformance-kvm.sh`: compose the `husk-e2e.sh` warm-pool
  setup (real VMMs on KVM) with the `facade-conformance` facade deploy, then:
  1. apply the UPSTREAM `agents.x-k8s.io/v1beta1` Sandbox manifest UNCHANGED;
  2. assert the facade bridges a husk-backed sandbox AND the bridged sandbox BOOTS its VMM,
     so the UPSTREAM Sandbox status reaches `Ready=True` (the "Pod is Ready; Service Exists"
     analog), within the husk activation budget;
  3. assert exec succeeds THROUGH the bridged sandbox (in-VM liveness);
  4. assert operatingMode `Running`->`Suspended`->`Running` re-activates to Ready (the in-VM
     resume tail).
  SETUP-vs-CONFORMANCE failure distinction + a diagnostics trap, mirroring the existing jobs.
- New CI job `facade-conformance-kvm` in `.github/workflows/ci.yaml` (or `kvm-test.yaml`,
  whichever already targets `/dev/kvm`), building facade + controller + forkd + husk-stub +
  kvm-device-plugin images, same as `kind-e2e-husk` plus the facade.

### B2. Matrix flip

- Introduce a `PROVEN-ON-KVM` status. Flip the rows the job covers:
  - `basic_test.go :: TestSimpleSandbox` Ready predicate -> PROVEN-ON-KVM.
  - the operatingMode in-VM resume tail (was the replicas in-VM tail) -> PROVEN-ON-KVM.
- ChromeReady / python-runtime / chrome-claim rows stay `NEEDS-BARE-METAL` (workload-specific),
  honestly noted.

### B3. Required-check + docs

- Adding the job to required checks is a repo-settings change (cannot be set from code);
  call it out explicitly in the PR description for the human merger, do NOT assume it silently.
- `CLAUDE.md` CI Pipeline section: the "eight required checks" line becomes nine when the
  setting is flipped; update the prose to list the new job.

## Sequencing, isolation, risk

- A before B: B applies the post-migration v1beta1 examples.
- `internal/facade`, `internal/daemon`, `internal/firecracker`, `guest/agent-rs` are on the
  named-human-reviewer list; this work touches the facade and the husk run path. Threat-model
  delta lands in the same PR; flag for human review before merge.
- Toolchain risk (go 1.26 / golangci-lint) is gated at A0 with a documented fallback.
- The whole effort is large; it is ONE plan but will land as a small sequence of commits /
  possibly more than one PR if review wants A and B split at merge time. The spec stays one
  document.

## Definition of done (so #357 can be closed)

- `go.mod` pins `agent-sandbox v0.5.0`; `third_party/agent-sandbox` vendored at v0.5.0;
  the facade compiles and tests green against `v1beta1`.
- `facade-conformance` (kind, object-level) green on the v1beta1 shapes.
- `facade-conformance-kvm` green: the upstream Sandbox applied unchanged reaches the in-VM
  Ready predicate through the facade on a real VMM, plus exec and operatingMode resume.
- `docs/facade-conformance.md` matrix has zero `NEEDS-BARE-METAL` rows for the predicates the
  KVM job covers; the version pin is v0.5.0; exceptions 2 and 5 reflect operatingMode/warmPoolRef.
- `docs/threat-model.md` updated; ADR updated; bench/BENCHMARKS updated.
- Two follow-up issues filed: (1) latest-two-minors CI matrix; (2) upstream Go e2e green.
- #357 closed with a comment linking the green jobs and the updated matrix.
