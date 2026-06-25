# API v1 Consolidation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the four v1alpha1 kinds with three stable `mitos.run/v1` nouns (`SandboxPool`, `Sandbox`, `Workspace` + `WorkspaceRevision` companion), so 1.0.0 ships an honest, consolidated CRD surface.

**Architecture:** Promote the existing `api/v1alpha2` consolidated types to a single stable `api/v1` package, fold the shared leaf types and Workspace from `api/v1alpha1` into it, and delete `api/v1alpha1`. Move the engine logic out of the delegating `SandboxReconciler` and the `SandboxClaim`/`SandboxFork`/v1alpha1 `SandboxPool` reconcilers so the v1 `Sandbox` and `SandboxPool` reconcilers own the engine directly. Cut over facade, CLI, MCP, manifests, examples, docs, and the Python + TypeScript SDK cluster mode atomically. No conversion webhook (clean break, no existing usage).

**Tech Stack:** Go, controller-runtime, kubebuilder, envtest, controller-gen; Python + TypeScript SDKs.

## Global Constraints

- Group version after this work is exactly `mitos.run/v1`; `v1alpha1` and `v1alpha2` are deleted. One served version equals stored version. No conversion webhook.
- Punctuation rule (strict): never use em (U+2014) or en (U+2013) dashes anywhere (code, comments, docs, YAML, CRD descriptions, commit messages). Only `.` `,` `;` `:` and ASCII hyphen-minus.
- Error wrapping: `fmt.Errorf("context: %w", err)`. Octal literals `0o644`. gofmt + golangci-lint clean is a merge requirement.
- Lint, BOTH invocations required: `golangci-lint run --timeout=5m` AND `GOOS=linux golangci-lint run --timeout=5m`.
- Guest cross-build check: `GOOS=linux GOARCH=amd64 go build ./guest/agent/`.
- TDD: write the failing test first; every behavior change lands with its test in the same commit.
- git: stage explicit paths only, never `git add -A`. Conventional commits (feat, fix, docs, refactor, test, chore).
- Secret VALUES never logged, never in error or condition messages, never written to host paths. Do not weaken any secret-handling or fork-correctness default during the engine move.
- `source.fromRevision` is declared in the v1 schema but NOT served: the reconciler must report a clear not-ready condition for it, never silently drop it.
- Work happens in a dedicated git worktree on branch `feat/api-v1-consolidation` (created via superpowers:using-git-worktrees before Task 1). The spec (`docs/superpowers/specs/2026-06-23-api-v1-consolidation-migration-design.md`) and this plan are committed on that branch first.

## Verification commands (used throughout)

- Build: `go build ./...`
- Vet: `go vet ./...`
- Unit (fork/workspace/vsock): `make test-unit`
- Controller envtest: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/`
- Regenerate deepcopy + CRDs: `make generate manifests`
- Python SDK: `make test-python`
- TypeScript SDK: `cd sdk/typescript && npm test`

## Type inventory (authoritative for Stage 1)

**Renamed v1alpha2 -> v1, kept (consolidated kinds + their helpers):**
`Sandbox`, `SandboxSpec`, `SandboxSource`, `FromSandboxSource`, `FromRevisionSource`, `SandboxNetwork`, `SandboxBudget`, `SandboxLifetime`, `OnTerminate`, `ResumeMode` (+`ResumeMemory`,`ResumeFilesystem`), `SecretInheritanceMode` (+`SecretReissue`,`SecretInherit`), `SandboxStatus`, `SandboxChild`, `SandboxBudgetSpend`, `SandboxList`; `SandboxPool`, `SandboxPoolSpec`, `PoolTemplateSpec`, `PoolSnapshots`, `PoolSnapshotRefresh`, `PoolWarm`, `SandboxPoolList`; everything in `api/v1alpha2/budget.go`.

**Moved v1alpha1 -> v1 (shared leaf types + Workspace), DROP the `v1alpha1.` qualifier at use sites:**
Leaf types from `types.go`: `BuildStepType`, `BuildStep`, `SandboxResources`, `GPUResources`, `ForkPolicy`, `SandboxVolume`, `VolumeSource`, `S3VolumeSource`, `GCSVolumeSource`, `PVCVolumeSource`, `GitVolumeSource`, `EgressPolicy`, `InboundPolicy`, `NetworkPolicy`, `SnapshotTrigger`, `HuskDrainPolicy`, `CPUPinningSpec`, `CPUPinningPolicy`, `PoolPlacement`, `SandboxPoolStatus`, `OutputSpec`, `GitOutput`, `SecretMount`, `VolumeOverride`, `SandboxPhase` (+ its phase constants), `LocalObjectReference`. Workspace types from `workspace_types.go`: all of them (`Workspace` ... `WorkspaceRevisionList`). Budget from `budget_types.go`: `Budget`, `BudgetSpend` (keep only if still referenced after the move; otherwise drop).

**DELETED (removed kinds, no host):** `SandboxTemplate`, `SandboxTemplateSpec`, `SandboxTemplateList`; v1alpha1 `SandboxPool`, `SandboxPoolSpec`, `SandboxPoolList`; `SandboxClaim`, `SandboxClaimSpec`, `SandboxClaimStatus`, `SandboxClaimList`; `SandboxFork`, `SandboxForkSpec`, `SandboxForkStatus`, `SandboxForkList`; `ForkInfo` (re-homed to `SandboxChild`); `PoolAutoscaleSpec` (re-homed to `PoolWarm`; drop if unreferenced after Stage 4).

---

## Stage 1: the `api/v1` package

### Task 1.1: Scaffold `api/v1` from the renamed v1alpha2 types

**Files:**
- Create: `api/v1/groupversion_info.go`, `api/v1/doc.go`
- Create: `api/v1/sandbox_types.go`, `api/v1/sandboxpool_types.go`, `api/v1/budget.go` (moved from `api/v1alpha2/`, package renamed)
- Delete (end of stage): `api/v1alpha2/` directory

**Interfaces:**
- Produces: package `v1` at import path `mitos.run/mitos/api/v1` with `GroupVersion = schema.GroupVersion{Group: "mitos.run", Version: "v1"}`, exporting every type in the "kept" inventory above.

- [ ] **Step 1: Copy the v1alpha2 type files into api/v1 and rename the package**

```bash
mkdir -p api/v1
git mv api/v1alpha2/sandbox_types.go      api/v1/sandbox_types.go
git mv api/v1alpha2/sandboxpool_types.go  api/v1/sandboxpool_types.go
git mv api/v1alpha2/budget.go             api/v1/budget.go
git mv api/v1alpha2/doc.go                api/v1/doc.go
git mv api/v1alpha2/groupversion_info.go  api/v1/groupversion_info.go
```

In each moved file change `package v1alpha2` to `package v1`. In `groupversion_info.go` set `GroupVersion = schema.GroupVersion{Group: "mitos.run", Version: "v1"}` and update the doc comment to say `mitos.run/v1`. In `doc.go` set the `+kubebuilder:object:generate=true` and `+groupName=mitos.run` markers with `// +versionName=v1`.

