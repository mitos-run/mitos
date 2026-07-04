package controller_test

// Envtest coverage for issue #698: a fork whose SOURCE sandbox reaches a
// terminal phase (Terminated or Failed) or disappears must stop its FAN-OUT
// terminally instead of parking in the 1 second requeue loop forever. The
// SOURCE of truth for what stops versus what survives: a child once activated
// is an INDEPENDENT sandbox (it holds its own copy of the source memory), so
// only the never-activated pending child pods are deleted; activated children
// and their Status.Children entries survive. With no survivors the fork is
// terminal all the way: phase Failed, FinishedAt for the GC TTL pass, a
// SourceTerminated condition mirrored on Ready=False (so the gateway's
// failureReason surfaces the cause). With survivors the phase stays
// non-terminal (the GC TTL pass must never reap the fork object out from
// under its running children). In both shapes there are NO further requeues
// (repeated reconciles are no-ops). A source that is merely not-yet-Ready
// (Pending) must still be waited for, never falsely failed.

import (
	"context"
	"crypto/tls"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/controller"
	"mitos.run/mitos/internal/husk"
)

// installOKForkTransports installs fork-snapshot and activate fakes that always
// succeed, so a fork under test progresses to child-pod creation.
func installOKForkTransports(t *testing.T) {
	t.Helper()
	setForkSnapshotter(func(_ context.Context, _ string, _ *tls.Config, req husk.ForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
		return husk.ForkSnapshotResult{OK: true, SnapshotDir: req.SnapshotDir}, nil
	})
	t.Cleanup(func() { setForkSnapshotter(nil) })
	setForkActivator(func(_ context.Context, _ string, _ *tls.Config, _ husk.ActivateRequest) (husk.ActivateResult, error) {
		return husk.ActivateResult{OK: true, VsockPath: "/run/husk/vsock.sock"}, nil
	})
	t.Cleanup(func() { setForkActivator(nil) })
}

// getFork fetches the named fork Sandbox from the default namespace.
func getFork(t *testing.T, name string) *v1.Sandbox {
	t.Helper()
	var got v1.Sandbox
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, &got); err != nil {
		t.Fatalf("get fork %s: %v", name, err)
	}
	return &got
}

// forkIsTerminallyFailed reports whether the fork carries a True
// SourceTerminated condition with the given reason and has phase Failed.
func forkIsTerminallyFailed(name, reason string) bool {
	var got v1.Sandbox
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, &got); err != nil {
		return false
	}
	c := meta.FindStatusCondition(got.Status.Conditions, "SourceTerminated")
	return c != nil && c.Status == metav1.ConditionTrue && c.Reason == reason &&
		got.Status.Phase == v1.SandboxFailed
}

