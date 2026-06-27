# Agent Sandbox v1beta1 Migration + Predicate-Level KVM Conformance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Migrate the `agents.x-k8s.io` conformance facade to upstream v0.5.0 / `v1beta1`, and add a permanent CI job that proves predicate-level (in-VM Ready) conformance on a real Firecracker VMM, so issue #357 can be closed honestly.

**Architecture:** The facade (`cmd/facade` + `internal/facade`) reconciles upstream Sandbox/SandboxTemplate/SandboxWarmPool/SandboxClaim objects onto our SandboxPool/Sandbox/Workspace run path. We re-vendor upstream at v0.5.0, repoint the Go imports from `v1alpha1` to `v1beta1`, and adapt two breaking field changes (`spec.replicas` -> `spec.operatingMode`; SandboxClaim `templateRef`+policy -> `warmPoolRef`). Then we add a KVM e2e job that composes the existing husk warm-pool boot (real VMM on `/dev/kvm`) with the facade, applies the upstream Sandbox unchanged, and asserts the upstream Ready predicate plus exec and operatingMode resume.

**Tech Stack:** Go 1.26, controller-runtime, Firecracker, kind, GitHub Actions (`ubuntu-latest` with `/dev/kvm`), `sigs.k8s.io/agent-sandbox@v0.5.0`, Python SDK (e2e driver).

## Global Constraints

- No em (U+2014) or en (U+2013) dashes anywhere. ASCII hyphen-minus only.
- Conventional commits; every commit carries `Signed-off-by` (use `git commit -s`). Branch is `feat/agent-sandbox-v1beta1-conformance-357`.
- Stage explicit paths only; never `git add -A`.
- TDD: failing test first, behavior change lands with its test in the same commit.
- Both lint invocations are required and must pass: `golangci-lint run --timeout=5m` AND `GOOS=linux golangci-lint run --timeout=5m`.
- Secret values never logged. Errors carry actionable remediation text.
- `internal/facade` is a security-sensitive path: the threat-model delta lands in the same PR and a named human reviews before merge.
- Vendored upstream files under `third_party/agent-sandbox/` are NEVER edited (apply-unchanged is the conformance point).
- Every public number stays reproducible from `bench/` or carries an issue reference.

---

### Task 1: Bump dependency to v0.5.0, re-vendor, toolchain gate

**Files:**
- Modify: `go.mod`, `go.sum`
- Replace (verbatim from module cache): `third_party/agent-sandbox/{crds,examples,extensions,test,README.md}`

**Interfaces:**
- Produces: the module `sigs.k8s.io/agent-sandbox@v0.5.0` resolvable; `api/v1beta1` and `extensions/api/v1beta1` Go packages importable; vendored v1beta1 CRDs + examples present under `third_party/agent-sandbox/`.

- [ ] **Step 1: Bump the module**

Run:
```bash
go get sigs.k8s.io/agent-sandbox@v0.5.0
go mod tidy
```
If `go mod tidy` reports a `go` directive bump required by the module, set it with `go mod edit -go=<version>` to match (the module declares `go 1.26`; we are on 1.26 already). Record the exact version in the commit body.

- [ ] **Step 2: Re-vendor the upstream tree verbatim**

Run (copies from the resolved module cache; never hand-edit the copies):
```bash
SRC="$(go env GOMODCACHE)/sigs.k8s.io/agent-sandbox@v0.5.0"
rm -rf third_party/agent-sandbox/{crds,examples,extensions,test}
cp -R "$SRC/config/crd" third_party/agent-sandbox/crds 2>/dev/null || cp -R "$SRC/crds" third_party/agent-sandbox/crds
cp -R "$SRC/examples" third_party/agent-sandbox/examples
cp -R "$SRC/extensions" third_party/agent-sandbox/extensions
cp -R "$SRC/test" third_party/agent-sandbox/test
```
Then edit ONLY `third_party/agent-sandbox/README.md` to state `Version: v0.5.0 (pinned)`. Confirm the CRD YAMLs now contain `v1beta1` (`grep -rl v1beta1 third_party/agent-sandbox/crds`).

- [ ] **Step 3: Toolchain gate (the hard gate before any reconciler change)**

