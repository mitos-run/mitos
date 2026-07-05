# Husk Terminate Billing Fix (#688) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A husk-backed sandbox that hits its lifetime or idle limit actually stops (pod deleted) and stops billing at the terminate instant, instead of running and billing forever until object deletion.

**Architecture:** `terminateLifetime` in `internal/controller/sandboxclaim_controller.go` today only calls `terminateOnNode` (a no-op for husk claims, whose VM lives in the claimed pod) and stamps Terminated. The fix mirrors the claimed-husk-pod deletion block that `reconcileDelete` already has: list pods by `huskClaimLabel`, record the usage tail on the listed items BEFORE stamping the terminal phase (so the one-event guard in `recordHuskTerminations` passes), delete each pod with grace 0. The scrape lister then loses the pod and the pool refills the slot. Raw-forkd mode is a natural no-op (no pod carries the label).

**Tech Stack:** Go, controller-runtime (fake client for unit tests, envtest for integration), existing `usage.TerminationLog` tail-billing machinery (#682/#687, already on main).

## Global Constraints

- Never use em (U+2014) or en (U+2013) dashes anywhere: code, comments, commit messages, PR text. Connectors limited to `.` `,` `;` `:`.
- Error wrapping `fmt.Errorf("context: %w", err)`; octal literals `0o644`.
- Conventional commits with DCO: every commit via `git commit -s`.
- TDD: failing test first, same commit as the fix.
- Lint gate: BOTH `golangci-lint run --timeout=5m` AND `GOOS=linux golangci-lint run --timeout=5m`.
- Working tree: `/Users/jannesstubbemann/repos/mitos-run/mitos/.claude/worktrees/hosted-offer-gaps`, branch `fix/hosted-offer-gaps` (rename or branch off per PR packaging at the end).
- Envtest: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/`.

---

### Task 1: Unit test + fix: terminateLifetime deletes claimed husk pods

**Files:**
- Modify: `internal/controller/sandboxclaim_controller.go` (function `terminateLifetime`, lines ~1352-1395)
- Test: `internal/controller/usage_termination_test.go` (append; it is the in-package test file that already fabricates fake husk pods)

**Interfaces:**
- Consumes: `recordHuskTerminations(claim, pods, at)` and `r.now()` (both exist, `internal/controller/usage_termination.go`), `huskClaimLabel`/`huskLabel` consts (`internal/controller/huskpod.go`), `tenant.OrgLabelKey`.
- Produces: `terminateLifetime` now guarantees: claimed husk pods deleted, exactly one `usage.Termination` recorded per billable pod at the terminate instant, THEN phase stamped `SandboxTerminated`. Task 2 and 3 rely on this ordering.

- [ ] **Step 1: Write the failing test**

Append to `internal/controller/usage_termination_test.go` (package `controller`, in-package; model the scheme/fixture setup on the existing `TestRecordClaimHuskTerminations` in the same file and reuse its helpers if any):

```go
func TestTerminateLifetimeDeletesClaimedHuskPodsAndRecordsTail(t *testing.T) {
	scheme := newUsageTestScheme(t) // if the existing test builds its scheme inline, repeat that inline block here instead

	started := metav1.NewTime(time.Date(2026, 7, 4, 11, 0, 0, 0, time.UTC))
	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-688", Namespace: "mitos-org-acme"},
		Status: v1.SandboxStatus{
			Phase:     v1.SandboxRunning,
			StartedAt: &started,
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "python-husk-688",
			Namespace: "mitos-org-acme",
			Labels: map[string]string{
				huskLabel:           "true",
				huskClaimLabel:      "sb-688",
				tenant.OrgLabelKey:  "acme",
			},
		},
	}
	cl := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(claim, pod).
		WithStatusSubresource(&v1.Sandbox{}).
		Build()
	frozen := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	r := &SandboxReconciler{
		Client:            cl,
		UsageTerminations: usage.NewTerminationLog(),
		Now:               func() time.Time { return frozen },
	}

	if _, err := r.terminateLifetime(context.Background(), claim, "MaxLifetimeExceeded", "ttl expired"); err != nil {
		t.Fatalf("terminateLifetime: %v", err)
	}

	// The claimed husk pod is DELETED: Terminated must mean the VM stopped (#688).
	var pods corev1.PodList
	if err := cl.List(context.Background(), &pods, client.InNamespace("mitos-org-acme")); err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 0 {
		t.Fatalf("claimed husk pod not deleted; %d pods remain", len(pods.Items))
	}

	// Exactly one tail termination, at the terminate instant.
	got := r.UsageTerminations.Drain()
	if len(got) != 1 {
		t.Fatalf("terminations = %d, want 1", len(got))
	}
	if got[0].VMID != "python-husk-688" || got[0].OrgID != "acme" || !got[0].At.Equal(frozen) {
		t.Fatalf("termination = %+v, want vm python-husk-688 org acme at %v", got[0], frozen)
	}

	// The claim is terminal.
	var after v1.Sandbox
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "sb-688", Namespace: "mitos-org-acme"}, &after); err != nil {
		t.Fatal(err)
	}
	if after.Status.Phase != v1.SandboxTerminated {
		t.Fatalf("phase = %q, want Terminated", after.Status.Phase)
	}
}
```

Adjust ONLY: the scheme-construction lines and the exact `SandboxReconciler` clock field name to match the existing `TestRecordClaimHuskTerminations` in the same file (it uses a frozen `Now` func and `usage.NewTerminationLog()`); and `v1.SandboxRunning` to the actual running-phase constant used elsewhere in the package if it differs. Do not change the assertions.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/jannesstubbemann/repos/mitos-run/mitos/.claude/worktrees/hosted-offer-gaps && go test ./internal/controller/ -run TestTerminateLifetimeDeletesClaimedHuskPodsAndRecordsTail -v`
Expected: FAIL at "claimed husk pod not deleted; 1 pods remain" (the current code records the tail but never deletes the pod).