// TestHuskForkSourceTerminatedFailsTerminallyAndReapsChildren drives the exact
// #698 shape: a husk fork mid fan-out (children created but never Ready) whose
// source then reaches the Terminated phase (the #697 lifetime-expiry reap).
// The fork must fail terminally, its child pods must be deleted (they are
// owner-ref'd to the FORK, not the pool, so nothing else releases their KVM
// and memory requests), and repeated reconciles must be no-ops (no status
// churn: before the fix every pass re-stamped CheckpointTime).
func TestHuskForkSourceTerminatedFailsTerminallyAndReapsChildren(t *testing.T) {
	poolName := uniqueName("pool-srcterm")
	srcName := uniqueName("src-term")
	forkName := uniqueName("srcterm")
	srcPod := makeDormantHuskPod(t, poolName, "10.0.3.5")
	makeForkSourceClaim(t, srcName, poolName, srcPod)
	installOKForkTransports(t)

	ttlZero := int32(0)
	fork := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      forkName,
			Namespace: "default",
			Labels:    map[string]string{controller.HuskForkTestLabel: "true"},
		},
		Spec: v1.SandboxSpec{
			Source:   v1.SandboxSource{FromSandbox: &v1.FromSandboxSource{Name: srcName}},
			Replicas: 2,
			Lifetime: &v1.SandboxLifetime{TTLSecondsAfterFinished: &ttlZero},
		},
	}
	if err := k8sClient.Create(ctx, fork); err != nil {
		t.Fatalf("create fork: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, fork) })

	// Wait until both child pods exist. They are deliberately never forced
	// Ready, so the fork is parked mid fan-out when the source dies.
	waitUntilForkReady(t, 15*time.Second, func() bool {
		var pods corev1.PodList
		_ = k8sClient.List(ctx, &pods, listForkChildren(forkName))
		return len(pods.Items) == 2
	})

	// The source reaches its terminal phase (lifetime expiry, kill, ...).
	updateSandboxStatusWithRetry(t, srcName, "default", func(sb *v1.Sandbox) {
		sb.Status.Phase = v1.SandboxTerminated
	})

	// The fork must converge to the terminal failure, not park.
	waitUntilForkReady(t, 15*time.Second, func() bool {
		return forkIsTerminallyFailed(forkName, "SourceTerminated")
	})

	got := getFork(t, forkName)
	src := meta.FindStatusCondition(got.Status.Conditions, "SourceTerminated")
	if !strings.Contains(src.Message, srcName) {
		t.Fatalf("SourceTerminated message must name the source sandbox for actionability; got %q", src.Message)
	}
	ready := meta.FindStatusCondition(got.Status.Conditions, "Ready")
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "SourceTerminated" {
		t.Fatalf("Ready condition must be False/SourceTerminated so the gateway surfaces the cause; got %+v", ready)
	}
	if got.Status.FinishedAt == nil {
		t.Fatalf("FinishedAt must be stamped so the GC TTL pass reaps the failed fork")
	}

	// The child pods must be deleted so their resources return to the scheduler
	// (envtest has no kubelet, so a deleted pod lingers Terminating: assert the
	// DeletionTimestamp, the same convention the husk reap tests use).
	waitUntilForkReady(t, 15*time.Second, func() bool {
		var pods corev1.PodList
		if err := k8sClient.List(ctx, &pods, listForkChildren(forkName)); err != nil {
			return false
		}
		for i := range pods.Items {
			if pods.Items[i].DeletionTimestamp == nil {
				return false
			}
		}
		return true
	})

	// Idempotency and no-requeue: a terminally failed fork must not churn. The
	// pre-fix loop re-stamped Status.CheckpointTime every second, so a stable
	// resourceVersion across this window proves repeated reconciles are no-ops.
	rv := getFork(t, forkName).ResourceVersion
	time.Sleep(2 * time.Second)
	if after := getFork(t, forkName).ResourceVersion; after != rv {
		t.Fatalf("terminally failed fork must be reconcile-stable; resourceVersion moved %s -> %s", rv, after)
	}

	// With NO surviving children the fork is fully terminal (Failed +
	// FinishedAt) and, with ttlSecondsAfterFinished=0, the GC TTL pass reaps
	// the object instead of leaking it in etcd forever.
	gc := &controller.GarbageCollector{Client: k8sClient, Registry: testRegistry, EnableHuskPods: true}
	gc.RunOnce(ctx)
	var reaped v1.Sandbox
	err := k8sClient.Get(ctx, types.NamespacedName{Name: forkName, Namespace: "default"}, &reaped)
	if err == nil && reaped.DeletionTimestamp == nil {
		t.Fatalf("GC TTL pass must reap a fully terminal fork with ttlSecondsAfterFinished=0")
	}
}

// TestHuskForkSourceTerminatedPartialFanOutKeepsActiveChildren is the
// partial-fan-out semantic: a child, ONCE ACTIVATED, is an independent sandbox
// (its memory was copied at fork time), so source death stops the FAN-OUT,
// never the born children. With 2 of 3 children active when the source
// terminates, the two active child pods must survive untouched, only the
// never-activated pending pod is deleted, ReadyReplicas stays 2 and
// Status.Children keeps both entries, the SourceTerminated condition names the
// stopped fan-out (2 of 3), and the fork must NOT be stamped Failed or
// FinishedAt: a Failed+FinishedAt fork is GC TTL-eligible, and deleting the
// fork object would take the surviving children down through their owner refs.
// A real GC pass is driven to prove the survivors outlive it.
func TestHuskForkSourceTerminatedPartialFanOutKeepsActiveChildren(t *testing.T) {
	poolName := uniqueName("pool-partial")
	srcName := uniqueName("src-partial")
	forkName := uniqueName("partial")
	srcPod := makeDormantHuskPod(t, poolName, "10.0.4.5")
	makeForkSourceClaim(t, srcName, poolName, srcPod)
	installOKForkTransports(t)

	ttlZero := int32(0)
	fork := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      forkName,
			Namespace: "default",
			Labels:    map[string]string{controller.HuskForkTestLabel: "true"},
		},
		Spec: v1.SandboxSpec{
			Source:   v1.SandboxSource{FromSandbox: &v1.FromSandboxSource{Name: srcName}},
			Replicas: 3,
			Lifetime: &v1.SandboxLifetime{TTLSecondsAfterFinished: &ttlZero},
		},
	}
	if err := k8sClient.Create(ctx, fork); err != nil {
		t.Fatalf("create fork: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, fork) })

	// Drive EXACTLY slots 0 and 1 to Ready (activated); slot 2 stays pending.
	activeName := map[string]bool{forkName + "-fork-0": true, forkName + "-fork-1": true}
	waitUntilForkReady(t, 15*time.Second, func() bool {
		var pods corev1.PodList
		_ = k8sClient.List(ctx, &pods, listForkChildren(forkName))
		for i := range pods.Items {
			if activeName[pods.Items[i].Name] {
				forceHuskPodReady(t, &pods.Items[i])
			}
		}
		if len(pods.Items) != 3 {
			return false
		}
		var got v1.Sandbox
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: forkName, Namespace: "default"}, &got); err != nil {
			return false
		}
		return got.Status.ReadyReplicas == 2
	})

	// The source reaches its terminal phase mid fan-out.
	updateSandboxStatusWithRetry(t, srcName, "default", func(sb *v1.Sandbox) {
		sb.Status.Phase = v1.SandboxTerminated
	})

	waitUntilForkReady(t, 15*time.Second, func() bool {
		var got v1.Sandbox
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: forkName, Namespace: "default"}, &got); err != nil {
			return false
		}
		return meta.IsStatusConditionTrue(got.Status.Conditions, "SourceTerminated")
	})

	got := getFork(t, forkName)
	if got.Status.ReadyReplicas != 2 || len(got.Status.Children) != 2 {
		t.Fatalf("active children must be kept: readyReplicas=%d children=%d, want 2/2",
			got.Status.ReadyReplicas, len(got.Status.Children))
	}
	if got.Status.Phase == v1.SandboxFailed {
		t.Fatalf("a fork with surviving children must not be stamped Failed (GC would reap it and its children)")
	}
	if got.Status.FinishedAt != nil {
		t.Fatalf("FinishedAt must not be stamped while children survive")
	}
	src := meta.FindStatusCondition(got.Status.Conditions, "SourceTerminated")
	if !strings.Contains(src.Message, "2 of 3") {
		t.Fatalf("SourceTerminated message must name the stopped fan-out (2 of 3); got %q", src.Message)
	}

	// The pending pod is released (deleted outright, or Terminating while the
	// apiserver waits out the grace period); the two ACTIVE children survive
	// untouched.
	waitUntilForkReady(t, 15*time.Second, func() bool {
		var pods corev1.PodList
		if err := k8sClient.List(ctx, &pods, listForkChildren(forkName)); err != nil {
			return false
		}
		activeAlive := 0
		pendingAlive := 0
		for i := range pods.Items {
			p := &pods.Items[i]
			if activeName[p.Name] {
				if p.DeletionTimestamp != nil {
					t.Fatalf("active child pod %s was deleted; activated children must outlive the source", p.Name)
				}
				activeAlive++
				continue
			}
			if p.DeletionTimestamp == nil {
				pendingAlive++
			}
		}
		return activeAlive == 2 && pendingAlive == 0
	})

	// A real GC pass must leave the fork and its survivors alone: the fork is
	// TTL-eligible-if-terminal (ttlSecondsAfterFinished=0), so this pass would
	// reap it if the terminal path had stamped Failed+FinishedAt. EnableHuskPods
	// matches the husk run mode (markNodeLost is a no-op there).
	gc := &controller.GarbageCollector{Client: k8sClient, Registry: testRegistry, EnableHuskPods: true}
	gc.RunOnce(ctx)

	after := getFork(t, forkName)
	if after.DeletionTimestamp != nil {
		t.Fatalf("GC must not reap a fork with surviving children")
	}
	var pods corev1.PodList
	if err := k8sClient.List(ctx, &pods, listForkChildren(forkName)); err != nil {
		t.Fatalf("list children after GC: %v", err)
	}
	for i := range pods.Items {
		if activeName[pods.Items[i].Name] && pods.Items[i].DeletionTimestamp != nil {
			t.Fatalf("active child pod %s deleted after GC pass", pods.Items[i].Name)
		}
	}
}