Run all of:
```bash
go build ./...
make test-unit
golangci-lint run --timeout=5m
GOOS=linux golangci-lint run --timeout=5m
```
Expected: `go build ./...` and unit tests pass; lints clean. The facade tests will FAIL to compile here because they still import `v1alpha1` and use `Replicas`; that is expected and fixed in Tasks 2-5. If `go build ./...` itself fails on the toolchain (not the facade), STOP and take the ADR fallback: hand-define `internal/facade/apis/v1beta1` types from the vendored CRDs and record the decision in `docs/adr/0001-facade-and-naming.md`.

- [ ] **Step 4: Commit**

```bash
git add go.mod go.sum third_party/agent-sandbox
git commit -s -m "chore(facade): vendor sigs.k8s.io/agent-sandbox v0.5.0 (v1beta1 graduation) (#357)"
```

---

### Task 2: Repoint facade imports and scheme registration to v1beta1

**Files:**
- Modify: `internal/facade/reconciler.go`, `claim_reconciler.go`, `template_reconciler.go`, `warmpool_reconciler.go`, `suite_test.go`, `cmd/facade/main.go` (and any other file importing the upstream types)

**Interfaces:**
- Consumes: v1beta1 packages from Task 1.
- Produces: every facade file imports `agentsv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"` and `extv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"`; the runtime scheme registers the v1beta1 group-versions.

- [ ] **Step 1: Repoint imports**

In every file under `internal/facade/` and `cmd/facade/` that imports the upstream types, change:
`sigs.k8s.io/agent-sandbox/api/v1alpha1` -> `sigs.k8s.io/agent-sandbox/api/v1beta1` (alias `agentsv1beta1`), and
`sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1` -> `sigs.k8s.io/agent-sandbox/extensions/api/v1beta1` (alias `extv1beta1`).
Find them: `grep -rln "agent-sandbox/api/v1alpha1\|agent-sandbox/extensions/api/v1alpha1" internal/facade cmd/facade`.

- [ ] **Step 2: Repoint scheme registration**

In `suite_test.go` and wherever `AddToScheme` is called (likely `cmd/facade/main.go` and `reconciler.go` setup), use `agentsv1beta1.AddToScheme` and `extv1beta1.AddToScheme`. Run `grep -rn "AddToScheme\|SchemeBuilder" internal/facade cmd/facade` to find them.

- [ ] **Step 3: Build (will still fail on field names)**

Run: `go build ./internal/facade/... ./cmd/facade/...`
Expected: failures now reference `Replicas`, `TemplateRef`, `WarmPool` policy fields (the field-shape changes), NOT the import paths. This confirms imports resolved. Field fixes are Tasks 3-5.

- [ ] **Step 4: Commit**

```bash
git add internal/facade cmd/facade
git commit -s -m "refactor(facade): repoint upstream imports + scheme to agents.x-k8s.io v1beta1 (#357)"
```

---

### Task 3: Core Sandbox reconciler: replicas -> operatingMode

**Files:**
- Modify: `internal/facade/reconciler.go`
- Test: `internal/facade/reconciler_test.go`

**Interfaces:**
- Consumes: `agentsv1beta1.Sandbox` with `Spec.OperatingMode` (`SandboxOperatingModeRunning` | `SandboxOperatingModeSuspended`, default Running) and the `Suspended` status condition constants (`agentsv1beta1.SandboxConditionSuspended`).
- Produces: the pause/resume mapping keyed on `OperatingMode`. `Suspended` releases the bridged husk sandbox + clears serving observables; `Running` (re)activates it. Status mirrors a `Suspended` condition honestly alongside Ready.

- [ ] **Step 1: Rewrite the failing tests for operatingMode**

In `reconciler_test.go`, replace every `Spec.Replicas` 0/1 assertion with `Spec.OperatingMode`. The pause test sets `OperatingMode = agentsv1beta1.SandboxOperatingModeSuspended` and asserts the bridged sandbox is released and `serviceFQDN`/`podIPs` cleared; the resume test sets it back to `SandboxOperatingModeRunning` and asserts re-activation. The idempotency test toggles Running->Suspended->Running->Suspended. Read the current test bodies first (`reconciler_test.go`) and translate each replicas value: replicas 1 == Running, replicas 0 == Suspended.

