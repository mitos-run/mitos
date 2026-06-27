# Fork SIGUSR2 opt-in (#467) and husk stale-digest invalidation (#461) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop the fork SIGUSR2 broadcast from silently killing tenant processes that do not handle it (#467), and make warm husk pods converge to a rebuilt snapshot instead of CrashLoopBackOff on a stale digest (#461).

**Architecture:** #467 is a guest-agent change: `select_targets` signals a pid only when `/proc/<pid>/status` `SigCgt` confirms a SIGUSR2 handler (fail-safe skip otherwise), layered under the existing #460 registered-workload session exclusion. #461 is a controller change: stamp each warm husk pod with the snapshot digest + node it verifies against, and reap dormant pods whose stamped digest no longer matches their node's current recorded digest so the deficit logic refills them fresh.

**Tech Stack:** Rust (guest/agent-rs, `libc`, `std::fs`), Go (internal/controller, controller-runtime, envtest).

## Global Constraints

- Punctuation: never use em or en dashes anywhere (source, comments, docs, commit messages). Only `. , ; :` connectors; ASCII hyphen-minus for ranges/compounds.
- Go: error wrapping `fmt.Errorf("context: %w", err)`; octal `0o644`; gofmt + golangci-lint clean is a merge gate, run BOTH `golangci-lint run --timeout=5m` AND `GOOS=linux golangci-lint run --timeout=5m`.
- Rust guest agent: every `unsafe` block needs a SAFETY comment (none added here; this change is pure file parsing).
- Secret values never logged; log keys and counts only.
- DCO: every commit carries `Signed-off-by` (use `git commit -s`).
- TDD: failing test first, behavior change lands with its test in the same commit.
- Stage explicit paths only; never `git add -A`.
- Commit message trailer: `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
- Security-sensitive paths (`guest/agent-rs`, controller husk path) need a named human reviewer; threat-model delta lands in the same PR as the #467 surface change.

---

## Part A: #467 SIGUSR2 opt-in by handler detection (guest/agent-rs)

### Task 1: `process_catches_sigusr2` helper

**Files:**
- Modify: `guest/agent-rs/src/sys/signal.rs` (add helper above `select_targets`; add tests in the `tests` module)

**Interfaces:**
- Produces: `pub fn process_catches_sigusr2(proc_path: &str, pid: i32) -> bool` — true only when `<proc_path>/<pid>/status` `SigCgt` has the SIGUSR2 bit set; false on any read/parse failure.

- [ ] **Step 1: Write the failing tests** (add to the `#[cfg(test)] mod tests` block in `guest/agent-rs/src/sys/signal.rs`)

```rust
    #[test]
    fn process_catches_sigusr2_reads_sigcgt() {
        let dir = tempfile::tempdir().unwrap();
        // bit (SIGUSR2 - 1) = bit 11 = 0x800.
        let with_handler = dir.path().join("10");
        std::fs::create_dir(&with_handler).unwrap();
        std::fs::write(
            with_handler.join("status"),
            "Name:\tapp\nState:\tS\nSigCgt:\t0000000000000800\n",
        )
        .unwrap();
        let no_handler = dir.path().join("11");
        std::fs::create_dir(&no_handler).unwrap();
        std::fs::write(
            no_handler.join("status"),
            "Name:\tapp\nState:\tS\nSigCgt:\t0000000000000000\n",
        )
        .unwrap();
        let malformed = dir.path().join("12");
        std::fs::create_dir(&malformed).unwrap();
        std::fs::write(malformed.join("status"), "SigCgt:\tnothex\n").unwrap();

        let proc = dir.path().to_str().unwrap();
        assert!(process_catches_sigusr2(proc, 10), "SIGUSR2 bit set => handler");
        assert!(!process_catches_sigusr2(proc, 11), "no bit => no handler");
        assert!(!process_catches_sigusr2(proc, 12), "malformed => fail-safe false");
        assert!(!process_catches_sigusr2(proc, 99), "missing file => fail-safe false");
    }
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd guest/agent-rs && cargo test --lib sys::signal::tests::process_catches_sigusr2_reads_sigcgt`
Expected: FAIL to compile ("cannot find function `process_catches_sigusr2`").

- [ ] **Step 3: Write the helper** (insert in `guest/agent-rs/src/sys/signal.rs` immediately before `select_targets`)