- [ ] **Step 3: Implement the fix**

In `internal/controller/sandboxclaim_controller.go`, inside `terminateLifetime`, replace this block:

```go
	// The backing VM is gone: close the usage tail window at this instant
	// (issue #682). Best-effort; a duplicate record from the later object
	// delete is deduplicated by the collector's finalized guard.
	r.recordClaimHuskTerminations(ctx, claim)
```

with:

```go
	// Husk path: the claim's backing VM lives in the claimed husk pod, which
	// terminateOnNode above never touches (forkd does not track husk pods), so
	// a lifetime terminate must delete the pod itself or the VM keeps running
	// and keeps being scraped and billed until object deletion (issue #688).
	// Mirror reconcileDelete: record the usage tail on the listed pods FIRST
	// (the phase is still pre-Terminated so the one-event guard in
	// recordHuskTerminations passes), then delete each pod so the scrape
	// lister drops it and the pool refills the slot. No-op in raw-forkd mode:
	// no pod carries the label.
	var claimedHusk corev1.PodList
	if err := r.List(ctx, &claimedHusk, client.InNamespace(claim.Namespace), client.MatchingLabels{huskClaimLabel: claim.Name}); err != nil {
		logger.Error(err, "list claimed husk pods on lifetime expiry; will retry", "claim", claim.Name)
		return ctrl.Result{RequeueAfter: capacityPendingRequeue}, nil
	}
	r.recordHuskTerminations(claim, claimedHusk.Items, r.now())
	for i := range claimedHusk.Items {
		if err := r.Delete(ctx, &claimedHusk.Items[i], client.GracePeriodSeconds(0)); err != nil && !apierrors.IsNotFound(err) {
			logger.Error(err, "delete claimed husk pod on lifetime expiry", "pod", claimedHusk.Items[i].Name)
			return ctrl.Result{}, err
		}
	}
```

Notes: `r.now()` is the reconciler's frozen-clock accessor already used by `recordClaimHuskTerminations`; if the accessor is spelled differently (e.g. direct `r.Now()` field call), match the existing usage in `usage_termination.go`. `corev1`, `client`, and `apierrors` are already imported in this file (used by `reconcileDelete`). Do NOT remove `recordClaimHuskTerminations` from `usage_termination.go`; other callers may exist, only this call site is replaced.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/controller/ -run TestTerminateLifetimeDeletesClaimedHuskPodsAndRecordsTail -v`
Expected: PASS

- [ ] **Step 5: Run the package unit tests (non-envtest) to catch regressions**

