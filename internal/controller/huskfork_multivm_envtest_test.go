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
	"k8s.io/apimachinery/pkg/api/resource"
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

// withCoLocationBudget stamps the source husk pod's memory request (the honest
// per-VM guest RAM) and limit (the pod cgroup memory.max) so the per-pod
// co-location budget (guarantee A) admits floor(limit/req) - 1 co-located fork
// VMs, one slot reserved for the source VM already in the pod. The multi-VM tests
// that exercise co-location stamp a budget large enough for their replicas; the
// honest worst-case accounting otherwise co-locates NOTHING on a resource-free
// pod and every child spills to a new pod.
func withCoLocationBudget(reqMem, limitMem string) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		for i := range p.Spec.Containers {
			if p.Spec.Containers[i].Name != "husk-stub" {
				continue
			}
			p.Spec.Containers[i].Resources = corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(reqMem)},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(limitMem)},
			}
		}
	}
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

	// A generous per-pod budget (1280Mi limit / 128Mi per VM = 10 VMs, 9
	// co-locatable) so both Replicas co-locate into the source pod.
	srcPod := makeDormantHuskPod(t, poolName, "10.0.8.1", multiVMSource, withCoLocationBudget("128Mi", "1280Mi"))
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

	srcPod := makeDormantHuskPod(t, poolName, "10.0.8.4", multiVMSource, withCoLocationBudget("128Mi", "1280Mi"))
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

// TestMultiVMForkSpillsPastPodBudgetToNewPods proves the L1.7b per-pod MEMORY
// accounting (guarantee A): with the flag on and a multi-VM-capable source, fork
// children co-locate into the source pod ONLY up to the pod's memory budget
// (floor(memory.max / per-VM guest RAM) - 1, the source VM reserving one slot),
// and every child beyond that budget SPILLS to a new child pod so the fork never
// overcommits the pod. The source pod is sized 1024Mi limit / 256Mi per VM = 4
// VMs total, so exactly 3 children co-locate and the rest spill.
func TestMultiVMForkSpillsPastPodBudgetToNewPods(t *testing.T) {
	poolName := uniqueName("pool-mvm-cap")
	srcClaimName := uniqueName("src-mvm-cap")
	forkName := uniqueName("mvm-cap")

	// 1024Mi / 256Mi = 4 VMs total; one reserved for the source, so 3 co-locate.
	srcPod := makeDormantHuskPod(t, poolName, "10.0.8.5", multiVMSource, withCoLocationBudget("256Mi", "1024Mi"))
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

	const coLocated = int32(3)
	const spill = int32(2)
	replicas := coLocated + spill

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

	// Exactly the over-budget children spilled to new pods; the budgeted children
	// stay co-located in the source pod.
	var pods corev1.PodList
	if err := k8sClient.List(ctx, &pods, listForkChildren(forkName)); err != nil {
		t.Fatalf("list children: %v", err)
	}
	if len(pods.Items) != int(spill) {
		t.Fatalf("expected %d spilled child pods past the pod memory budget, got %d", spill, len(pods.Items))
	}
	if got := atomic.LoadInt32(&spawnCalls); got < coLocated {
		t.Fatalf("expected at least %d spawn-vm calls (the co-located children), got %d", coLocated, got)
	}
}