- [ ] **Step 2: Run tests to verify they fail**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/facade/ -run TestSandbox -v`
Expected: FAIL (compile or assertion) on the new operatingMode shape.

- [ ] **Step 3: Implement the operatingMode mapping**

In `reconciler.go`, replace the `Spec.Replicas == 0` / `== 1` branches with `Spec.OperatingMode`. Treat empty as Running (the upstream default). `Suspended` runs the existing release path; `Running` runs the existing activate path. Where the code reported `Status.Replicas`, set the `Suspended` condition (`metav1.Condition{Type: string(agentsv1beta1.SandboxConditionSuspended), Status: ...}`) instead, plus the existing Ready condition. Keep clearing serving observables on suspend and re-populating on resume.

- [ ] **Step 4: Run tests to verify they pass**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/facade/ -run TestSandbox -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/facade/reconciler.go internal/facade/reconciler_test.go
git commit -s -m "feat(facade): map upstream Sandbox spec.operatingMode to husk pause/resume (#357)"
```

---

### Task 4: SandboxClaim reconciler: warmPoolRef, drop the policy exception

**Files:**
- Modify: `internal/facade/claim_reconciler.go`
- Test: `internal/facade/extension_reconciler_test.go`

**Interfaces:**
- Consumes: `extv1beta1.SandboxClaim` with `Spec.WarmPoolRef.Name` (required), `Spec.Lifecycle` (`ShutdownTime`, `TTLSecondsAfterFinished`, `ShutdownPolicy`), `Spec.Env []EnvVar`, `Spec.VolumeClaimTemplates`, `Spec.AdditionalPodMetadata`.
- Produces: a claim mapping that binds OUR `Sandbox` from OUR pool named `WarmPoolRef.Name` (the pool our warmpool reconciler created under the same name). The `none`/`default`/`named` policy resolution is deleted. `env` maps through to the guest; `volumeClaimTemplates` stays the unmapped storage exception; lifecycle + additionalPodMetadata preserved.

- [ ] **Step 1: Rewrite the failing claim tests**

In `extension_reconciler_test.go`, replace the claim cases: a claim with `Spec.WarmPoolRef.Name = "sandboxwarmpool-example"` must bind our `Sandbox` from our pool of that exact name and carry the `mitos.run/pool` bridge annotation set to it. Delete the `none`/`default`/`named` policy sub-cases and the `mitos.run/warmpool-policy` annotation assertions. Add a case: a claim with `Spec.Env` set maps the env onto our claim/run path. Read the current test first to preserve the owner-reference + status-mirror + GC assertions, only swapping the binding mechanism.

- [ ] **Step 2: Run to verify failure**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/facade/ -run TestExtension -v`
Expected: FAIL on `WarmPoolRef` / removed policy.

- [ ] **Step 3: Implement the warmPoolRef binding**

In `claim_reconciler.go`: resolve the bound pool as `claim.Spec.WarmPoolRef.Name` directly (our warmpool reconciler created our pool under that name). Delete the policy-resolution helper (the none/default/named switch) and the `mitos.run/warmpool-policy` annotation write. Keep writing `mitos.run/pool`. Map `Spec.Env` onto our claim's env; keep lifecycle (`ShutdownTime` -> `mitos.run/shutdown-time`, `TTLSecondsAfterFinished` -> our field, `ShutdownPolicy` as the documented owner-ref-cascade exception) and `AdditionalPodMetadata.Annotations` propagation.

- [ ] **Step 4: Run to verify pass**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/facade/ -run TestExtension -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/facade/claim_reconciler.go internal/facade/extension_reconciler_test.go
git commit -s -m "feat(facade): map SandboxClaim spec.warmPoolRef to our pool binding, drop the v1alpha1 policy (#357)"
```

---

### Task 5: Template + WarmPool reconcilers, examples test, full facade suite green

**Files:**
- Modify: `internal/facade/template_reconciler.go`, `internal/facade/warmpool_reconciler.go`
- Test: `internal/facade/examples_test.go`, `internal/facade/extension_reconciler_test.go`, `internal/facade/suite_test.go`

**Interfaces:**
- Consumes: `extv1beta1.SandboxWarmPool` (`Spec.Replicas`, `Spec.TemplateRef.Name` via json `sandboxTemplateRef`), `extv1beta1.SandboxTemplate`.
- Produces: the whole `internal/facade` package compiles and `go test ./internal/facade/` is green against v1beta1; every vendored v0.5.0 example applies and bridges.

