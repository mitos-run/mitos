package controller_test

// Envtest coverage for the MultiVMFork routing (L1.7a): a hosted fork child routed
// into an ADDITIONAL VM spawned INSIDE the source pod (the spawn-vm control op)
// instead of a brand-new child pod, behind the controller MultiVMFork flag
// (default OFF).
//
// The routing takes effect ONLY when the flag is on AND the source pod is multi-VM
// capable (its stub runs --multi-vm, recorded by the mitos.run/multi-vm pod label).
// With the flag off, or a non-capable source, the fork keeps the byte-for-byte
// new-pod path. A spawn failure never wedges the child: the slot stays not-ready
// with the cause logged and the reconcile requeues, and a later successful spawn
// converges.
//
// envtest has no kubelet, so the source pod is forced Running+Ready and the
// fork-snapshot / activate / spawn-vm transports run through the suite's swappable
// fakes.

import (
	"context"
	"crypto/tls"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/controller"
	"mitos.run/mitos/internal/husk"
)

// multiVMSource stamps the mitos.run/multi-vm capability label so the source pod is
// a spawn-vm candidate.
func multiVMSource(p *corev1.Pod) {
	if p.Labels == nil {
		p.Labels = map[string]string{}
	}
	p.Labels[controller.HuskMultiVMLabel] = "true"
}

// TestMultiVMForkRoutesToSourcePodWhenEnabled proves the core L1.7a wiring: with
// the flag ON and a multi-VM-capable source, each fork child is spawned as an
// additional VM INSIDE the source pod (SpawnVMOnHusk), NO new child pod is created,
// and the recorded child carries status.Pod = the source pod, status.VMID = the
// spawned vmID, and status.Node = the source node.
func TestMultiVMForkRoutesToSourcePodWhenEnabled(t *testing.T) {
	poolName := uniqueName("pool-mvm-on")
	srcClaimName := uniqueName("src-mvm-on")
	forkName := uniqueName("mvm-on")

	srcPod := makeDormantHuskPod(t, poolName, "10.0.8.1", multiVMSource)
	makeForkSourceClaim(t, srcClaimName, poolName, srcPod)

	setForkSnapshotter(func(_ context.Context, _ string, _ *tls.Config, req husk.ForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
		return husk.ForkSnapshotResult{OK: true, SnapshotDir: req.SnapshotDir}, nil
	})
	t.Cleanup(func() { setForkSnapshotter(nil) })
	// A fork activator that fails loud: if the routing wrongly took the new-pod
	// path, the child would go through activate and never reach Ready, so the test
	// would fail instead of passing on the wrong path.
	setForkActivator(func(_ context.Context, _ string, _ *tls.Config, _ husk.ActivateRequest) (husk.ActivateResult, error) {
		return husk.ActivateResult{OK: false, Error: "activate must not be called on the spawn-in-source-pod path"}, nil
	})
	t.Cleanup(func() { setForkActivator(nil) })

	var spawnCalls int32
	var gotVMIDs sync.Map
	setForkVMSpawner(func(_ context.Context, _ string, _ *tls.Config, req husk.SpawnVMRequest) (husk.SpawnVMResult, error) {
		atomic.AddInt32(&spawnCalls, 1)
		gotVMIDs.Store(req.VMID, req.Activate.Token)
		return husk.SpawnVMResult{OK: true, VMID: req.VMID, VsockPath: "/run/husk/" + req.VMID + ".sock"}, nil
	})
	t.Cleanup(func() { setForkVMSpawner(nil) })

	setForkMultiVM(true)
	t.Cleanup(func() { setForkMultiVM(false) })

	fork := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      forkName,
			Namespace: "default",
			Labels:    map[string]string{controller.HuskForkTestLabel: "true"},
		},
		Spec: v1.SandboxSpec{Source: v1.SandboxSource{FromSandbox: &v1.FromSandboxSource{Name: srcClaimName}}, Replicas: 2},
	}
	if err := k8sClient.Create(ctx, fork); err != nil {
		t.Fatalf("create fork: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, fork) })

	waitUntilForkReady(t, 15*time.Second, func() bool {
		var got v1.Sandbox
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: forkName, Namespace: "default"}, &got); err != nil {
			return false
		}
		return got.Status.ReadyReplicas == 2
	})

	// No new child pod was created: the children live inside the source pod.
	var pods corev1.PodList
	if err := k8sClient.List(ctx, &pods, listForkChildren(forkName)); err != nil {
		t.Fatalf("list children: %v", err)
	}
	if len(pods.Items) != 0 {
		t.Fatalf("MultiVMFork must not create child pods; got %d", len(pods.Items))
	}
	if got := atomic.LoadInt32(&spawnCalls); got < 2 {
		t.Fatalf("expected at least 2 spawn-vm calls for Replicas=2, got %d", got)
	}

	var got v1.Sandbox
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: forkName, Namespace: "default"}, &got); err != nil {
		t.Fatalf("get fork: %v", err)
	}
	if len(got.Status.Children) != 2 {
		t.Fatalf("expected 2 recorded children, got %d", len(got.Status.Children))
	}
	for _, c := range got.Status.Children {
		if c.Pod != srcPod.Name {
			t.Errorf("child %s status.Pod = %q, want the source pod %q", c.Name, c.Pod, srcPod.Name)
		}
		if c.VMID == "" {
			t.Errorf("child %s status.VMID is empty, want the spawned vmID", c.Name)
		}
		if c.Node != srcPod.Spec.NodeName {
			t.Errorf("child %s status.Node = %q, want the source node %q", c.Name, c.Node, srcPod.Spec.NodeName)
		}
		if _, ok := gotVMIDs.Load(c.VMID); !ok {
			t.Errorf("child %s status.VMID %q was never passed to spawn-vm", c.Name, c.VMID)
		}
	}
}