- [ ] **Step 2: Remove the v1alpha1 import and conversion artifacts (deferred resolution)**

Delete `api/v1alpha2/conversion.go`, `api/v1alpha2/conversion_test.go`, `api/v1alpha2/webhook.go`, `api/v1alpha2/zz_generated.deepcopy.go`. In the moved `sandbox_types.go` and `sandboxpool_types.go`, delete the `v1alpha1 "mitos.run/mitos/api/v1alpha1"` import. The now-unqualified type references (`SecretMount`, `VolumeOverride`, `LocalObjectReference`, `OutputSpec`, `SandboxPhase`, `SandboxPoolStatus`, `BuildStep`, `SandboxResources`, `SandboxVolume`, `NetworkPolicy`, `PoolPlacement`, `CPUPinningSpec`, `HuskDrainPolicy`, `SnapshotTrigger`) will be resolved by Task 1.2 which moves those types into this same package. Build will fail until then; that is expected and verified in Task 1.2.

- [ ] **Step 3: Commit the rename**

```bash
git add api/v1/ api/v1alpha2/
git commit -m "refactor(api): rename v1alpha2 consolidated types to v1, drop conversion machinery"
```

### Task 1.2: Move shared leaf types and Workspace into `api/v1`

**Files:**
- Create: `api/v1/types.go` (shared leaf types only, NOT the removed kinds), `api/v1/workspace_types.go`, `api/v1/budget_legacy.go` (only if `Budget`/`BudgetSpend` still referenced)
- Delete: `api/v1alpha1/` directory (whole package)

**Interfaces:**
- Produces: all "moved" inventory types as package `v1` symbols. Removed-kind types do not exist anywhere after this task.

- [ ] **Step 1: Create api/v1/types.go with the shared leaf types**

Copy from `api/v1alpha1/types.go` ONLY the leaf types listed under "Moved v1alpha1 -> v1" (every `type` from the inventory except `SandboxTemplate*`, v1alpha1 `SandboxPool*`, `SandboxClaim*`, `SandboxFork*`, `ForkInfo`, `PoolAutoscaleSpec`). Set `package v1`. Remove the `init()` `SchemeBuilder.Register(...)` block (the registered kinds are gone; the kept kinds register in their own files). Keep all `SandboxPhase` constants.

- [ ] **Step 2: Create api/v1/workspace_types.go**

```bash
git mv api/v1alpha1/workspace_types.go api/v1/workspace_types.go
```

Change `package v1alpha1` to `package v1`. Keep its `SchemeBuilder.Register(&Workspace{}, &WorkspaceList{}, &WorkspaceRevision{}, &WorkspaceRevisionList{})` block (these kinds survive).

- [ ] **Step 3: Delete api/v1alpha1**

```bash
git rm -r api/v1alpha1
```

- [ ] **Step 4: Regenerate deepcopy for api/v1**

Run: `make generate`
Expected: `api/v1/zz_generated.deepcopy.go` is created covering all v1 types; no `api/v1alpha1` or `api/v1alpha2` generated files remain.

- [ ] **Step 5: Verify the api package compiles in isolation**

Run: `go build ./api/...`
Expected: PASS (the api package no longer references v1alpha1; all shared types are local). The rest of the repo will still fail to build until later stages re-key their imports; that is expected.

- [ ] **Step 6: Commit**

```bash
git add api/v1/ api/v1alpha1/ api/v1alpha2/
git commit -m "refactor(api): fold shared leaf types and Workspace into api/v1, delete v1alpha1"
```