```rust
/// process_catches_sigusr2 reports whether `pid` installed a SIGUSR2 handler,
/// read from the SigCgt (caught-signals) bitmask in `<proc_path>/<pid>/status`.
/// SIGUSR2's default disposition is terminate, so the fork broadcast signals ONLY
/// confirmed handlers (issue #467); a process that caught no SIGUSR2 could not act
/// on the reseed notification anyway, it could only die. Fail-safe: any read or
/// parse failure returns false, so a process we cannot confirm is never signaled
/// (and so never killed by the broadcast).
pub fn process_catches_sigusr2(proc_path: &str, pid: i32) -> bool {
    let status = match fs::read_to_string(format!("{proc_path}/{pid}/status")) {
        Ok(s) => s,
        Err(_) => return false,
    };
    for line in status.lines() {
        if let Some(rest) = line.strip_prefix("SigCgt:") {
            let Ok(mask) = u64::from_str_radix(rest.trim(), 16) else {
                return false;
            };
            // SigCgt bit (signo - 1) is set when a handler is installed for signo.
            let bit = 1u64 << ((libc::SIGUSR2 - 1) as u32);
            return mask & bit != 0;
        }
    }
    false
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd guest/agent-rs && cargo test --lib sys::signal::tests::process_catches_sigusr2_reads_sigcgt`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add guest/agent-rs/src/sys/signal.rs
git commit -s -m "feat(guest-agent): detect a process's SIGUSR2 handler from SigCgt (#467)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

### Task 2: gate `select_targets` on handler detection

**Files:**
- Modify: `guest/agent-rs/src/sys/signal.rs` (`select_targets`; update existing tests `excludes_pids_in_an_excluded_session` and `sigusr2_delivered_to_child_via_synthetic_proc`; add `targets_only_processes_that_catch_sigusr2`)

**Interfaces:**
- Consumes: `process_catches_sigusr2` (Task 1).

- [ ] **Step 1: Add the failing test and update the session-exclusion test**

Add this new test to the `tests` module:

```rust
    #[test]
    fn targets_only_processes_that_catch_sigusr2() {
        let dir = tempfile::tempdir().unwrap();
        // pid 300 installs a SIGUSR2 handler (SigCgt bit 11 set); pid 301 does not.
        for (pid, sigcgt) in [(300, "0000000000000800"), (301, "0000000000000000")] {
            let p = dir.path().join(pid.to_string());
            std::fs::create_dir(&p).unwrap();
            std::fs::write(p.join("stat"), format!("{pid} (app) S 1 {pid} {pid} 0 0")).unwrap();
            std::fs::write(p.join("status"), format!("SigCgt:\t{sigcgt}\n")).unwrap();
        }
        let proc = dir.path().to_str().unwrap();
        let selected = select_targets(proc, &HashSet::new());
        assert!(selected.contains(&300), "a SIGUSR2 handler must be a target");
        assert!(!selected.contains(&301), "a non-handler must never be a target");
    }
```

In the existing `excludes_pids_in_an_excluded_session`, after each pid's `stat` is written, also write a `status` granting the SIGUSR2 handler bit so the surviving pids remain selectable under the new gate:

```rust
            std::fs::write(p.join("status"), "SigCgt:\t0000000000000800\n").unwrap();
```

(Place it inside the existing `for (pid, sid) in [...]` loop, right after the `stat` write.)

- [ ] **Step 2: Run the tests to verify the new one fails (and the updated one still asserts correctly)**

Run: `cd guest/agent-rs && cargo test --lib sys::signal::tests::targets_only_processes_that_catch_sigusr2`
Expected: FAIL (pid 301 is currently selected because there is no handler gate yet).

- [ ] **Step 3: Add the handler gate to `select_targets`**

In `select_targets`, replace the final push:

```rust
        targets.push(pid);
```

with the gate plus push (keep it AFTER the existing PID 1 / self / excluded-session checks):

```rust
        // Issue #467: SIGUSR2's default disposition is terminate, so signal ONLY
        // processes that installed a SIGUSR2 handler (opt-in by handler presence).
        // Fail-safe: a process whose handler we cannot confirm is skipped, never
        // killed. This is layered UNDER the session exclusion above so a registered
        // serving workload (e.g. nginx, which traps SIGUSR2 for binary upgrade) is
        // left entirely alone rather than triggered.
        if !process_catches_sigusr2(proc_path, pid) {
            continue;
        }
        targets.push(pid);
```