// TestMultiVMForkOffCreatesNewChildPod proves the default-OFF path is unchanged:
// with the flag off, a fork still creates a new child pod and never calls the
// spawn-vm seam, even though the source is multi-VM capable.
func TestMultiVMForkOffCreatesNewChildPod(t *testing.T) {
	poolName := uniqueName("pool-mvm-off")
	srcClaimName := uniqueName("src-mvm-off")
	forkName := uniqueName("mvm-off")

	srcPod := makeDormantHuskPod(t, poolName, "10.0.8.2", multiVMSource)
	makeForkSourceClaim(t, srcClaimName, poolName, srcPod)

	setForkSnapshotter(func(_ context.Context, _ string, _ *tls.Config, req husk.ForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
		return husk.ForkSnapshotResult{OK: true, SnapshotDir: req.SnapshotDir}, nil
	})
	t.Cleanup(func() { setForkSnapshotter(nil) })
	setForkActivator(func(_ context.Context, _ string, _ *tls.Config, _ husk.ActivateRequest) (husk.ActivateResult, error) {
		return husk.ActivateResult{OK: true, VsockPath: "/run/husk/vsock.sock"}, nil
	})
	t.Cleanup(func() { setForkActivator(nil) })

	var spawnCalls int32
	setForkVMSpawner(func(_ context.Context, _ string, _ *tls.Config, _ husk.SpawnVMRequest) (husk.SpawnVMResult, error) {
		atomic.AddInt32(&spawnCalls, 1)
		return husk.SpawnVMResult{OK: false, Error: "spawn-vm must not be called with the flag off"}, nil
	})
	t.Cleanup(func() { setForkVMSpawner(nil) })

	// Flag OFF (the default). Assert explicitly rather than rely on the default.
	setForkMultiVM(false)

	fork := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      forkName,
			Namespace: "default",
			Labels:    map[string]string{controller.HuskForkTestLabel: "true"},
		},
		Spec: v1.SandboxSpec{Source: v1.SandboxSource{FromSandbox: &v1.FromSandboxSource{Name: srcClaimName}}, Replicas: 1},
	}
	if err := k8sClient.Create(ctx, fork); err != nil {
		t.Fatalf("create fork: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, fork) })

	waitUntilForkReady(t, 15*time.Second, func() bool {
		var p corev1.PodList
		_ = k8sClient.List(ctx, &p, listForkChildren(forkName))
		for i := range p.Items {
			forceHuskPodReady(t, &p.Items[i])
		}
		var got v1.Sandbox
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: forkName, Namespace: "default"}, &got); err != nil {
			return false
		}
		return got.Status.ReadyReplicas == 1
	})

	var pods corev1.PodList
	if err := k8sClient.List(ctx, &pods, listForkChildren(forkName)); err != nil {
		t.Fatalf("list children: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("flag-off fork must create exactly 1 child pod, got %d", len(pods.Items))
	}
	if got := atomic.LoadInt32(&spawnCalls); got != 0 {
		t.Fatalf("spawn-vm must not be called with the flag off; got %d calls", got)
	}

	var got v1.Sandbox
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: forkName, Namespace: "default"}, &got); err != nil {
		t.Fatalf("get fork: %v", err)
	}
	// The unchanged path records the child pod as SandboxID and leaves Pod/VMID empty.
	if len(got.Status.Children) != 1 {
		t.Fatalf("expected 1 recorded child, got %d", len(got.Status.Children))
	}
	if got.Status.Children[0].Pod != "" || got.Status.Children[0].VMID != "" {
		t.Errorf("flag-off child must leave status.Pod/VMID empty, got pod=%q vmId=%q", got.Status.Children[0].Pod, got.Status.Children[0].VMID)
	}
}