// TestMultiVMForkPodBudgetHoldsOneVMSpills proves the tight edge of guarantee A:
// a source pod whose memory budget holds only ONE VM (the honest default sizing,
// 768Mi limit / 512Mi per VM = 1 VM total) co-locates NO fork child, because the
// single VM slot is already the source. The one fork child SPILLS to a new pod and
// the spawn-vm seam is never called, so a second full guest is never packed into a
// pod that cannot hold it.
func TestMultiVMForkPodBudgetHoldsOneVMSpills(t *testing.T) {
	poolName := uniqueName("pool-mvm-1vm")
	srcClaimName := uniqueName("src-mvm-1vm")
	forkName := uniqueName("mvm-1vm")

	// 768Mi limit / 512Mi per VM = 1 VM total; the source takes it, so 0 co-locate.
	srcPod := makeDormantHuskPod(t, poolName, "10.0.8.10", multiVMSource, withCoLocationBudget("512Mi", "768Mi"))
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
		return husk.SpawnVMResult{OK: false, Error: "spawn-vm must not be called when the pod budget holds only the source VM"}, nil
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
		var g v1.Sandbox
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: forkName, Namespace: "default"}, &g); err != nil {
			return false
		}
		return g.Status.ReadyReplicas == 1
	})

	var pods corev1.PodList
	if err := k8sClient.List(ctx, &pods, listForkChildren(forkName)); err != nil {
		t.Fatalf("list children: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("a pod budget holding only the source VM must spill the child to a new pod, got %d pods", len(pods.Items))
	}
	if got := atomic.LoadInt32(&spawnCalls); got != 0 {
		t.Fatalf("spawn-vm must not be called when the pod budget holds only the source VM; got %d calls", got)
	}

	var got v1.Sandbox
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: forkName, Namespace: "default"}, &got); err != nil {
		t.Fatalf("get fork: %v", err)
	}
	// The spilled child took the new-pod path, so it carries no source-pod VM record.
	if len(got.Status.Children) != 1 {
		t.Fatalf("expected 1 recorded child, got %d", len(got.Status.Children))
	}
	if got.Status.Children[0].Pod != "" || got.Status.Children[0].VMID != "" {
		t.Errorf("spilled child must leave status.Pod/VMID empty, got pod=%q vmId=%q", got.Status.Children[0].Pod, got.Status.Children[0].VMID)
	}
}

// TestMultiVMForkFlagFlipOffDoesNotOrphanCoLocatedChild proves the CodeRabbit
// Major fix: a slot already spawned INSIDE the source pod is carried forward on a
// later reconcile even after --multi-vm-fork is flipped OFF (a controller
// restart with the flag off), and does NOT fall to the new-pod branch and create
// an orphan pod that status never references. Without hoisting the recorded-slot
// check above the routing branches, that flip would leak a KVM device slot.
func TestMultiVMForkFlagFlipOffDoesNotOrphanCoLocatedChild(t *testing.T) {
	poolName := uniqueName("pool-mvm-flip")
	srcClaimName := uniqueName("src-mvm-flip")
	forkName := uniqueName("mvm-flip")

	srcPod := makeDormantHuskPod(t, poolName, "10.0.8.9", multiVMSource, withCoLocationBudget("128Mi", "1280Mi"))
	makeForkSourceClaim(t, srcClaimName, poolName, srcPod)

	setForkSnapshotter(func(_ context.Context, _ string, _ *tls.Config, req husk.ForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
		return husk.ForkSnapshotResult{OK: true, SnapshotDir: req.SnapshotDir}, nil
	})
	t.Cleanup(func() { setForkSnapshotter(nil) })
	setForkActivator(func(_ context.Context, _ string, _ *tls.Config, _ husk.ActivateRequest) (husk.ActivateResult, error) {
		return husk.ActivateResult{OK: false, Error: "activate must not be called for an already-recorded co-located child"}, nil
	})
	t.Cleanup(func() { setForkActivator(nil) })
	setForkVMSpawner(func(_ context.Context, _ string, _ *tls.Config, req husk.SpawnVMRequest) (husk.SpawnVMResult, error) {
		return husk.SpawnVMResult{OK: true, VMID: req.VMID, VsockPath: "/run/husk/" + req.VMID + ".sock"}, nil
	})
	t.Cleanup(func() { setForkVMSpawner(nil) })

	// Phase 1: flag ON, the child co-locates in the source pod (no child pod).
	setForkMultiVM(true)
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
		var got v1.Sandbox
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: forkName, Namespace: "default"}, &got); err != nil {
			return false
		}
		return got.Status.ReadyReplicas == 1
	})
	var afterOn v1.Sandbox
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: forkName, Namespace: "default"}, &afterOn); err != nil {
		t.Fatalf("get fork: %v", err)
	}
	if len(afterOn.Status.Children) != 1 || afterOn.Status.Children[0].Pod != srcPod.Name {
		t.Fatalf("phase 1: child must be recorded in the source pod, got %+v", afterOn.Status.Children)
	}

	// Phase 2: flip the flag OFF and force a reconcile (annotation bump). The
	// already-co-located slot must be carried forward, NOT re-routed to a new pod.
	setForkMultiVM(false)
	var live v1.Sandbox
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: forkName, Namespace: "default"}, &live); err != nil {
		t.Fatalf("re-get fork: %v", err)
	}
	if live.Annotations == nil {
		live.Annotations = map[string]string{}
	}
	live.Annotations["test.mitos.run/rereconcile"] = "1"
	if err := k8sClient.Update(ctx, &live); err != nil {
		t.Fatalf("bump fork to force reconcile: %v", err)
	}

	// Give the reconcile loop time to run; assert NO child pod was ever created and
	// the child record still points at the source pod (never re-routed).
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		var pods corev1.PodList
		if err := k8sClient.List(ctx, &pods, listForkChildren(forkName)); err != nil {
			t.Fatalf("list children: %v", err)
		}
		if len(pods.Items) != 0 {
			t.Fatalf("flag-flip-off must NOT create an orphan child pod for an already-co-located slot; got %d", len(pods.Items))
		}
		time.Sleep(300 * time.Millisecond)
	}
	var afterFlip v1.Sandbox
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: forkName, Namespace: "default"}, &afterFlip); err != nil {
		t.Fatalf("get fork after flip: %v", err)
	}
	if len(afterFlip.Status.Children) != 1 || afterFlip.Status.Children[0].Pod != srcPod.Name || afterFlip.Status.Children[0].VMID == "" {
		t.Fatalf("after flip, the co-located child record must be unchanged (source pod + vmID), got %+v", afterFlip.Status.Children)
	}
}