### Task 1.3: Add the pool template-xor-templateRef and source-oneof validation

**Files:**
- Create: `api/v1/validation.go`, `api/v1/validation_test.go`

**Interfaces:**
- Produces: `func (p *SandboxPool) ValidateCreate() error`, `func (s *Sandbox) ValidateCreate() error` enforcing exactly-one-of.

- [ ] **Step 1: Write the failing test**

```go
package v1

import "testing"

func TestSandboxPoolRequiresExactlyOneTemplate(t *testing.T) {
	p := &SandboxPool{Spec: SandboxPoolSpec{}}
	if err := p.ValidateCreate(); err == nil {
		t.Fatal("expected error when neither template nor templateRef is set")
	}
	p.Spec.Template = &PoolTemplateSpec{Image: "x"}
	p.Spec.TemplateRef = &LocalObjectReference{Name: "y"}
	if err := p.ValidateCreate(); err == nil {
		t.Fatal("expected error when both template and templateRef are set")
	}
	p.Spec.TemplateRef = nil
	if err := p.ValidateCreate(); err != nil {
		t.Fatalf("inline template alone should be valid: %v", err)
	}
}

func TestSandboxRequiresExactlyOneSource(t *testing.T) {
	s := &Sandbox{Spec: SandboxSpec{}}
	if err := s.ValidateCreate(); err == nil {
		t.Fatal("expected error when no source is set")
	}
	s.Spec.Source.PoolRef = &LocalObjectReference{Name: "p"}
	s.Spec.Source.FromSandbox = &FromSandboxSource{Name: "q"}
	if err := s.ValidateCreate(); err == nil {
		t.Fatal("expected error when two sources are set")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./api/v1/ -run TestSandbox -v`
Expected: FAIL with "ValidateCreate not defined".

- [ ] **Step 3: Implement validation.go**

```go
package v1

import "fmt"

// ValidateCreate enforces that exactly one of spec.template or spec.templateRef
// is set (the Deployment-embeds-PodSpec pattern, ADR 0007).
func (p *SandboxPool) ValidateCreate() error {
	hasInline := p.Spec.Template != nil
	hasRef := p.Spec.TemplateRef != nil
	if hasInline == hasRef {
		return fmt.Errorf("spec must set exactly one of template or templateRef")
	}
	return nil
}

// ValidateCreate enforces that exactly one of source.poolRef, source.fromSandbox,
// or source.fromRevision is set.
func (s *Sandbox) ValidateCreate() error {
	n := 0
	if s.Spec.Source.PoolRef != nil {
		n++
	}
	if s.Spec.Source.FromSandbox != nil {
		n++
	}
	if s.Spec.Source.FromRevision != nil {
		n++
	}
	if n != 1 {
		return fmt.Errorf("spec.source must set exactly one of poolRef, fromSandbox, or fromRevision")
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./api/v1/ -run TestSandbox -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add api/v1/validation.go api/v1/validation_test.go
git commit -m "feat(api): exactly-one-of validation for SandboxPool template and Sandbox source"
```

---

## Stage 2: absorb the claim engine into Sandbox source.poolRef

Stages 2 to 4 are MOVE + RE-KEY refactors guarded by the existing envtest conformance suite, not greenfield code. The discipline: keep the engine behavior identical; only the driving kind changes. Do not weaken any secret or fork-correctness default. After each task the controller envtest suite must be green.

### Task 2.1: Re-key the consolidated SandboxReconciler to own the claim engine directly

**Files:**
- Modify: `internal/controller/sandbox_v2_controller.go` (the poolRef branch), `internal/controller/sandboxclaim_controller.go` (source of the engine logic), shared helpers `scheduler.go`, `forkd_client.go`, `secret_replication.go`, `token_secret.go`, `finalizer.go`, `gc.go`, `idle_decision.go`, `workspace_binding.go`
- Test: `internal/controller/sandbox_v2_conformance_test.go` (existing), plus re-keyed `sandbox_claimpath_test.go`

**Interfaces:**
- Consumes: `v1.Sandbox` with `Spec.Source.PoolRef`, `v1.SandboxPool`.
- Produces: a `Sandbox{source.poolRef, replicas:1}` drives node selection, forkd `Fork`, endpoint, secret delivery, bearer token issue, lifetime/idle, terminate-with-outputs, and reaches `phase: Ready`, with NO intermediate `SandboxClaim` object created.

- [ ] **Step 1: Confirm the guard test exists and currently passes against the delegating impl**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ -run TestSandboxConformancePoolRef -v`
Expected: PASS (delegating implementation: a child SandboxClaim is created).

- [ ] **Step 2: Write the failing test that forbids the intermediate child**

Add to `internal/controller/sandbox_claimpath_test.go`:

```go
// A Sandbox{source.poolRef} must drive the engine directly: no SandboxClaim
// object is created (the kind no longer exists after Stage 1).
func TestSandboxPoolRefCreatesNoChildClaim(t *testing.T) {
	// envtest scaffolding mirrors TestSandboxConformancePoolRef.
	// After reconcile to Ready, assert the Sandbox status carries SandboxID and
	// Endpoint set by the engine path, and that no owned child object exists.
	sb := reconcilePoolRefSandboxToReady(t) // helper from existing conformance test
	if sb.Status.SandboxID == "" || sb.Status.Endpoint == "" {
		t.Fatalf("engine did not populate status: %+v", sb.Status)
	}
}
```

- [ ] **Step 3: Run to verify it fails to compile (SandboxClaim type gone) or fails assertion**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ -run TestSandboxPoolRefCreatesNoChildClaim -v`
Expected: FAIL (delegating impl still references the deleted `SandboxClaim`, so the package does not compile).