- [ ] **Step 4: Update `sigusr2_delivered_to_child_via_synthetic_proc`**

The child installs a real SIGUSR2 handler, so its synthetic /proc dir must expose a matching `SigCgt`, else the new fail-safe gate (correctly) skips it. After `std::fs::create_dir_all(&child_dir).expect(...)`, add:

```rust
        // The child installs a SIGUSR2 handler; expose it via SigCgt so the
        // handler-detection gate (issue #467) signals it.
        std::fs::write(child_dir.join("status"), "SigCgt:\t0000000000000800\n")
            .expect("write synthetic status");
```

- [ ] **Step 5: Run the full signal-module test suite**

Run: `cd guest/agent-rs && cargo test --lib sys::signal`
Expected: PASS (all tests, including `targets_only_processes_that_catch_sigusr2`, the updated session-exclusion test, and the delivery test).

- [ ] **Step 6: Lint the guest agent**

Run: `cd guest/agent-rs && cargo clippy --all-targets -- -D warnings && cargo fmt --check`
Expected: clean (no warnings, formatting unchanged).

- [ ] **Step 7: Commit**

```bash
git add guest/agent-rs/src/sys/signal.rs
git commit -s -m "fix(guest-agent): signal SIGUSR2 only to confirmed handlers, not all userspace (#467)

The fork NotifyForked broadcast default-terminated any process that did not
install a SIGUSR2 handler, silently killing captured serving workloads. Gate
select_targets on the process's SigCgt handler bit, fail-safe skipping any
process whose handler cannot be confirmed, layered under the existing #460
registered-workload session exclusion.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

### Task 3: #467 docs and threat-model delta

**Files:**
- Modify: `docs/fork-correctness.md` (the SIGUSR2 signal rows: the legend row 3 and the "SIGUSR2 signal to userspace" table row)
- Modify: `docs/threat-model.md` (the serving-workload per-fork SIGUSR2 reset paragraph near line 602)

- [ ] **Step 1: Update `docs/fork-correctness.md`**

In the "SIGUSR2 signal to userspace" row (the `fork/signal.rs` description), append after the existing #460 session-exclusion sentence:

```
Issue #467: SIGUSR2's default disposition is terminate, so the broadcast now
delivers ONLY to processes that installed a SIGUSR2 handler (the SigCgt bit in
/proc/<pid>/status), fail-safe skipping any process whose handler cannot be
confirmed. A non-handler process is never signaled and so never silently killed;
a registered serving workload's whole session is still excluded outright (so a
handler-having app like nginx is left alone, not triggered).
```

Set that row's status to `**done (handler-gated broadcast + workload-session exclusion; unit-tested)**`.

- [ ] **Step 2: Update `docs/threat-model.md`**

In the serving-workload note, extend the sentence about the per-fork SIGUSR2 reset to record the new behavior:

```
The per-fork SIGUSR2 reset no longer default-terminates a non-handler process:
the broadcast delivers only to confirmed SIGUSR2 handlers (SigCgt), so a captured
serving workload that installs no handler survives the fork rather than being
silently killed (issue #467). This closes a self-DoS/availability hazard on the
fork path; it adds no new escape surface (the signal set only shrinks).
```

- [ ] **Step 3: Verify no dashes were introduced**

Run: `! grep -nP "[\x{2013}\x{2014}]" docs/fork-correctness.md docs/threat-model.md`
Expected: no output (exit 0); if any match prints, rewrite it.

- [ ] **Step 4: Commit**

```bash
git add docs/fork-correctness.md docs/threat-model.md
git commit -s -m "docs(fork-correctness,threat-model): SIGUSR2 broadcast is handler-gated (#467)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Part B: #461 husk stale-digest invalidation (internal/controller)

### Task 4: stamp warm husk pods with their snapshot digest and node

**Files:**
- Modify: `internal/controller/huskpod.go` (add two annotation constants near `huskClaimLabel` ~line 57; stamp them in `buildHuskPod`'s ObjectMeta ~line 688)
- Test: `internal/controller/husk_stale_digest_test.go` (new, `package controller_test`)

**Interfaces:**
- Produces: `huskTemplateDigestAnnotation = "mitos.run/template-digest"`, `huskSnapshotNodeAnnotation = "mitos.run/snapshot-node"`; a warm husk pod pinned to exactly one snapshot node with a non-empty digest carries both annotations.

- [ ] **Step 1: Write the failing test** (create `internal/controller/husk_stale_digest_test.go`)

```go
package controller_test

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/controller"
)

// A warm husk pod must record the snapshot digest and node it verifies against,
// so a later reconcile can reap it if the snapshot is rebuilt under it (#461).
func TestHuskPodStampsDigestAndNode(t *testing.T) {
	c := k8sClient
	const (
		poolName = "stamp-pool"
		tmpl     = poolName
		digestA  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: poolName, Namespace: "default"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}
	if err := c.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, p := range listHuskPods(t, c, poolName) {
			_ = c.Delete(ctx, &p)
		}
		_ = c.Delete(ctx, pool)
	})

	reg := controller.NewNodeRegistry()
	reg.Register(&controller.NodeInfo{
		Name:            "node-a",
		TemplateIDs:     []string{tmpl},
		TemplateDigests: map[string]string{tmpl: digestA},
	})
	r := &controller.SandboxPoolReconciler{
		Client:          c,
		NodeRegistry:    reg,
		EnableHuskPods:  true,
		HuskStubImage:   "mitos-husk-stub:test",
		KVMResourceName: "mitos.run/kvm",
	}
	if _, err := r.ReconcileHuskPodsForTest(ctx, pool, pool.Spec.Template); err != nil {
		t.Fatalf("reconcileHuskPods: %v", err)
	}

	pods := listHuskPods(t, c, poolName)
	if len(pods) != 1 {
		t.Fatalf("want 1 husk pod, got %d", len(pods))
	}
	if got := pods[0].Annotations["mitos.run/template-digest"]; got != digestA {
		t.Errorf("template-digest annotation = %q, want %q", got, digestA)
	}
	if got := pods[0].Annotations["mitos.run/snapshot-node"]; got != "node-a" {
		t.Errorf("snapshot-node annotation = %q, want %q", got, "node-a")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ -run TestHuskPodStampsDigestAndNode -count=1`
Expected: FAIL (annotations are empty/nil).

- [ ] **Step 3: Add the constants** (in `internal/controller/huskpod.go`, after the `huskClaimLabel` const ~line 57)

```go
	// huskTemplateDigestAnnotation records the content-addressed snapshot digest a
	// warm husk pod was built to verify against, so the reconcile can reap a pod
	// whose snapshot was rebuilt under it (issue #461). An annotation, not a label:
	// a digest contains ':' and exceeds 63 chars, both invalid in a label value.
	huskTemplateDigestAnnotation = "mitos.run/template-digest"
	// huskSnapshotNodeAnnotation records the single snapshot node a warm husk pod
	// is pinned to, so the reconcile compares the pod's stamped digest against THAT
	// node's current recorded digest (per-node digests differ, issue #175).
	huskSnapshotNodeAnnotation = "mitos.run/snapshot-node"
```

- [ ] **Step 4: Stamp the annotations in `buildHuskPod`** (just before the `pod := &corev1.Pod{` literal ~line 688)

```go
	// Stamp the per-node digest + node on a warm pod pinned to exactly one snapshot
	// node, so a later reconcile can reap it if that node's snapshot is rebuilt
	// under a new digest (issue #461). The fallback path (no single pinned node) and
	// fork children (no SnapshotNodes) get no stamp and are never reaped.
	annotations := map[string]string{}
	if len(opts.SnapshotNodes) == 1 && opts.ExpectedDigest != "" {
		annotations[huskTemplateDigestAnnotation] = opts.ExpectedDigest
		annotations[huskSnapshotNodeAnnotation] = opts.SnapshotNodes[0]
	}
```

Then add `Annotations: annotations,` to the `ObjectMeta` literal, after the `Labels:` field:

```go
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: pool.Name + "-husk-",
			Namespace:    pool.Namespace,
			Labels: map[string]string{
				huskPoolLabel: pool.Name,
				huskLabel:     "true",
			},
			Annotations: annotations,
		},
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ -run TestHuskPodStampsDigestAndNode -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/huskpod.go internal/controller/husk_stale_digest_test.go
git commit -s -m "feat(controller): stamp warm husk pods with their snapshot digest and node (#461)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

### Task 5: reap dormant husk pods whose stamped digest is stale

**Files:**
- Modify: `internal/controller/huskpod.go` (add `huskPodHasStaleDigest`; insert a reap pass in `reconcileHuskPods` after `owned` is built, ~line 1017)
- Test: `internal/controller/husk_stale_digest_test.go` (append the reap tests)

**Interfaces:**
- Consumes: `huskTemplateDigestAnnotation`, `huskSnapshotNodeAnnotation` (Task 4); `NodeRegistry.TemplateDigestOnNode(node, templateID) (string, bool)`.
- Produces: `func (r *SandboxPoolReconciler) huskPodHasStaleDigest(p *corev1.Pod, templateID string) bool`.

- [ ] **Step 1: Write the failing tests** (append to `internal/controller/husk_stale_digest_test.go`)

```go
// A dormant husk pod whose node's snapshot was rebuilt under a new digest is
// reaped and refilled with the fresh digest (#461 acceptance, controller half).
func TestHuskPodWithStaleDigestIsReapedAndRefilled(t *testing.T) {
	c := k8sClient
	const (
		poolName = "stale-pool"
		tmpl     = poolName
		oldD     = "1111111111111111111111111111111111111111111111111111111111111111"
		newD     = "2222222222222222222222222222222222222222222222222222222222222222"
	)
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: poolName, Namespace: "default"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}
	if err := c.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, p := range listHuskPods(t, c, poolName) {
			_ = c.Delete(ctx, &p)
		}
		_ = c.Delete(ctx, pool)
	})

	reg := controller.NewNodeRegistry()
	reg.Register(&controller.NodeInfo{
		Name:            "node-a",
		TemplateIDs:     []string{tmpl},
		TemplateDigests: map[string]string{tmpl: oldD},
	})
	r := &controller.SandboxPoolReconciler{
		Client:          c,
		NodeRegistry:    reg,
		EnableHuskPods:  true,
		HuskStubImage:   "mitos-husk-stub:test",
		KVMResourceName: "mitos.run/kvm",
	}
	if _, err := r.ReconcileHuskPodsForTest(ctx, pool, pool.Spec.Template); err != nil {
		t.Fatalf("reconcileHuskPods (build): %v", err)
	}
	if pods := listHuskPods(t, c, poolName); len(pods) != 1 || pods[0].Annotations["mitos.run/template-digest"] != oldD {
		t.Fatalf("setup: want 1 pod stamped %q, got %+v", oldD, pods)
	}

	// Simulate a same-name rebuild: node-a now reports a NEW digest.
	reg.AddTemplateWithDigest("node-a", tmpl, newD)
	if _, err := r.ReconcileHuskPodsForTest(ctx, pool, pool.Spec.Template); err != nil {
		t.Fatalf("reconcileHuskPods (after rebuild): %v", err)
	}

	// The stale pod is reaped and a fresh-digest pod refills the slot. envtest has
	// no kubelet, so poll until the only NON-terminating pod carries the new digest.
	waitForSingleHuskDigest(t, c, poolName, newD)
}