- [ ] **Step 1: Repoint template/warmpool field access**

In `template_reconciler.go` and `warmpool_reconciler.go`, adjust any field names that moved in v1beta1 (verify `Spec.Replicas` and `Spec.TemplateRef` against `extv1beta1`). Run `go build ./internal/facade/...` and fix each compile error against the v1beta1 struct.

- [ ] **Step 2: Repoint the examples test to the v0.5.0 vendored tree**

`examples_test.go` walks `third_party/agent-sandbox/examples` (+ `extensions/examples`). The v0.5.0 examples are v1beta1; ensure the test decodes them as `agentsv1beta1`/`extv1beta1`. Update the expected per-example exception notes only where the v0.5.0 example set changed (re-list from the vendored dir).

- [ ] **Step 3: Run the full facade suite to verify it fails then passes**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/facade/ -v`
Iterate until PASS. Then run BOTH lints:
```bash
golangci-lint run --timeout=5m
GOOS=linux golangci-lint run --timeout=5m
```
Expected: all green.

- [ ] **Step 4: Commit**

```bash
git add internal/facade
git commit -s -m "feat(facade): complete v1beta1 migration for template/warmpool/examples; facade suite green (#357)"
```

---

### Task 6: Object-level conformance CI job to v1beta1

**Files:**
- Modify: `.github/workflows/ci.yaml` (the `facade-conformance` job)

**Interfaces:**
- Consumes: the v1beta1 CRDs + examples vendored in Task 1; the migrated facade.
- Produces: the `facade-conformance` kind job installs v1beta1 CRDs, applies the v1beta1 hello-world Sandbox unchanged, and asserts facts (a)-(j) re-pointed to v1beta1, with (e)/(f) using operatingMode.

- [ ] **Step 1: Repoint CRD install + manifests**

In the `facade-conformance` job: install the v1beta1 CRDs from `third_party/agent-sandbox/crds`. The applied example paths are the v0.5.0 ones (re-confirm the hello-world + extension example filenames in the re-vendored tree). Replace the replicas-0 / replicas 1->0->1 kubectl steps (facts (e),(f)) with `operatingMode: Suspended` and a `Suspended -> Running` patch. Update the inline comments that describe replicas semantics to operatingMode.

- [ ] **Step 2: Validate the workflow locally**

Run: `python3 -c "import yaml,sys; yaml.safe_load(open('.github/workflows/ci.yaml'))" && echo OK`
Expected: `OK`. (Full job runs in CI; it needs kind.)

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/ci.yaml
git commit -s -m "ci(facade): object-level conformance job asserts v1beta1 + operatingMode (#357)"
```

---

### Task 7: Docs, ADR, bench, BENCHMARKS, threat-model to v1beta1

**Files:**
- Modify: `docs/facade-conformance.md`, `docs/adr/0001-facade-and-naming.md`, `bench/facade/README.md`, `BENCHMARKS.md`, `docs/threat-model.md`

**Interfaces:**
- Produces: docs describing the v0.5.0 / v1beta1 facade; exceptions 2 (operatingMode) and 5 (warmPoolRef) rewritten; threat-model delta recorded.

- [ ] **Step 1: Rewrite the conformance doc body**

In `docs/facade-conformance.md`: pinned version -> `v0.5.0`; "Pinned upstream version" section updated; exception 2 (pause/resume) -> operatingMode Suspended/Running (delete the replicas language); exception 5 (warmpool policy) -> warmPoolRef binding, delete the none/default/named text, add the cold-start (`env`/`volumeClaimTemplates`) note. Re-point every matrix row's "Upstream test" / "Example manifest" reference to the v1beta1 path. (Matrix STATUS flips to PROVEN-ON-KVM happen in Task 9.)

- [ ] **Step 2: ADR + bench + BENCHMARKS**

In `docs/adr/0001-facade-and-naming.md` add a dated note: migrated to v0.5.0/v1beta1, the toolchain path taken (module import vs ADR fallback). In `bench/facade/README.md` and `BENCHMARKS.md` replace the `spec.replicas 0<->1` language with `spec.operatingMode Running<->Suspended`.

- [ ] **Step 3: Threat-model delta**