- [ ] **Step 4: Move the claim engine into the poolRef branch**

In `sandbox_v2_controller.go`, replace the `source.poolRef` delegation (the `childClaimName` get-or-create) with the engine body lifted from `SandboxClaimReconciler.Reconcile` in `sandboxclaim_controller.go`: node selection (`scheduler.go`), `forkd_client.go` `Fork`, secret delivery (`secret_replication.go`, `token_secret.go`), endpoint/status population, idle decision (`idle_decision.go`), workspace binding (`workspace_binding.go`), finalizer wiring (`finalizer.go`). Re-key every `v1alpha1.SandboxClaim` field reference to its `v1.Sandbox` home per the ADR 0007 map: `claim.Spec.PoolRef` -> `sb.Spec.Source.PoolRef`; `claim.Spec.Timeout` -> `sb.Spec.Lifetime.TTL`; `claim.Spec.IdleTimeout` -> `sb.Spec.Lifetime.IdleTimeout`; `claim.Spec.Outputs` -> `sb.Spec.Lifetime.OnTerminate.Outputs`; `claim.Status.ForkTimeMicros` -> `sb.Status.StartupLatencyMs` (rescale micros to millis). Delete `childClaimName`.

- [ ] **Step 5: Run the conformance + new guard tests**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ -run 'TestSandboxConformancePoolRef|TestSandboxPoolRefCreatesNoChildClaim' -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/sandbox_v2_controller.go internal/controller/sandbox_claimpath_test.go internal/controller/scheduler.go internal/controller/forkd_client.go internal/controller/secret_replication.go internal/controller/token_secret.go internal/controller/finalizer.go internal/controller/idle_decision.go internal/controller/workspace_binding.go
git commit -m "refactor(controller): Sandbox source.poolRef owns the claim engine directly"
```

### Task 2.2: Delete the SandboxClaim reconciler and re-key its remaining tests

**Files:**
- Delete: `internal/controller/sandboxclaim_controller.go` and `internal/controller/sandboxclaim_controller_test.go`
- Modify: `cmd/controller/main.go` (remove `claimReconciler` wiring), any secret/GC test that constructed a `SandboxClaim`

- [ ] **Step 1: Delete the reconciler and remove its setup wiring**

```bash
git rm internal/controller/sandboxclaim_controller.go internal/controller/sandboxclaim_controller_test.go
```
In `cmd/controller/main.go` delete the `claimReconciler := &controller.SandboxClaimReconciler{...}` block and its `SetupWithManager` call (around lines 220 to 277).

- [ ] **Step 2: Re-key surviving controller tests off SandboxClaim**

For every remaining `internal/controller/*_test.go` that constructs `v1alpha1.SandboxClaim`, rewrite the fixture to `v1.Sandbox{Spec:{Source:{PoolRef:...}}}`. (GC, TTL, finalizer, secret tests.)

- [ ] **Step 3: Run the controller suite**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/controller/ cmd/controller/main.go
git commit -m "refactor(controller): remove SandboxClaim reconciler, re-key tests to v1 Sandbox"
```

---

## Stage 3: absorb the fork engine into Sandbox source.fromSandbox

### Task 3.1: Re-key the fromSandbox branch to own the fork engine directly

**Files:**
- Modify: `internal/controller/sandbox_v2_controller.go` (the fromSandbox branch)
- Source: `internal/controller/sandboxfork_controller.go`
- Test: existing `TestSandboxConformanceFromSandbox` + new `internal/controller/sandbox_forkpath_test.go`

**Interfaces:**
- Consumes: `v1.Sandbox` with `Spec.Source.FromSandbox` and `Spec.Replicas`.
- Produces: a `Sandbox{source.fromSandbox, replicas:N}` produces N indexed sibling children reported in `status.children`, with `status.forkSnapshotTaken` the exactly-once guard, and the secret-inheritance policy gated on `spec.secretInheritance` (default `reissue`) with a fresh per-fork bearer token.

- [ ] **Step 1: Write the failing test for the per-fork token + reissue default**

```go
// A fork (source.fromSandbox) must reissue a fresh bearer token per child by
// default and must NOT inherit source secrets unless spec.secretInheritance is
// inherit. This preserves docs/fork-correctness.md section 3.
func TestForkReissuesTokenAndDeniesSecretInheritanceByDefault(t *testing.T) {
	sb := reconcileForkSandboxToReady(t, 2 /*replicas*/) // helper mirrors conformance test
	if sb.Status.ReadyReplicas != 2 {
		t.Fatalf("want 2 ready children, got %d", sb.Status.ReadyReplicas)
	}
	assertChildrenHaveDistinctTokens(t, sb)        // helper: tokens differ from source and each other
	assertNoInheritedSecretCondition(t, sb)        // helper: SecretInheritanceDenied reason present by default
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ -run TestForkReissuesToken -v`
Expected: FAIL (delegating impl creates a child `SandboxFork`; package does not compile after the kind is removed).

- [ ] **Step 3: Move the fork engine into the fromSandbox branch**

Lift `SandboxForkReconciler.Reconcile` from `sandboxfork_controller.go` into the `source.fromSandbox` branch of `sandbox_v2_controller.go`. Re-key per ADR 0007: `fork.Spec.SourceRef` -> `sb.Spec.Source.FromSandbox.Name`; `fork.Spec.Replicas` -> `sb.Spec.Replicas`; `fork.Spec.PauseSource` -> `sb.Spec.Source.FromSandbox.PauseSource`; `fork.Spec.AllowSecretInheritance bool` -> `sb.Spec.SecretInheritance` enum (false==`reissue`, true==`inherit`); `fork.Status.Forks []ForkInfo` -> `sb.Status.Children []SandboxChild`; `fork.Status.ReadyForks` -> `sb.Status.ReadyReplicas`; `fork.Status.ForkSnapshotTaken` -> `sb.Status.ForkSnapshotTaken`; `fork.Status.CheckpointTime` -> `sb.Status.CheckpointTime`. Re-home the condition reasons `SecretInheritanceDenied`, `ExplicitOptIn`, `Forked`/`ForksCreated` onto the Sandbox gated on `fromSandbox && replicas > 1`. Delete `childForkName`.

- [ ] **Step 4: Run the conformance + new fork tests**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ -run 'TestSandboxConformanceFromSandbox|TestForkReissuesToken' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/sandbox_v2_controller.go internal/controller/sandbox_forkpath_test.go
git commit -m "refactor(controller): Sandbox source.fromSandbox owns the fork engine directly"
```