// A pod whose stamped digest still matches its node is NOT reaped (no churn).
func TestHuskPodWithCurrentDigestNotReaped(t *testing.T) {
	c := k8sClient
	const (
		poolName = "current-pool"
		tmpl     = poolName
		dig      = "3333333333333333333333333333333333333333333333333333333333333333"
	)
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: poolName, Namespace: "default"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}
	if err := c.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, p := range listHuskPods(t, c, poolName) {
			_ = c.Delete(ctx, &p)
		}
		_ = c.Delete(ctx, pool)
	})
	reg := controller.NewNodeRegistry()
	reg.Register(&controller.NodeInfo{
		Name:            "node-a",
		TemplateIDs:     []string{tmpl},
		TemplateDigests: map[string]string{tmpl: dig},
	})
	r := &controller.SandboxPoolReconciler{
		Client: c, NodeRegistry: reg, EnableHuskPods: true,
		HuskStubImage: "mitos-husk-stub:test", KVMResourceName: "mitos.run/kvm",
	}
	if _, err := r.ReconcileHuskPodsForTest(ctx, pool, pool.Spec.Template); err != nil {
		t.Fatalf("reconcileHuskPods (build): %v", err)
	}
	pods := listHuskPods(t, c, poolName)
	if len(pods) != 1 {
		t.Fatalf("want 1 pod, got %d", len(pods))
	}
	firstUID := pods[0].UID
	if _, err := r.ReconcileHuskPodsForTest(ctx, pool, pool.Spec.Template); err != nil {
		t.Fatalf("reconcileHuskPods (steady): %v", err)
	}
	pods = listHuskPods(t, c, poolName)
	if len(pods) != 1 || pods[0].UID != firstUID {
		t.Errorf("steady-state pod churned: want same UID %s, got %+v", firstUID, pods)
	}
}
```

Add this poll helper at the bottom of the file:

```go
// waitForSingleHuskDigest polls until exactly one NON-terminating husk pod exists
// and it carries wantDigest. envtest has no kubelet, so a reaped pod is removed
// asynchronously; poll rather than asserting once.
func waitForSingleHuskDigest(t *testing.T, c client.Client, poolName, wantDigest string) {
	t.Helper()
	for i := 0; i < 50; i++ {
		live := make([]corev1.Pod, 0)
		for _, p := range listHuskPods(t, c, poolName) {
			if p.DeletionTimestamp == nil {
				live = append(live, p)
			}
		}
		if len(live) == 1 && live[0].Annotations["mitos.run/template-digest"] == wantDigest {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("did not converge to a single husk pod with digest %q", wantDigest)
}
```

Add the imports this file now needs to its import block: `"time"`, `corev1 "k8s.io/api/core/v1"`, and `"sigs.k8s.io/controller-runtime/pkg/client"`.

- [ ] **Step 2: Run the tests to verify the reap test fails**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ -run 'TestHuskPodWithStaleDigestIsReapedAndRefilled|TestHuskPodWithCurrentDigestNotReaped' -count=1`
Expected: `TestHuskPodWithStaleDigestIsReapedAndRefilled` FAILS (the stale pod is never reaped, so the pod still carries the old digest); `TestHuskPodWithCurrentDigestNotReaped` PASSES (no reap path exists yet, so no churn).

- [ ] **Step 3: Add the staleness helper** (in `internal/controller/huskpod.go`, near `reconcileHuskPods`)

```go
// huskPodHasStaleDigest reports whether a husk pod's stamped snapshot digest no
// longer matches its pinned node's current recorded digest for the template
// (issue #461). A claimed (activating/active) pod is never stale here: it holds a
// tenant VM and must not be reaped. A pod with no stamped digest/node (the
// pre-digest fallback or a fork child) or a node with no currently known digest
// is treated as NOT stale, so steady state never churns.
func (r *SandboxPoolReconciler) huskPodHasStaleDigest(p *corev1.Pod, templateID string) bool {
	if p.Labels[huskClaimLabel] != "" {
		return false
	}
	stamped := p.Annotations[huskTemplateDigestAnnotation]
	node := p.Annotations[huskSnapshotNodeAnnotation]
	if stamped == "" || node == "" || r.NodeRegistry == nil {
		return false
	}
	current, known := r.NodeRegistry.TemplateDigestOnNode(node, templateID)
	if !known {
		return false
	}
	return current != stamped
}
```

- [ ] **Step 4: Insert the reap pass in `reconcileHuskPods`** (immediately after the `owned` slice is built, i.e. after the loop ending at ~line 1017 and before `r.observeRefillForReadyPods(ctx, owned)`)

```go
	// Issue #461: a warm husk pod bakes the snapshot digest it verifies against.
	// If the pool's snapshot is rebuilt under the same name (same templateID, new
	// mem + new digest), an existing dormant pod re-hashes the NEW mem against its
	// OLD manifest and CrashLoopBackOffs forever. Reap dormant pods whose stamped
	// digest no longer matches their node's current recorded digest so the scale-up
	// below refills them against the fresh snapshot. Claimed pods (a tenant VM) are
	// never reaped.
	templateID := poolTemplateID(pool)
	kept := owned[:0:0]
	for i := range owned {
		p := owned[i]
		if r.huskPodHasStaleDigest(&p, templateID) {
			if err := r.Delete(ctx, &p); err != nil && !apierrors.IsNotFound(err) {
				return huskReconcileResult{}, fmt.Errorf("delete stale husk pod %s/%s: %w", p.Namespace, p.Name, err)
			}
			logger.Info("reaped husk pod with stale snapshot digest", "pod", p.Name, "node", p.Annotations[huskSnapshotNodeAnnotation])
			continue
		}
		kept = append(kept, p)
	}
	owned = kept
```

- [ ] **Step 5: Run the reap tests to verify they pass**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ -run 'TestHuskPodWithStaleDigestIsReapedAndRefilled|TestHuskPodWithCurrentDigestNotReaped|TestHuskPodStampsDigestAndNode' -count=1`
Expected: PASS (all three).

- [ ] **Step 6: Add the guard tests for claimed and fallback pods** (append to `husk_stale_digest_test.go`)

```go
// A CLAIMED husk pod with a stale digest is never reaped (it holds a tenant VM).
func TestClaimedHuskPodWithStaleDigestNotReaped(t *testing.T) {
	c := k8sClient
	const (
		poolName = "claimed-pool"
		tmpl     = poolName
		oldD     = "4444444444444444444444444444444444444444444444444444444444444444"
		newD     = "5555555555555555555555555555555555555555555555555555555555555555"
	)
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: poolName, Namespace: "default"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}
	if err := c.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, p := range listHuskPods(t, c, poolName) {
			_ = c.Delete(ctx, &p)
		}
		_ = c.Delete(ctx, pool)
	})
	reg := controller.NewNodeRegistry()
	reg.Register(&controller.NodeInfo{
		Name: "node-a", TemplateIDs: []string{tmpl},
		TemplateDigests: map[string]string{tmpl: oldD},
	})
	r := &controller.SandboxPoolReconciler{
		Client: c, NodeRegistry: reg, EnableHuskPods: true,
		HuskStubImage: "mitos-husk-stub:test", KVMResourceName: "mitos.run/kvm",
	}
	if _, err := r.ReconcileHuskPodsForTest(ctx, pool, pool.Spec.Template); err != nil {
		t.Fatalf("reconcileHuskPods (build): %v", err)
	}
	pods := listHuskPods(t, c, poolName)
	if len(pods) != 1 {
		t.Fatalf("want 1 pod, got %d", len(pods))
	}
	// Mark the pod claimed (consumed by a SandboxClaim): it is no longer a warm slot.
	claimed := pods[0]
	if claimed.Labels == nil {
		claimed.Labels = map[string]string{}
	}
	claimed.Labels["mitos.run/claim"] = "some-claim"
	if err := c.Update(ctx, &claimed); err != nil {
		t.Fatal(err)
	}
	claimedUID := claimed.UID

	// Rebuild bumps the node digest; reconcile must NOT reap the claimed pod.
	reg.AddTemplateWithDigest("node-a", tmpl, newD)
	if _, err := r.ReconcileHuskPodsForTest(ctx, pool, pool.Spec.Template); err != nil {
		t.Fatalf("reconcileHuskPods (after rebuild): %v", err)
	}
	stillThere := false
	for _, p := range listHuskPods(t, c, poolName) {
		if p.UID == claimedUID && p.DeletionTimestamp == nil {
			stillThere = true
		}
	}
	if !stillThere {
		t.Errorf("claimed husk pod with a stale digest was reaped; it must be left alone")
	}
}