In `docs/threat-model.md` add/adjust the facade row(s): the facade now admits `agents.x-k8s.io/v1beta1` inputs (operatingMode, warmPoolRef, claim env). State that untrusted spec fields are bounded to pool binding + env mirroring (no new host surface), and that the dropped v1alpha1 policy field removed a resolution branch. Keep status current per the same-PR rule.

- [ ] **Step 4: Dash + consistency sweep, then commit**

Run: `grep -RnP "\x{2014}|\x{2013}" docs/facade-conformance.md docs/adr/0001-facade-and-naming.md bench/facade/README.md BENCHMARKS.md docs/threat-model.md` (expect no output).
```bash
git add docs/facade-conformance.md docs/adr/0001-facade-and-naming.md bench/facade/README.md BENCHMARKS.md docs/threat-model.md
git commit -s -m "docs(facade): document the v0.5.0/v1beta1 facade, exceptions, and threat-model delta (#357)"
```

---

### Task 8: Predicate-level KVM conformance script + CI job

**Files:**
- Create: `test/cluster-e2e/facade-conformance-kvm.sh`
- Modify: `.github/workflows/ci.yaml` (new `facade-conformance-kvm` job) or `.github/workflows/kvm-test.yaml`

**Interfaces:**
- Consumes: the husk warm-pool boot pattern from `test/cluster-e2e/husk-e2e.sh`; the facade deploy from the `facade-conformance` job; the migrated v1beta1 facade.
- Produces: a job that, on a `/dev/kvm` runner, warms a real-VMM husk pool, deploys the facade, applies an upstream `agents.x-k8s.io/v1beta1` Sandbox UNCHANGED, and asserts: (1) the upstream Sandbox status reaches `Ready=True` (in-VM predicate) within the husk budget; (2) exec succeeds through the bridged sandbox; (3) `operatingMode` Running->Suspended->Running re-activates to Ready.

- [ ] **Step 1: Write the e2e script**

Create `test/cluster-e2e/facade-conformance-kvm.sh` modeled on `husk-e2e.sh` (reuse its kvm-node guard, warm-pool wait, and SDK-driver pattern; read it first). Stages: (0) require a `mitos.run/kvm=true` node; (1) warm a dormant husk pool; (2) `kubectl apply` the vendored upstream hello-world Sandbox UNCHANGED and `kubectl wait --for=condition=Ready sandbox/<name> --timeout=<budget>`; (3) drive exec through the bridged sandbox via the Python SDK (mirror the husk-e2e stage 2 `sb.exec("echo mitos-e2e-ok")` against the facade-created claim); (4) `kubectl patch` operatingMode to `Suspended`, assert release, patch back to `Running`, assert Ready again. Use the existing `pass`/`fail`/`info` helpers and a diagnostics trap. SETUP failures (no kvm node, image load, rollout) are distinguished from CONFORMANCE failures.

- [ ] **Step 2: Shellcheck + executable bit**

Run: `chmod +x test/cluster-e2e/facade-conformance-kvm.sh && bash -n test/cluster-e2e/facade-conformance-kvm.sh && echo OK`
Expected: `OK`.

- [ ] **Step 3: Add the CI job**

Add a `facade-conformance-kvm` job mirroring `kind-e2e-husk` (build/load controller + forkd + husk-stub + kvm-device-plugin + facade images, kind cluster with the kvm device plugin, deploy the stack + a warm pool + the facade), then `run: ./test/cluster-e2e/facade-conformance-kvm.sh`. Gate it on the same `/dev/kvm` runner as the husk job. Transient nested-KVM setup steps may retry; the Ready / exec / resume assertions hard-fail.

- [ ] **Step 4: Validate YAML + commit**

Run: `python3 -c "import yaml; yaml.safe_load(open('.github/workflows/ci.yaml'))" && echo OK`
```bash
git add test/cluster-e2e/facade-conformance-kvm.sh .github/workflows/ci.yaml
git commit -s -m "ci(facade): predicate-level KVM conformance job (upstream Sandbox -> in-VM Ready via the facade) (#357)"
```

---

### Task 9: Flip the matrix to PROVEN-ON-KVM

**Files:**
- Modify: `docs/facade-conformance.md`

**Interfaces:**
- Consumes: the green `facade-conformance-kvm` job from Task 8.
- Produces: a `PROVEN-ON-KVM` status defined; the rows the KVM job covers flipped from `NEEDS-BARE-METAL`; "What is PROVEN" / "What is OPEN" updated.