// withMultiVMWarmPodSizing copies the memory REQUEST, LIMIT, and per-VM guest-RAM
// annotation that a REAL multi-VM warm husk pod carries (the production pod builder
// buildHuskPod with MultiVM on, default template memory) onto the source husk pod,
// so the co-location routing sees the exact resource shape a production all-multi-VM
// warm pool produces. This is the canary shape: before the fix, buildHuskPod left a
// multi-VM pod sized for ONE VM (limit = request + modest headroom), so
// coLocatedForkVMBudget floored to 0 and every fork spilled to the new-pod path; the
// fix reserves memory for co-located fork VMs up front so the budget is >= 1.
func withMultiVMWarmPodSizing(t *testing.T) func(*corev1.Pod) {
	t.Helper()
	poolR := &controller.SandboxPoolReconciler{MultiVM: true}
	pool := &v1.SandboxPool{ObjectMeta: metav1.ObjectMeta{Name: "mvm-sizing-ref", Namespace: "default"}}
	ref := poolR.BuildHuskPodForTest(pool, &v1.PoolTemplateSpec{}, controller.HuskPodOptions{MultiVM: true})
	var refResources corev1.ResourceRequirements
	for i := range ref.Spec.Containers {
		if ref.Spec.Containers[i].Name == "husk-stub" {
			refResources = *ref.Spec.Containers[i].Resources.DeepCopy()
		}
	}
	return func(p *corev1.Pod) {
		for i := range p.Spec.Containers {
			if p.Spec.Containers[i].Name == "husk-stub" {
				p.Spec.Containers[i].Resources = *refResources.DeepCopy()
			}
		}
		if p.Annotations == nil {
			p.Annotations = map[string]string{}
		}
		for k, v := range ref.Annotations {
			p.Annotations[k] = v
		}
	}
}

// TestMultiVMWarmPodReservesCoLocationBudget is the fast reproduction of the canary
// root cause: a multi-VM-capable warm husk pod, built by the SAME production pod
// builder that a --multi-vm-fork warm pool uses, must grant a co-location budget of
// at least one fork child. On origin/main the builder sizes a multi-VM pod for a
// SINGLE VM (memory limit = request + headroom), so coLocatedForkVMBudget floors to
// 0: NO child co-locates and every fork spills to the new-pod path (where the prod
// canary hit "re-get fork child pod not found"). A single-VM warm pod is unchanged:
// it still grants 0 (it never co-locates, spawnInSourcePod is false for it).
func TestMultiVMWarmPodReservesCoLocationBudget(t *testing.T) {
	poolR := &controller.SandboxPoolReconciler{MultiVM: true}
	pool := &v1.SandboxPool{ObjectMeta: metav1.ObjectMeta{Name: "mvm-budget", Namespace: "default"}}

	multiVMPod := poolR.BuildHuskPodForTest(pool, &v1.PoolTemplateSpec{}, controller.HuskPodOptions{MultiVM: true})
	if got := controller.CoLocatedForkVMBudgetForTest(multiVMPod); got < 1 {
		t.Fatalf("a multi-VM warm pod must reserve room for at least one co-located fork VM, got budget %d", got)
	}

	// The single-VM (default) pod is unchanged: no co-location, budget 0.
	singleVMPod := poolR.BuildHuskPodForTest(pool, &v1.PoolTemplateSpec{}, controller.HuskPodOptions{})
	if got := controller.CoLocatedForkVMBudgetForTest(singleVMPod); got != 0 {
		t.Fatalf("a single-VM warm pod must grant no co-location budget, got %d", got)
	}
}