// TestMultiVMForkNonMultiVMSourceFallsBack proves the capability gate: with the
// flag ON but a source pod that is NOT multi-VM capable (no mitos.run/multi-vm
// label), the fork silently falls back to the new-pod path and never calls
// spawn-vm.
func TestMultiVMForkNonMultiVMSourceFallsBack(t *testing.T) {
	poolName := uniqueName("pool-mvm-nc")
	srcClaimName := uniqueName("src-mvm-nc")
	forkName := uniqueName("mvm-nc")

	// No multiVMSource mutator: the source stub is single-VM.
	srcPod := makeDormantHuskPod(t, poolName, "10.0.8.3")
	makeForkSourceClaim(t, srcClaimName, poolName, srcPod)

	setForkSnapshotter(func(_ context.Context, _ string, _ *tls.Config, req husk.ForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
		return husk.ForkSnapshotResult{OK: true, SnapshotDir: req.SnapshotDir}, nil
	})
	t.Cleanup(func() { setForkSnapshotter(nil) })
	setForkActivator(func(_ context.Context, _ string, _ *tls.Config, _ husk.ActivateRequest) (husk.ActivateResult, error) {
		return husk.ActivateResult{OK: true, VsockPath: "/run/husk/vsock.sock"}, nil
	})
	t.Cleanup(func() { setForkActivator(nil) })

	var spawnCalls int32
	setForkVMSpawner(func(_ context.Context, _ string, _ *tls.Config, _ husk.SpawnVMRequest) (husk.SpawnVMResult, error) {
		atomic.AddInt32(&spawnCalls, 1)
		return husk.SpawnVMResult{OK: false, Error: "spawn-vm must not be called for a non-multi-vm source"}, nil
	})
	t.Cleanup(func() { setForkVMSpawner(nil) })

	setForkMultiVM(true)
	t.Cleanup(func() { setForkMultiVM(false) })

	fork := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      forkName,
			Namespace: "default",
			Labels:    map[string]string{controller.HuskForkTestLabel: "true"},
		},
		Spec: v1.SandboxSpec{Source: v1.SandboxSource{FromSandbox: &v1.FromSandboxSource{Name: srcClaimName}}, Replicas: 1},
	}
	if err := k8sClient.Create(ctx, fork); err != nil {
		t.Fatalf("create fork: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, fork) })

	waitUntilForkReady(t, 15*time.Second, func() bool {
		var p corev1.PodList
		_ = k8sClient.List(ctx, &p, listForkChildren(forkName))
		for i := range p.Items {
			forceHuskPodReady(t, &p.Items[i])
		}
		var got v1.Sandbox
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: forkName, Namespace: "default"}, &got); err != nil {
			return false
		}
		return got.Status.ReadyReplicas == 1
	})

	var pods corev1.PodList
	if err := k8sClient.List(ctx, &pods, listForkChildren(forkName)); err != nil {
		t.Fatalf("list children: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("non-multi-vm source must fall back to a new child pod, got %d pods", len(pods.Items))
	}
	if got := atomic.LoadInt32(&spawnCalls); got != 0 {
		t.Fatalf("spawn-vm must not be called for a non-multi-vm source; got %d calls", got)
	}
}