- [ ] **Step 1: Define the new status + flip rows**

In `docs/facade-conformance.md`: add `PROVEN-ON-KVM` to the status legend (asserts the in-VM predicate on a `/dev/kvm` runner via `facade-conformance-kvm`). Flip `basic_test.go :: TestSimpleSandbox` (Ready predicate) and the operatingMode in-VM resume tail rows to `PROVEN-ON-KVM`. Leave ChromeReady / python-runtime / chrome-claim rows `NEEDS-BARE-METAL` with an honest note (workload-specific, not run here).

- [ ] **Step 2: Update PROVEN / OPEN sections**

Move the in-VM Ready predicate from "What is OPEN" to "What is PROVEN". Keep in OPEN, explicitly: the latest-two-minors matrix and the full upstream Go e2e suite (now pointing at the two follow-up issues filed in Task 10).

- [ ] **Step 3: Dash sweep + commit**

Run: `grep -RnP "\x{2014}|\x{2013}" docs/facade-conformance.md` (expect no output).
```bash
git add docs/facade-conformance.md
git commit -s -m "docs(facade): flip the conformance matrix to PROVEN-ON-KVM for the in-VM Ready predicate (#357)"
```

---

### Task 10: CLAUDE.md CI prose, follow-up issues, close-out

**Files:**
- Modify: `CLAUDE.md`

**Interfaces:**
- Produces: the CI Pipeline prose lists the new job; two follow-up issues exist; #357 ready to close.

- [ ] **Step 1: Update the CI Pipeline prose**

In `CLAUDE.md`, the CI Pipeline section: add `facade-conformance-kvm` to the job list and note that when the repo-settings required-check is flipped, the required-check count becomes nine. Do NOT claim the required-check setting is already applied (it is a repo-settings change, called out for the human merger in the PR description).

- [ ] **Step 2: Commit**

```bash
git add CLAUDE.md
git commit -s -m "docs: list the facade-conformance-kvm job in the CI pipeline (#357)"
```

- [ ] **Step 3: File the two honest follow-up issues**

Run (after PR review, with `gh`):
```bash
gh issue create --repo mitos-run/mitos --title "Facade conformance: pin the latest two upstream minors (CI matrix)" --label integrations,api --body "Follow-up to #357. Vendor both current minors of sigs.k8s.io/agent-sandbox and add a CI matrix dimension so the facade conformance runs against both. Today only v0.5.0 is wired."
gh issue create --repo mitos-run/mitos --title "Facade conformance: run upstream test/e2e Go suite green end to end" --label integrations,api --body "Follow-up to #357. Stand up the upstream in-tree controller alongside ours and run their test/e2e suite green (needs their Pod/Service + running-sandbox tail)."
```

- [ ] **Step 4: Open the PR**

Push the branch and open a PR whose description: lists the v1beta1 migration + the KVM job, NAMES a human reviewer for the security-sensitive `internal/facade` path, and explicitly asks the merger to add `facade-conformance-kvm` to the required checks after it goes green. Do NOT close #357 until the PR merges and both new jobs are green on main; then close with a comment linking the green jobs + the updated matrix + the two follow-ups.

---

## Self-Review

**Spec coverage:** A0 vendor/gate -> Task 1. v1beta1 imports -> Task 2. operatingMode -> Task 3. warmPoolRef -> Task 4. template/warmpool/examples -> Task 5. object-level CI -> Task 6. docs/ADR/bench/threat-model -> Task 7. KVM job -> Task 8. matrix flip -> Task 9. CLAUDE.md + follow-ups + close -> Task 10. All three "Done when" criteria covered; both OPEN follow-ups filed not silently dropped.

**Placeholder scan:** no TBD/TODO; each code-touching step names the exact field/import change. Where exact existing line content is needed (reconciler bodies, test bodies), the step instructs reading the specific file first, because the migration adapts existing code whose verbatim content the implementer must see in-repo.

**Type consistency:** `OperatingMode` / `SandboxOperatingModeRunning` / `SandboxOperatingModeSuspended` / `SandboxConditionSuspended` (Task 3) and `WarmPoolRef.Name` / `Spec.Env` / `Lifecycle` (Task 4) match the v1beta1 shapes confirmed from upstream sources. Aliases `agentsv1beta1` / `extv1beta1` used consistently from Task 2 on.