// TestMultiVMForkCoLocatesRealisticWarmPod is the integration reproduction the CI
// missed: with --multi-vm-fork ON and a source husk pod carrying the EXACT resource
// shape a production multi-VM warm pool produces (withMultiVMWarmPodSizing, from the
// real pod builder), a fork child must co-locate INSIDE the source pod (spawn-vm, NO
// new child pod) and the fork must reach ReadyReplicas.
//
// On origin/main this reproduces the canary: the realistic multi-VM pod grants a
// co-location budget of 0, so the child spills to the new-pod path, the loud
// activator below refuses it (a co-located child never activates), the child never
// goes Ready, and the fork never reaches ReadyReplicas (plus a stray
// sandbox-<id>-fork-0 pod appears). After the fix the child co-locates: status.Pod
// is the source pod, status.VMID is set, and no child pod exists.
func TestMultiVMForkCoLocatesRealisticWarmPod(t *testing.T) {
	poolName := uniqueName("pool-mvm-real")
	srcClaimName := uniqueName("src-mvm-real")
	forkName := uniqueName("mvm-real")

	srcPod := makeDormantHuskPod(t, poolName, "10.0.8.20", multiVMSource, withMultiVMWarmPodSizing(t))
	makeForkSourceClaim(t, srcClaimName, poolName, srcPod)

	setForkSnapshotter(func(_ context.Context, _ string, _ *tls.Config, req husk.ForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
		return husk.ForkSnapshotResult{OK: true, SnapshotDir: req.SnapshotDir}, nil
	})
	t.Cleanup(func() { setForkSnapshotter(nil) })
	// A loud activator: the new-pod fork path activates, the co-location path does
	// NOT. If the realistic multi-VM source misroutes to a new pod, that child would
	// go through activate; failing it loud means a misroute can never masquerade as a
	// passing run.
	setForkActivator(func(_ context.Context, _ string, _ *tls.Config, _ husk.ActivateRequest) (husk.ActivateResult, error) {
		return husk.ActivateResult{OK: false, Error: "activate must not be called on the co-location path (fork misrouted to a new pod)"}, nil
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
		var got v1.Sandbox
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: forkName, Namespace: "default"}, &got); err != nil {
			return false
		}
		return got.Status.ReadyReplicas == 1
	})

	// The child co-located: no new sandbox-<id>-fork-0 pod exists.
	var pods corev1.PodList
	if err := k8sClient.List(ctx, &pods, listForkChildren(forkName)); err != nil {
		t.Fatalf("list children: %v", err)
	}
	if len(pods.Items) != 0 {
		t.Fatalf("a realistic multi-VM warm pod must co-locate the fork child, not spill to a new pod; got %d child pods", len(pods.Items))
	}
	if got := atomic.LoadInt32(&spawnCalls); got < 1 {
		t.Fatalf("expected at least one spawn-vm call (the co-located child), got %d", got)
	}

	var got v1.Sandbox
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: forkName, Namespace: "default"}, &got); err != nil {
		t.Fatalf("get fork: %v", err)
	}
	if len(got.Status.Children) != 1 {
		t.Fatalf("expected 1 recorded child, got %d", len(got.Status.Children))
	}
	if got.Status.Children[0].Pod != srcPod.Name {
		t.Errorf("child status.Pod = %q, want the source pod %q (co-located)", got.Status.Children[0].Pod, srcPod.Name)
	}
	if got.Status.Children[0].VMID == "" {
		t.Errorf("child status.VMID is empty, want the spawned vmID")
	}
}