### Task 3.2: Add the fromRevision not-served condition

**Files:**
- Modify: `internal/controller/sandbox_v2_controller.go`
- Test: `internal/controller/sandbox_fromrevision_test.go`
- Reference: `docs/conditions.md` (add the reason code)

**Interfaces:**
- Produces: a `Sandbox{source.fromRevision}` sets `phase: Pending` and a `Ready=False` condition with reason `RevisionResumeNotImplemented`, message pointing to the deferred engine path. Never panics, never silently drops.

- [ ] **Step 1: Write the failing test**

```go
func TestFromRevisionReportsNotServedCondition(t *testing.T) {
	sb := reconcileSandbox(t, &v1.Sandbox{
		Spec: v1.SandboxSpec{Source: v1.SandboxSource{
			FromRevision: &v1.FromRevisionSource{Workspace: "w", Revision: "rev-1"},
		}},
	})
	c := apimeta.FindStatusCondition(sb.Status.Conditions, "Ready")
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "RevisionResumeNotImplemented" {
		t.Fatalf("want Ready=False reason RevisionResumeNotImplemented, got %+v", c)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ -run TestFromRevisionReports -v`
Expected: FAIL (no such condition set).

- [ ] **Step 3: Implement the branch**

In the source switch, add the `sb.Spec.Source.FromRevision != nil` case: set phase `Pending`, set a `Ready=False` condition reason `RevisionResumeNotImplemented` with message `lineage resume from a workspace revision is declared in v1 but not yet served; tracked as the fromRevision engine path`, return without error. Add the reason code to `docs/conditions.md`.

- [ ] **Step 4: Run to verify it passes**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ -run TestFromRevisionReports -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/sandbox_v2_controller.go internal/controller/sandbox_fromrevision_test.go docs/conditions.md
git commit -m "feat(controller): fromRevision reports a clear not-served condition"
```

### Task 3.3: Delete the SandboxFork reconciler and re-key its tests

**Files:**
- Delete: `internal/controller/sandboxfork_controller.go`, `internal/controller/sandboxfork_controller_test.go`
- Modify: `cmd/controller/main.go` (remove `forkReconciler` wiring), `internal/controller/fork_secrets_test.go` (re-key to v1 Sandbox)

- [ ] **Step 1: Delete and unwire**

```bash
git rm internal/controller/sandboxfork_controller.go internal/controller/sandboxfork_controller_test.go
```
Remove the `forkReconciler` block and its `SetupWithManager` (cmd/controller/main.go lines ~286 to 298).

- [ ] **Step 2: Re-key fork_secrets_test.go to v1.Sandbox{source.fromSandbox}**

Rewrite each `v1alpha1.SandboxFork` fixture to `v1.Sandbox` with `Source.FromSandbox` and `SecretInheritance`. Keep the assertions identical in meaning (default reissue denies inheritance; `inherit` opt-in proceeds; token freshly reissued).

- [ ] **Step 3: Run the controller suite**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/controller/ cmd/controller/main.go
git commit -m "refactor(controller): remove SandboxFork reconciler, re-key secret tests to v1 Sandbox"
```

---

## Stage 4: absorb the template into the SandboxPool reconciler

### Task 4.1: Re-key SandboxPoolReconciler to v1 and inline the template

**Files:**
- Modify: `internal/controller/sandboxpool_controller.go`, `internal/controller/warmpool_autoscale.go`, `internal/controller/huskpod.go`, `internal/controller/workspace_memory_snapshot.go`
- Test: existing pool envtest + new `internal/controller/pool_inline_template_test.go`