Run: `go test ./internal/controller/ -run 'TestRecordClaimHuskTerminations|TestTerminateLifetime' -v`
Expected: PASS (the existing tail-record test must still pass; the ordering record-then-delete preserves its behavior).

- [ ] **Step 6: Commit**

```bash
git add internal/controller/sandboxclaim_controller.go internal/controller/usage_termination_test.go
git commit -s -m "fix(controller): lifetime terminate deletes claimed husk pods so Terminated stops the VM and the billing (#688)"
```

---

### Task 2: Regression test: no double-billing after terminate then delete

**Files:**
- Test: `internal/controller/usage_termination_test.go` (append)

**Interfaces:**
- Consumes: the Task 1 guarantee (pods deleted, tail recorded, phase Terminated) and `recordHuskTerminations`'s phase guard (`if claim.Status.Phase == v1.SandboxTerminated { return }`).
- Produces: proof that the later object delete cannot synthesize a second billing event (the #688 issue's coupling warning with #687).

- [ ] **Step 1: Write the test**

```go
func TestTerminateLifetimeThenDeleteRecordsExactlyOneTail(t *testing.T) {
	// Same fixture as TestTerminateLifetimeDeletesClaimedHuskPodsAndRecordsTail:
	// build claim sb-688b + claimed husk pod, fake client, frozen clock, reconciler.
	// (Repeat the fixture inline; do not factor prematurely.)

	// 1. Lifetime terminate: records the one tail event and deletes the pod.
	if _, err := r.terminateLifetime(context.Background(), claim, "IdleTimeout", "idle"); err != nil {
		t.Fatalf("terminateLifetime: %v", err)
	}
	if n := len(r.UsageTerminations.Drain()); n != 1 {
		t.Fatalf("tail events after terminate = %d, want 1", n)
	}

	// 2. Simulate the later object delete's record step on the now-Terminated
	// claim: the phase guard must swallow it (one claim, one event), and the
	// pod list is empty anyway because Task 1 deleted it.
	var after v1.Sandbox
	if err := cl.Get(context.Background(), types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}, &after); err != nil {
		t.Fatal(err)
	}
	r.recordClaimHuskTerminations(context.Background(), &after)
	if n := len(r.UsageTerminations.Drain()); n != 0 {
		t.Fatalf("tail events after delete-time re-record = %d, want 0", n)
	}
}
```

- [ ] **Step 2: Run it**

Run: `go test ./internal/controller/ -run TestTerminateLifetimeThenDeleteRecordsExactlyOneTail -v`
Expected: PASS immediately (both guards already exist); if it FAILS, the ordering in Task 1 is wrong: fix Task 1, not the test.

- [ ] **Step 3: Commit**

```bash
git add internal/controller/usage_termination_test.go
git commit -s -m "test(controller): lifetime terminate then delete bills exactly one tail event (#688)"
```

---

### Task 3: Envtest: husk claim lifetime expiry deletes the pod object-level

**Files:**
- Test: `internal/controller/husk_lifetime_envtest_test.go` (create)
- Reference (read, do not modify): `internal/controller/husk_nodeloss_failover_envtest_test.go` (helpers `scheduleAndReadyHuskPod`, `dormantHuskPods`, `driveHuskPoolReconcile`, `setHuskTestActivator`, `uniqueName`), `internal/controller/suite_test.go` (the `huskClaim` reconciler is scoped via `OnlyLabels(HuskTestClaimLabel, HuskForkTestLabel)`), `internal/controller/claim_lifecycle_test.go` (`waitClaimTerminated`, `terminatedReason`).

**Interfaces:**
- Consumes: Task 1's controller behavior, the suite's husk-mode claim reconciler (label-scoped via `HuskTestClaimLabel`).
- Produces: object-level proof: pool creates dormant pod, claim activates it, TTL expires, claim goes Terminated AND the claimed pod is gone while the pool refills a dormant replacement.

- [ ] **Step 1: Write the test**

Create `internal/controller/husk_lifetime_envtest_test.go` (package `controller_test`), modeled on the nodeloss test's setup sequence. Skeleton to adapt (keep the four assertions verbatim; wire the setup exactly the way the nodeloss test does it, same helper names and label plumbing):

```go
func TestHuskClaimLifetimeExpiryDeletesClaimedPod(t *testing.T) {
	poolName := uniqueName("lt688-pool")

	// 1. Husk pool with one warm replica; drive the pool reconciler until a
	//    dormant husk pod exists, then bind + force it Running/Ready the way
	//    scheduleAndReadyHuskPod does (envtest has no scheduler or kubelet).
	// 2. Create a Sandbox claim labeled with HuskTestClaimLabel (so the suite's
	//    husk-mode reconciler owns it) referencing the pool, with
	//    Lifetime.TTL = 2s, and let the stubbed activator (setHuskTestActivator)
	//    take it to Ready.

	got := waitClaimTerminated(t, claimName)
	if r := terminatedReason(got); r != "MaxLifetimeExceeded" {
		t.Fatalf("terminated reason = %q, want MaxLifetimeExceeded", r)
	}

	// The claimed pod must be GONE (not merely the claim stamped Terminated).
	deadline := time.Now().Add(20 * time.Second)
	for {
		var pods corev1.PodList
		if err := k8sClient.List(context.Background(), &pods,
			client.InNamespace(claimNamespace),
			client.MatchingLabels{"mitos.run/claim": claimName}); err != nil {
			t.Fatal(err)
		}
		if len(pods.Items) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("claimed husk pod still present %s after Terminated", pods.Items[0].Name)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
```

If the suite's husk claim reconciler or the pool reconciler cannot drive a full Ready husk claim in envtest without KVM (check how the nodeloss test gets a CLAIMED pod: it may claim via the test activator or fabricate the claimed label directly), fall back to the same fabrication the nodeloss test uses: what matters object-level is a Running pod labeled `mitos.run/claim: <claim>` at the moment the TTL fires.

- [ ] **Step 2: Run envtest**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ -run TestHuskClaimLifetimeExpiryDeletesClaimedPod -v -timeout 120s`
Expected: PASS. Before Task 1's fix this would hang at the pod-gone poll and fail at the 20s deadline; you can verify by `git stash`-ing the Task 1 hunk once if you want the red proof, then unstash.

- [ ] **Step 3: Run the full controller envtest suite**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ -timeout 20m`
Expected: PASS, no regressions.

- [ ] **Step 4: Commit**

```bash
git add internal/controller/husk_lifetime_envtest_test.go
git commit -s -m "test(controller): envtest gate that husk lifetime expiry deletes the claimed pod (#688)"
```

---

### Task 4: Docs + lint + wrap-up

**Files:**
- Modify: `docs/lifecycle.md` (the maxLifetime/idleTimeout section) and `docs/failure-gc.md` (the claim-TTL guarantee row)

**Interfaces:**
- Consumes: the now-true behavior from Tasks 1-3.
- Produces: docs stating the guarantee: on lifetime or idle expiry a husk-backed sandbox's pod is deleted at terminate time; Terminated means the VM stopped and billing stopped; the warm slot refills.

- [ ] **Step 1: Update docs**

In `docs/lifecycle.md`, in the lifetime/idle reap section, add (adapting to surrounding prose style):

> On expiry the claim's backing VM is actually stopped at the terminate instant: in husk mode the claimed pod is deleted (the warm pool refills the slot), in raw-forkd mode the node engine terminates the VM. Terminated therefore also marks the end of metered usage; the final billing sample is recorded at the terminate instant (issue #682's tail window), never at the later object deletion.

In `docs/failure-gc.md`, extend the claim-TTL guarantee entry to name the proving tests: `TestTerminateLifetimeDeletesClaimedHuskPodsAndRecordsTail`, `TestTerminateLifetimeThenDeleteRecordsExactlyOneTail`, `TestHuskClaimLifetimeExpiryDeletesClaimedPod`.

Check both edits for em/en dashes before committing.

- [ ] **Step 2: Lint both ways**

Run: `golangci-lint run --timeout=5m && GOOS=linux golangci-lint run --timeout=5m`
Expected: clean.

- [ ] **Step 3: Commit**

```bash
git add docs/lifecycle.md docs/failure-gc.md
git commit -s -m "docs: Terminated means stopped; lifetime expiry deletes the husk pod and ends billing (#688)"
```