// TestMultiVMForkSpawnErrorDoesNotWedge proves a spawn failure is never a silent
// hang: while the spawn-vm seam fails, the child stays not-ready (ReadyReplicas
// stays 0) and NO new child pod is silently created; once the seam recovers, the
// fork converges. This exercises the "fail toward a clear pending, never a silent
// hang" requirement.
func TestMultiVMForkSpawnErrorDoesNotWedge(t *testing.T) {
	poolName := uniqueName("pool-mvm-err")
	srcClaimName := uniqueName("src-mvm-err")
	forkName := uniqueName("mvm-err")

	srcPod := makeDormantHuskPod(t, poolName, "10.0.8.4", multiVMSource)
	makeForkSourceClaim(t, srcClaimName, poolName, srcPod)

	setForkSnapshotter(func(_ context.Context, _ string, _ *tls.Config, req husk.ForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
		return husk.ForkSnapshotResult{OK: true, SnapshotDir: req.SnapshotDir}, nil
	})
	t.Cleanup(func() { setForkSnapshotter(nil) })
	setForkActivator(func(_ context.Context, _ string, _ *tls.Config, _ husk.ActivateRequest) (husk.ActivateResult, error) {
		return husk.ActivateResult{OK: false, Error: "activate must not be called on the spawn path"}, nil
	})
	t.Cleanup(func() { setForkActivator(nil) })

	var failing atomic.Bool
	failing.Store(true)
	setForkVMSpawner(func(_ context.Context, _ string, _ *tls.Config, req husk.SpawnVMRequest) (husk.SpawnVMResult, error) {
		if failing.Load() {
			return husk.SpawnVMResult{OK: false, VMID: req.VMID, Error: "spawn refused: transient"}, nil
		}
		return husk.SpawnVMResult{OK: true, VMID: req.VMID, VsockPath: "/run/husk/" + req.VMID + ".sock"}, nil
	})
	t.Cleanup(func() { setForkVMSpawner(nil) })

	setForkMultiVM(true)
	t.Cleanup(func() { setForkMultiVM(false) })

	fork := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      forkName,
			Namespace: "default",
			Labels:    map[string]string{controller.HuskForkTestLabel: "true"},
		},
		Spec: v1.SandboxSpec{Source: v1.SandboxSource{FromSandbox: &v1.FromSandboxSource{Name: srcClaimName}}, Replicas: 1},
	}
	if err := k8sClient.Create(ctx, fork); err != nil {
		t.Fatalf("create fork: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, fork) })

	// Let several requeue passes elapse while spawn fails: the child must stay
	// not-ready and NO child pod may be silently created (the fork snapshot ran, so
	// the loop is being driven).
	time.Sleep(3 * time.Second)

	var got v1.Sandbox
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: forkName, Namespace: "default"}, &got); err != nil {
		t.Fatalf("get fork: %v", err)
	}
	if got.Status.ReadyReplicas != 0 {
		t.Fatalf("child must stay not-ready while spawn fails, got ReadyReplicas=%d", got.Status.ReadyReplicas)
	}
	var pods corev1.PodList
	if err := k8sClient.List(ctx, &pods, listForkChildren(forkName)); err != nil {
		t.Fatalf("list children: %v", err)
	}
	if len(pods.Items) != 0 {
		t.Fatalf("a failing spawn must not silently create a child pod; got %d", len(pods.Items))
	}

	// Recover the spawn seam: the fork must now converge (a clean retry, not a wedge).
	failing.Store(false)
	waitUntilForkReady(t, 15*time.Second, func() bool {
		var g v1.Sandbox
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: forkName, Namespace: "default"}, &g); err != nil {
			return false
		}
		return g.Status.ReadyReplicas == 1
	})
}