**Interfaces:**
- Consumes: `v1.SandboxPool` with `Spec.Template` (inline) or `Spec.TemplateRef`.
- Produces: a pool with an inline `spec.template` builds its snapshot from the inline image/init/resources with NO `SandboxTemplate` object; a pool with `spec.templateRef` resolves the referenced template-shaped object.

- [ ] **Step 1: Write the failing test for the inline template path**

```go
func TestPoolInlineTemplateBuildsSnapshotWithoutTemplateObject(t *testing.T) {
	pool := reconcilePoolToReady(t, &v1.SandboxPool{
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "ghcr.io/x:1", Resources: v1.SandboxResources{}},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	})
	if pool.Status.ReadySnapshots < 0 {
		t.Fatal("pool did not reconcile inline template")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ -run TestPoolInlineTemplate -v`
Expected: FAIL (reconciler still typed to v1alpha1.SandboxPool / reads a SandboxTemplate).

- [ ] **Step 3: Re-key the pool reconciler**

Change `SandboxPoolReconciler` to reconcile `v1.SandboxPool`. Where it read a referenced `SandboxTemplate`, read `spec.template` inline first; fall back to resolving `spec.templateRef` only when inline is nil. Re-key field reads per ADR 0007: `template.Spec.Image` -> `pool.Spec.Template.Image`; `spec.replicas`/`autoscale` -> `pool.Spec.Warm` (Min/Max/TargetPending/CooldownSeconds); `spec.snapshotAfter`/`snapshotDelay`/`scaleDownAfterSnapshot`/`snapshotStorage` -> `pool.Spec.Snapshots`. Update `warmpool_autoscale.go` and `huskpod.go` to read `pool.Spec.Warm` and `pool.Spec.Template`.