// A fallback pod (no node digest known, so no stamp) is never treated as stale.
func TestFallbackHuskPodWithoutDigestNotReaped(t *testing.T) {
	c := k8sClient
	const poolName = "fallback-pool"
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: poolName, Namespace: "default"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}
	if err := c.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, p := range listHuskPods(t, c, poolName) {
			_ = c.Delete(ctx, &p)
		}
		_ = c.Delete(ctx, pool)
	})
	// Registry with NO snapshot holder: pods take the fallback path (no stamp).
	reg := controller.NewNodeRegistry()
	r := &controller.SandboxPoolReconciler{
		Client: c, NodeRegistry: reg, EnableHuskPods: true,
		HuskStubImage: "mitos-husk-stub:test", KVMResourceName: "mitos.run/kvm",
	}
	if _, err := r.ReconcileHuskPodsForTest(ctx, pool, pool.Spec.Template); err != nil {
		t.Fatalf("reconcileHuskPods (build): %v", err)
	}
	pods := listHuskPods(t, c, poolName)
	if len(pods) != 1 {
		t.Fatalf("want 1 fallback pod, got %d", len(pods))
	}
	firstUID := pods[0].UID
	if got := pods[0].Annotations["mitos.run/template-digest"]; got != "" {
		t.Fatalf("fallback pod should carry no digest stamp, got %q", got)
	}
	if _, err := r.ReconcileHuskPodsForTest(ctx, pool, pool.Spec.Template); err != nil {
		t.Fatalf("reconcileHuskPods (steady): %v", err)
	}
	pods = listHuskPods(t, c, poolName)
	if len(pods) != 1 || pods[0].UID != firstUID {
		t.Errorf("fallback pod churned: want same UID %s, got %+v", firstUID, pods)
	}
}
```

- [ ] **Step 7: Run the full stale-digest suite**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ -run 'HuskPod|StaleDigest|Reaped|Fallback|Claimed' -count=1`
Expected: PASS (all stamp + reap + guard tests).