// TestHuskForkSourceGoneFailsTerminally covers the missing-source arm: a fork
// whose source object does not exist must fail terminally with SourceGone
// instead of error-requeueing forever (the pre-fix lines 81-82 behavior).
func TestHuskForkSourceGoneFailsTerminally(t *testing.T) {
	forkName := uniqueName("srcgone")
	missingSource := uniqueName("no-such-source")
	installOKForkTransports(t)

	fork := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      forkName,
			Namespace: "default",
			Labels:    map[string]string{controller.HuskForkTestLabel: "true"},
		},
		Spec: v1.SandboxSpec{Source: v1.SandboxSource{FromSandbox: &v1.FromSandboxSource{Name: missingSource}}, Replicas: 1},
	}
	if err := k8sClient.Create(ctx, fork); err != nil {
		t.Fatalf("create fork: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, fork) })

	waitUntilForkReady(t, 15*time.Second, func() bool {
		return forkIsTerminallyFailed(forkName, "SourceGone")
	})

	got := getFork(t, forkName)
	c := meta.FindStatusCondition(got.Status.Conditions, "SourceTerminated")
	if !strings.Contains(c.Message, missingSource) {
		t.Fatalf("SourceGone message must name the missing source; got %q", c.Message)
	}
	if got.Status.FinishedAt == nil {
		t.Fatalf("FinishedAt must be stamped on the SourceGone terminal failure")
	}
}

// TestRawForkSourceFailedFailsTerminally proves the terminal-source handling is
// shared with the raw-forkd fork path (no husk labels, the rawClaim reconciler)
// and covers the Failed source phase variant.
func TestRawForkSourceFailedFailsTerminally(t *testing.T) {
	srcName := uniqueName("src-rawfail")
	forkName := uniqueName("rawfail")

	src := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: srcName, Namespace: "default"},
		Spec:       v1.SandboxSpec{Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: uniqueName("pool-rawfail")}}},
	}
	if err := k8sClient.Create(ctx, src); err != nil {
		t.Fatalf("create source claim: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, src) })
	updateSandboxStatusWithRetry(t, srcName, "default", func(sb *v1.Sandbox) {
		sb.Status.Phase = v1.SandboxFailed
	})

	fork := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: forkName, Namespace: "default"},
		Spec:       v1.SandboxSpec{Source: v1.SandboxSource{FromSandbox: &v1.FromSandboxSource{Name: srcName}}, Replicas: 1},
	}
	if err := k8sClient.Create(ctx, fork); err != nil {
		t.Fatalf("create fork: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, fork) })

	waitUntilForkReady(t, 15*time.Second, func() bool {
		return forkIsTerminallyFailed(forkName, "SourceFailed")
	})
}

// TestForkWaitsWhileSourceOnlyPending is the no-false-terminal guard: a source
// that is merely not-yet-Ready (still starting) must keep the fork waiting;
// the terminal handling must never fire on a non-terminal phase.
func TestForkWaitsWhileSourceOnlyPending(t *testing.T) {
	srcName := uniqueName("src-pend")
	forkName := uniqueName("pendwait")

	src := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: srcName, Namespace: "default"},
		Spec:       v1.SandboxSpec{Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: uniqueName("pool-pend")}}},
	}
	if err := k8sClient.Create(ctx, src); err != nil {
		t.Fatalf("create source claim: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, src) })
	updateSandboxStatusWithRetry(t, srcName, "default", func(sb *v1.Sandbox) {
		sb.Status.Phase = v1.SandboxPending
	})

	fork := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: forkName, Namespace: "default"},
		Spec:       v1.SandboxSpec{Source: v1.SandboxSource{FromSandbox: &v1.FromSandboxSource{Name: srcName}}, Replicas: 1},
	}
	if err := k8sClient.Create(ctx, fork); err != nil {
		t.Fatalf("create fork: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, fork) })

	// Give the reconciler several passes; the fork must still be waiting.
	time.Sleep(3 * time.Second)

	got := getFork(t, forkName)
	if c := meta.FindStatusCondition(got.Status.Conditions, "SourceTerminated"); c != nil {
		t.Fatalf("a merely Pending source must not trigger the terminal failure; got condition %+v", c)
	}
	if got.Status.Phase == v1.SandboxFailed {
		t.Fatalf("a merely Pending source must not fail the fork; phase is %s", got.Status.Phase)
	}
}