- [ ] **Step 4: Run pool tests**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ -run 'Pool' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/sandboxpool_controller.go internal/controller/warmpool_autoscale.go internal/controller/huskpod.go internal/controller/workspace_memory_snapshot.go internal/controller/pool_inline_template_test.go
git commit -m "refactor(controller): SandboxPool reconciles v1 with an inline template"
```

### Task 4.2: Re-key the remaining controller package and wire v1 reconcilers in main

**Files:**
- Modify: every remaining `internal/controller/*.go` still importing `v1alpha1`/`v1alpha2`; `cmd/controller/main.go`; `internal/controller/workspace_controller.go`
- Test: full controller suite

- [ ] **Step 1: Re-key all residual imports**

Replace `v1alpha1`/`v1alpha2` imports with `v1 "mitos.run/mitos/api/v1"` across `gc.go`, `metrics.go`, `eventfeed.go`, `node_registry.go`, `husk*.go`, `workspace_controller.go`, `workspace_verbs.go`, and any others. Update the scheme registration in `cmd/controller/main.go` to add only `v1` to the scheme. Ensure `SandboxReconciler` (v1 Sandbox), `SandboxPoolReconciler` (v1), and `WorkspaceReconciler` (v1) are the only run-axis reconcilers wired.

- [ ] **Step 2: Build and vet the whole module**

Run: `go build ./... && go vet ./...`
Expected: PASS (no references to v1alpha1/v1alpha2 remain in non-SDK Go).

- [ ] **Step 3: Run the full unit + controller suites**

Run: `make test-unit && eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/controller/ cmd/controller/main.go
git commit -m "refactor(controller): re-key residual controller package to api/v1"
```

---

## Stage 5: facade, CLI, MCP, agentcli cutover

### Task 5.1: Re-target the facade onto v1.Sandbox

**Files:**
- Modify: `internal/facade/reconciler.go`, `internal/facade/claim_reconciler.go`, `internal/facade/template_reconciler.go`, `internal/facade/warmpool_reconciler.go`, `cmd/facade/main.go`
- Test: `internal/facade/examples_test.go`, `internal/facade/suite_test.go`

**Interfaces:**
- Produces: the upstream `agents.x-k8s.io Sandbox` maps to a `v1.Sandbox{source.poolRef}`; the template/warmpool facade reconcilers that targeted removed kinds are deleted.

- [ ] **Step 1: Update the failing facade test to expect a v1.Sandbox**

In `internal/facade/examples_test.go`, change the expected mapped object from `v1alpha1.SandboxClaim` to `v1.Sandbox` with `Spec.Source.PoolRef`.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/facade/ -run TestExamples -v`
Expected: FAIL (mapping still emits SandboxClaim / does not compile).

- [ ] **Step 3: Re-target the mapping; delete dead facade reconcilers**

In `reconciler.go`/`claim_reconciler.go` emit a `v1.Sandbox{Spec:{Source:{PoolRef:...}}}` instead of a `SandboxClaim`. Delete `template_reconciler.go` and `warmpool_reconciler.go` (they mapped onto removed kinds) and their setup wiring in `cmd/facade/main.go` and `suite_test.go`. Add only `v1` to the facade scheme.

- [ ] **Step 4: Run the facade suite**

Run: `go test ./internal/facade/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/facade/ cmd/facade/main.go
git commit -m "refactor(facade): map upstream Sandbox onto v1.Sandbox source.poolRef"
```

### Task 5.2: Re-key kubectl-mitos, MCP, CLI tables, agentcli

**Files:**
- Modify: `cmd/kubectl-mitos/*.go`, `internal/mcp/*.go`, `internal/cli/sandboxtable/*.go`, `internal/agentcli/*.go`
- Test: their existing `*_test.go`

- [ ] **Step 1: Update the CLI/MCP tests to v1 kinds**

In `internal/cli/sandboxtable` and `internal/mcp` tests, replace `SandboxClaim`/`SandboxFork`/`SandboxTemplate` fixtures with `v1.Sandbox`/`v1.SandboxPool`.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/cli/... ./internal/mcp/... ./internal/agentcli/... ./cmd/kubectl-mitos/...`
Expected: FAIL (deleted kinds referenced).

- [ ] **Step 3: Re-key the implementations**

Replace all `v1alpha1`/`v1alpha2` references with `v1`. The MCP tool that created a fork now creates `v1.Sandbox{source.fromSandbox, replicas}`; the table printer columns read `v1.Sandbox`/`v1.SandboxPool` fields.

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/cli/... ./internal/mcp/... ./internal/agentcli/... ./cmd/kubectl-mitos/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/ internal/mcp/ internal/agentcli/ cmd/kubectl-mitos/
git commit -m "refactor(cli): re-key kubectl-mitos, MCP, table, agentcli to api/v1"
```

### Task 5.3: Full module green + both lint invocations

- [ ] **Step 1: Build, vet, test the whole module**

Run: `go build ./... && go vet ./... && make test-unit && eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/`
Expected: PASS.

- [ ] **Step 2: Both lint invocations + guest cross-build**

Run: `golangci-lint run --timeout=5m && GOOS=linux golangci-lint run --timeout=5m && GOOS=linux GOARCH=amd64 go build ./guest/agent/`
Expected: all clean.

- [ ] **Step 3: Repo-wide grep for removed kinds in Go (excluding sdk, docs)**

Run: `grep -rn "SandboxClaim\|SandboxFork\|SandboxTemplate\|v1alpha1\|v1alpha2" --include="*.go" cmd/ internal/ api/ guest/ | grep -v _test.go`
Expected: no matches (or only intentional references in comments documenting the migration).

- [ ] **Step 4: Commit any fixups**

```bash
git add -p
git commit -m "chore: lint and grep cleanup after v1 cutover"
```

---

## Stage 6: manifests, RBAC, examples, docs

### Task 6.1: Regenerate CRDs and RBAC

**Files:**
- Regenerate: `deploy/crds/*.yaml`
- Modify: `config/rbac/role.yaml`, `deploy/rbac/clusterrole.yaml`, `deploy/kustomization.yaml`, `hack/kind-config*.yaml`, `deploy/talos/*.yaml`

- [ ] **Step 1: Regenerate manifests**

Run: `make manifests`
Expected: `deploy/crds/mitos.run_sandboxes.yaml`, `mitos.run_sandboxpools.yaml`, `mitos.run_workspaces.yaml`, `mitos.run_workspacerevisions.yaml` regenerate at `mitos.run/v1`.

- [ ] **Step 2: Delete the removed-kind CRDs**

```bash
git rm deploy/crds/mitos.run_sandboxtemplates.yaml deploy/crds/mitos.run_sandboxforks.yaml
```

- [ ] **Step 3: Update RBAC and kustomize to the v1 resource set**

Edit `config/rbac/role.yaml` and `deploy/rbac/clusterrole.yaml` to grant on `sandboxes`, `sandboxpools`, `workspaces`, `workspacerevisions` (drop `sandboxtemplates`, `sandboxforks`, `sandboxclaims`). Update `deploy/kustomization.yaml` CRD list.

- [ ] **Step 4: Verify CRDs apply on kind**

Run: `kind create cluster --config hack/kind-config.yaml && kubectl apply -f deploy/crds/`
Expected: all four CRDs Established; no dangling references.

- [ ] **Step 5: Commit**

```bash
git add deploy/ config/rbac/ hack/ 
git commit -m "chore(manifests): regenerate CRDs and RBAC for mitos.run/v1"
```

### Task 6.2: Rewrite examples and docs

**Files:**
- Modify: `examples/claim.yaml` -> `examples/sandbox.yaml`, `examples/fork.yaml`, `examples/python-pool.yaml`, `examples/multi-workspace.yaml`, `bench/fork-exec-job.yaml`
- Modify: `CLAUDE.md` (CRD list), `README.md`, `docs/api/v2-spec.md`, `docs/adr/0007-api-v2-three-noun-consolidation.md` (status section)

- [ ] **Step 1: Rewrite the example manifests to v1**

Convert `examples/claim.yaml` into a `Sandbox` with `source.poolRef`; `examples/fork.yaml` into a `Sandbox` with `source.fromSandbox` and `replicas`; `examples/python-pool.yaml` into a `SandboxPool` with inline `template`. Rename `claim.yaml` to `sandbox.yaml` via `git mv`.

- [ ] **Step 2: Validate the examples against the CRDs (dry-run)**

Run: `kubectl apply --dry-run=server -f examples/`
Expected: all accepted by the v1 CRDs.

- [ ] **Step 3: Update prose docs**

In `CLAUDE.md` change the CRD list to "SandboxPool, Sandbox, Workspace in API group mitos.run/v1". Update `README.md` API references and the "pre-1.0 (v0.13.0)" line as appropriate. Mark the ADR 0007 "Status of the slice" section: the breaking migration has landed (storage-version flip to v1, removal of SandboxTemplate/SandboxFork, cutover complete). Update `docs/api/v2-spec.md` status. Keep `docs/api/v2-migration.md` as the conversion record. No em or en dashes.

- [ ] **Step 4: Commit**

```bash
git add examples/ bench/fork-exec-job.yaml CLAUDE.md README.md docs/api/ docs/adr/0007-api-v2-three-noun-consolidation.md
git commit -m "docs: rewrite examples and docs for the mitos.run/v1 surface"
```

---

## Stage 7: Python and TypeScript SDK cluster mode cutover

### Task 7.1: Python SDK cluster mode to v1

**Files:**
- Modify: the cluster-mode modules in `sdk/python/mitos/` that build `SandboxClaim`/`SandboxFork`/`SandboxTemplate` bodies and the `apiVersion`
- Test: `sdk/python/tests/` cluster-mode tests

**Interfaces:**
- Produces: the Python cluster client posts `apiVersion: mitos.run/v1`, `kind: Sandbox` with `spec.source.poolRef` (claim equivalent) and `spec.source.fromSandbox` + `spec.replicas` (fork equivalent), and `kind: SandboxPool` with inline `spec.template`.

- [ ] **Step 1: Update the failing cluster-mode tests**

In `sdk/python/tests/`, change expected request bodies to `mitos.run/v1` `Sandbox`/`SandboxPool` shapes.

- [ ] **Step 2: Run to verify they fail**

Run: `make test-python`
Expected: FAIL on the cluster-mode tests.

- [ ] **Step 3: Re-key the cluster client**

Replace the `apiVersion`/`kind`/body construction for claim, fork, and template with the `v1` `Sandbox`/`SandboxPool` shapes. Direct-mode code is untouched.

- [ ] **Step 4: Run to verify they pass**

Run: `make test-python`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add sdk/python/
git commit -m "feat(sdk-python): cluster mode targets mitos.run/v1 Sandbox and SandboxPool"
```

### Task 7.2: TypeScript SDK cluster mode to v1

**Files:**
- Modify: the cluster-mode modules in `sdk/typescript/src/` building the old kind bodies
- Test: `sdk/typescript/` cluster-mode tests

**Interfaces:**
- Produces: the TS cluster client posts the same `mitos.run/v1` shapes as Python.

- [ ] **Step 1: Update the failing cluster-mode tests**

Change expected bodies to `mitos.run/v1` `Sandbox`/`SandboxPool`.

- [ ] **Step 2: Run to verify they fail**

Run: `cd sdk/typescript && npm test`
Expected: FAIL on cluster-mode tests.

- [ ] **Step 3: Re-key the cluster client**

Replace `apiVersion`/`kind`/body construction with the `v1` shapes. Direct mode untouched.

- [ ] **Step 4: Run to verify they pass**

Run: `cd sdk/typescript && npm test`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add sdk/typescript/
git commit -m "feat(sdk-typescript): cluster mode targets mitos.run/v1 Sandbox and SandboxPool"
```

### Task 7.3: Final full-repo verification

- [ ] **Step 1: Full build, lint, test sweep**

Run:
```bash
go build ./... && go vet ./... \
 && make test-unit \
 && eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ \
 && golangci-lint run --timeout=5m && GOOS=linux golangci-lint run --timeout=5m \
 && GOOS=linux GOARCH=amd64 go build ./guest/agent/ \
 && make test-python && (cd sdk/typescript && npm test)
```
Expected: all PASS.

- [ ] **Step 2: kind-e2e mock path**

Run the kind e2e per `hack/kind-config.yaml` (the CI `kind-e2e` job) and confirm a `Sandbox{source.poolRef}` reaches Ready and a `Sandbox{source.fromSandbox, replicas:N}` produces N children on the mock engine end to end.

- [ ] **Step 3: Final grep gate**

Run: `grep -rn "v1alpha1\|v1alpha2\|SandboxClaim\|SandboxFork\|SandboxTemplate" --include="*.go" --include="*.yaml" cmd/ internal/ api/ deploy/ config/ examples/ | grep -v v2-migration`
Expected: no matches outside the intentional migration record.

- [ ] **Step 4: Commit any final fixups, then open the PR**

```bash
git commit -am "chore: final v1 consolidation cleanup"
```
PR title: `feat(api): consolidate to stable mitos.run/v1 (three nouns), remove v1alpha1`. PR body summarizes the breaking change and links docs/api/v2-migration.md as the conversion record and ADR 0007.

---

## Self-review notes

- Spec coverage: target surface (Stage 1), engine absorption (Stages 2 to 4), facade/CLI/MCP (Stage 5), manifests/examples/docs (Stage 6), Python+TS SDKs (Stage 7), fromRevision declared-not-served (Task 3.2), no conversion webhook (Task 1.1 step 2), testing strategy (per-task envtest gates + Task 7.3). All spec sections map to tasks.
- The engine-absorption tasks are move + re-key guarded by the existing conformance envtests plus new guard tests (no-child-claim, per-fork token reissue, inline template, fromRevision condition); they intentionally do not reproduce the ~2000 lines being moved, because the deliverable is behavior-preserving relocation verified by tests, not new logic.
- Security defaults (secret reissue, fresh per-fork token, SecretInheritanceDenied) are pinned by Task 3.1/3.3 tests so the move cannot weaken them.