// TestMultiVMForkSpillsPastCapToNewPods proves the conservative L1.7a co-location
// cap: with the flag on and a multi-VM-capable source, the first
// MaxCoLocatedForkVMsPerPod children are spawned in the source pod and every
// child beyond the cap SPILLS to a new child pod (the stand-in for the real
// per-pod memory accounting deferred to L1.7b).
func TestMultiVMForkSpillsPastCapToNewPods(t *testing.T) {
	poolName := uniqueName("pool-mvm-cap")
	srcClaimName := uniqueName("src-mvm-cap")
	forkName := uniqueName("mvm-cap")

	srcPod := makeDormantHuskPod(t, poolName, "10.0.8.5", multiVMSource)
	makeForkSourceClaim(t, srcClaimName, poolName, srcPod)

	setForkSnapshotter(func(_ context.Context, _ string, _ *tls.Config, req husk.ForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
		return husk.ForkSnapshotResult{OK: true, SnapshotDir: req.SnapshotDir}, nil
	})
	t.Cleanup(func() { setForkSnapshotter(nil) })
	setForkActivator(func(_ context.Context, _ string, _ *tls.Config, _ husk.ActivateRequest) (husk.ActivateResult, error) {
		return husk.ActivateResult{OK: true, VsockPath: "/run/husk/vsock.sock"}, nil
	})
	t.Cleanup(func() { setForkActivator(nil) })

	var spawnCalls int32
	setForkVMSpawner(func(_ context.Context, _ string, _ *tls.Config, req husk.SpawnVMRequest) (husk.SpawnVMResult, error) {
		atomic.AddInt32(&spawnCalls, 1)
		return husk.SpawnVMResult{OK: true, VMID: req.VMID, VsockPath: "/run/husk/" + req.VMID + ".sock"}, nil
	})
	t.Cleanup(func() { setForkVMSpawner(nil) })

	setForkMultiVM(true)
	t.Cleanup(func() { setForkMultiVM(false) })

	capN := int32(controller.MaxCoLocatedForkVMsPerPod)
	replicas := capN + 2

	fork := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      forkName,
			Namespace: "default",
			Labels:    map[string]string{controller.HuskForkTestLabel: "true"},
		},
		Spec: v1.SandboxSpec{Source: v1.SandboxSource{FromSandbox: &v1.FromSandboxSource{Name: srcClaimName}}, Replicas: replicas},
	}
	if err := k8sClient.Create(ctx, fork); err != nil {
		t.Fatalf("create fork: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, fork) })

	waitUntilForkReady(t, 20*time.Second, func() bool {
		var p corev1.PodList
		_ = k8sClient.List(ctx, &p, listForkChildren(forkName))
		for i := range p.Items {
			forceHuskPodReady(t, &p.Items[i])
		}
		var g v1.Sandbox
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: forkName, Namespace: "default"}, &g); err != nil {
			return false
		}
		return g.Status.ReadyReplicas == replicas
	})

	// Exactly the over-cap children spilled to new pods; the capped children stay
	// in the source pod.
	var pods corev1.PodList
	if err := k8sClient.List(ctx, &pods, listForkChildren(forkName)); err != nil {
		t.Fatalf("list children: %v", err)
	}
	spill := int(replicas - capN)
	if len(pods.Items) != spill {
		t.Fatalf("expected %d spilled child pods past the cap, got %d", spill, len(pods.Items))
	}
	if got := atomic.LoadInt32(&spawnCalls); got < capN {
		t.Fatalf("expected at least %d spawn-vm calls (the co-located children), got %d", capN, got)
	}
}