- [ ] **Step 8: Lint (both invocations)**

Run: `golangci-lint run --timeout=5m ./internal/controller/... && GOOS=linux golangci-lint run --timeout=5m ./internal/controller/...`
Expected: clean.

- [ ] **Step 9: Commit**

```bash
git add internal/controller/huskpod.go internal/controller/husk_stale_digest_test.go
git commit -s -m "fix(controller): reap warm husk pods whose snapshot digest went stale on rebuild (#461)

A same-name pool rebuild overwrites the templateID snapshot mem in place with a
new digest, but existing warm husk pods kept the old baked digest and
CrashLoopBackOff verifying new mem against the old manifest. Reap dormant pods
whose stamped digest no longer matches their node's current recorded digest so
the deficit logic refills them against the fresh snapshot. Claimed pods are never
reaped.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

### Task 6: #461 doc note

**Files:**
- Modify: `docs/husk-pods.md` (note the stale-digest reap behavior on rebuild)

- [ ] **Step 1: Add a short subsection to `docs/husk-pods.md`** (under the warm-pool reconcile / digest section; grep for `expected-digest` or `per-node digest` to find the spot)

```
### Snapshot rebuild under the same pool name (#461)

A template snapshot is stored by templateID (the pool name), so rebuilding a pool
under the same name overwrites its mem in place and produces a new content-
addressed digest. A warm husk pod records the digest and node it verifies against
(annotations mitos.run/template-digest and mitos.run/snapshot-node). On reconcile
the controller reaps any DORMANT pod whose stamped digest no longer matches its
node's current recorded digest, and the warm-pool deficit logic refills the slot
against the fresh snapshot. Claimed (activating or active) pods are never reaped.
This does not trigger the rebuild itself (a content change re-triggering a build
is #475); it ensures that once a rebuild has happened, warm husks converge instead
of CrashLoopBackOff on a stale digest.
```

- [ ] **Step 2: Verify no dashes**

Run: `! grep -nP "[\x{2013}\x{2014}]" docs/husk-pods.md`
Expected: no output.

- [ ] **Step 3: Commit**

```bash
git add docs/husk-pods.md
git commit -s -m "docs(husk-pods): document stale-digest reap on same-name pool rebuild (#461)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Final verification

- [ ] **Guest agent full unit tests:** `cd guest/agent-rs && cargo test --lib && cargo clippy --all-targets -- -D warnings`
- [ ] **Controller envtest:** `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ -count=1`
- [ ] **Go build + vet:** `go build ./... && go vet ./internal/controller/...`
- [ ] **Lint both:** `golangci-lint run --timeout=5m && GOOS=linux golangci-lint run --timeout=5m`
- [ ] **No dashes anywhere in the diff:** `git diff main --unified=0 | grep -nP "^\+.*[\x{2013}\x{2014}]"` returns nothing.
- [ ] Open the PR referencing #467 and #461; note that #461's final acceptance (large running-workload snapshot verifies and forks) needs the maintainer's live KVM run, which CI cannot perform here.

## Self-review notes

- Spec coverage: #467 helper (Task 1), gate + test updates (Task 2), docs + threat-model (Task 3); #461 stamp (Task 4), reap + guards (Task 5), docs (Task 6). All spec sections mapped.
- Verification boundary for #461 (live KVM) is restated in the final step, matching the spec's honesty note.
- Type/name consistency: annotation keys `mitos.run/template-digest` / `mitos.run/snapshot-node` used identically in stamp, helper, and tests; `huskPodHasStaleDigest` and `TemplateDigestOnNode` signatures match their definitions.
