package controller

// Unit coverage for the fork reconcile status round-trip budget.
//
// The husk fork reconcile used to persist ForkSnapshotTaken in its OWN
// r.Status().Update before the child loop, on every path. That standalone write is
// redundant with the pass-boundary write on the NEW-POD path (where no child is
// activated in the snapshot pass), so folding it there saves one apiserver
// round-trip. On the CO-LOCATION path the child is ACTIVATED in the same pass
// before the per-spawn write commits, so the flag MUST stay durable before that
// first activation (else a crash could re-snapshot under an already-restored child
// and split a multi-replica fork point); that write is load-bearing and kept.
//
// These tests drive reconcileHuskFork directly through a status-write counting
// client, so the counts are fully deterministic (one writer, no informer lag).

import (
	"context"
	"crypto/tls"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/husk"
)

// statusUpdateCounter wraps a client.Client and counts Status().Update calls keyed
// by the target object's name, so a test can assert how many status round-trips a
// reconcile pass spends on a given object.
type statusUpdateCounter struct {
	client.Client
	mu     sync.Mutex
	counts map[string]int
}

func (c *statusUpdateCounter) Status() client.SubResourceWriter {
	return &countingStatusWriter{parent: c, SubResourceWriter: c.Client.Status()}
}

type countingStatusWriter struct {
	client.SubResourceWriter
	parent *statusUpdateCounter
}

func (w *countingStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	w.parent.mu.Lock()
	w.parent.counts[obj.GetName()]++
	w.parent.mu.Unlock()
	// The wrapper must be transparent so the reconciler observes the delegate's
	// error verbatim (apierrors.IsConflict and friends must still match); it is not
	// wrapped with fmt.Errorf.
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func (c *statusUpdateCounter) count(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[name]
}

func forkRoundtripScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

// forkRoundtripSource builds a Ready husk-backed source Sandbox and its backing pod
// (running, with a PodIP so the reconciler can dial it). multiVM stamps the source
// pod as a co-location candidate with a memory budget that admits co-location.
func forkRoundtripSource(t *testing.T, srcPodName string, multiVM bool) (*v1.Sandbox, *corev1.Pod) {
	t.Helper()
	source := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "src", Namespace: "default"},
		Status:     v1.SandboxStatus{Phase: v1.SandboxReady, Node: "n1", SandboxID: srcPodName},
	}
	srcPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: srcPodName, Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName:   "n1",
			Containers: []corev1.Container{{Name: huskContainerName}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.9"},
	}
	if multiVM {
		srcPod.Labels = map[string]string{huskMultiVMLabel: "true"}
		// 1280Mi limit / 128Mi per VM = 10 VMs, 9 co-locatable.
		srcPod.Spec.Containers[0].Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1280Mi")},
		}
	}
	return source, srcPod
}

// TestHuskForkNewPodPathFoldsSnapshotWrite proves the round-trip saving: on the
// NEW-POD path the snapshot pass no longer spends a standalone ForkSnapshotTaken
// write. Pass 1 (children created, not yet Ready) writes status EXACTLY ONCE (the
// pass-boundary write, which now also persists the flag) where the former code
// wrote twice, and the flag is still durable afterward. The fork then reaches Ready
// on a later pass, with no re-snapshot write.
func TestHuskForkNewPodPathFoldsSnapshotWrite(t *testing.T) {
	scheme := forkRoundtripScheme(t)
	ctx := context.Background()

	source, srcPod := forkRoundtripSource(t, "src-pod", false) // NOT multi-VM: new-pod path
	fork := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "fork-1", Namespace: "default"},
		Spec: v1.SandboxSpec{
			Source:   v1.SandboxSource{FromSandbox: &v1.FromSandboxSource{Name: "src"}},
			Replicas: 1,
		},
	}
	base := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1.Sandbox{}).
		WithObjects(source, srcPod, fork).
		Build()
	counter := &statusUpdateCounter{Client: base, counts: map[string]int{}}

	var snapCalls, actCalls int
	r := &SandboxReconciler{
		Client:         counter,
		Scheme:         scheme,
		EnableHuskPods: true,
		HuskTLS:        &tls.Config{}, //nolint:gosec // unit stub; the fake seam ignores it
		HuskStubImage:  "mitos-husk-stub:test",
		DataDir:        "/var/lib/mitos",
		forkSnapshot: func(_ context.Context, _ string, _ *tls.Config, req husk.ForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
			snapCalls++
			return husk.ForkSnapshotResult{OK: true, SnapshotDir: req.SnapshotDir}, nil
		},
		Activate: func(_ context.Context, _ string, _ *tls.Config, _ husk.ActivateRequest) (husk.ActivateResult, error) {
			actCalls++
			return husk.ActivateResult{OK: true, VsockPath: "/run/husk/vsock.sock"}, nil
		},
	}

	// Pass 1: the child pod is created (not Ready), so the snapshot pass writes
	// status exactly ONCE (the folded pass-boundary write). The former standalone
	// ForkSnapshotTaken write made this 2.
	if _, err := r.reconcileHuskFork(ctx, fork, source); err != nil {
		t.Fatalf("reconcileHuskFork pass 1: %v", err)
	}
	if n := counter.count("fork-1"); n != 1 {
		t.Fatalf("new-pod snapshot pass spent %d status round-trips, want 1 (a standalone ForkSnapshotTaken write regressed the fold)", n)
	}
	var mid v1.Sandbox
	if err := counter.Get(ctx, client.ObjectKey{Name: "fork-1", Namespace: "default"}, &mid); err != nil {
		t.Fatalf("get fork after pass 1: %v", err)
	}
	if !mid.Status.ForkSnapshotTaken {
		t.Fatalf("ForkSnapshotTaken not durable after the snapshot pass; the fold must still persist it")
	}
	if mid.Status.ForkStartedAt == nil || mid.Status.ForkReconcilePasses != 1 {
		t.Errorf("timing anchors not persisted: ForkStartedAt=%v passes=%d", mid.Status.ForkStartedAt, mid.Status.ForkReconcilePasses)
	}
	// The child pod exists but is not Ready yet, so no activation happened.
	childName := "fork-1-fork-0"
	var childPod corev1.Pod
	if err := counter.Get(ctx, client.ObjectKey{Name: childName, Namespace: "default"}, &childPod); err != nil {
		t.Fatalf("child pod not created in pass 1: %v", err)
	}
	if actCalls != 0 {
		t.Fatalf("child was activated in the snapshot pass (%d activations); the new-pod fold relies on activation happening a LATER pass", actCalls)
	}

	// Pass 2: force the child Running+Ready, so it activates and the fork reaches
	// Ready. The snapshot is NOT re-taken (flag durable) and this pass writes status
	// once (the final pass-boundary write).
	childPod.Status.Phase = corev1.PodRunning
	childPod.Status.PodIP = "10.0.2.2"
	childPod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := counter.Status().Update(ctx, &childPod); err != nil {
		t.Fatalf("force child ready: %v", err)
	}
	before := counter.count("fork-1")
	if _, err := r.reconcileHuskFork(ctx, fork, source); err != nil {
		t.Fatalf("reconcileHuskFork pass 2: %v", err)
	}
	if delta := counter.count("fork-1") - before; delta != 1 {
		t.Fatalf("pass 2 spent %d fork-status writes, want 1 (no re-snapshot write)", delta)
	}

	var got v1.Sandbox
	if err := counter.Get(ctx, client.ObjectKey{Name: "fork-1", Namespace: "default"}, &got); err != nil {
		t.Fatalf("get fork after pass 2: %v", err)
	}
	if got.Status.ReadyReplicas != 1 || got.Status.Phase != v1.SandboxReady {
		t.Fatalf("fork did not reach Ready: ReadyReplicas=%d phase=%s", got.Status.ReadyReplicas, got.Status.Phase)
	}
	if snapCalls != 1 {
		t.Errorf("fork-snapshot taken %d times across passes, want exactly 1", snapCalls)
	}
}

// TestHuskForkCoLocatedSnapshotDurableBeforeActivation is the crash-coherence
// regression guard for the co-location path: spawn-vm ACTIVATES the child inside the
// source pod, so ForkSnapshotTaken MUST already be durable at the moment of the
// FIRST spawn. Otherwise a failed per-spawn write or a crash in that window leaves
// an active child while the flag is not durable, and a later pass re-snapshots under
// it, splitting a multi-replica fork into an incoherent point. This asserts the flag
// is persisted (readable from the store) before the first spawn-vm call, and that
// the fork still reaches Ready.
func TestHuskForkCoLocatedSnapshotDurableBeforeActivation(t *testing.T) {
	scheme := forkRoundtripScheme(t)
	ctx := context.Background()

	source, srcPod := forkRoundtripSource(t, "src-pod", true) // multi-VM: co-location path
	fork := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "fork-2", Namespace: "default"},
		Spec: v1.SandboxSpec{
			Source:   v1.SandboxSource{FromSandbox: &v1.FromSandboxSource{Name: "src"}},
			Replicas: 2,
		},
	}
	base := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1.Sandbox{}).
		WithObjects(source, srcPod, fork).
		Build()
	counter := &statusUpdateCounter{Client: base, counts: map[string]int{}}

	var mu sync.Mutex
	var firstSpawn bool
	var durableAtFirstSpawn bool
	r := &SandboxReconciler{
		Client:          counter,
		Scheme:          scheme,
		EnableHuskPods:  true,
		HuskTLS:         &tls.Config{}, //nolint:gosec // unit stub; the fake seam ignores it
		multiVMForkGate: func() bool { return true },
		forkSnapshot: func(_ context.Context, _ string, _ *tls.Config, req husk.ForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
			return husk.ForkSnapshotResult{OK: true, SnapshotDir: req.SnapshotDir}, nil
		},
		spawnVM: func(_ context.Context, _ string, _ *tls.Config, req husk.SpawnVMRequest) (husk.SpawnVMResult, error) {
			// Read the fork back from the store AT SPAWN TIME: the flag must already be
			// durable (the load-bearing pre-activation write committed it).
			mu.Lock()
			defer mu.Unlock()
			if !firstSpawn {
				firstSpawn = true
				var stored v1.Sandbox
				if err := counter.Get(ctx, client.ObjectKey{Name: "fork-2", Namespace: "default"}, &stored); err == nil {
					durableAtFirstSpawn = stored.Status.ForkSnapshotTaken
				}
			}
			return husk.SpawnVMResult{OK: true, VMID: req.VMID, VsockPath: "/run/husk/" + req.VMID + ".sock"}, nil
		},
	}

	if _, err := r.reconcileHuskFork(ctx, fork, source); err != nil {
		t.Fatalf("reconcileHuskFork: %v", err)
	}

	if !firstSpawn {
		t.Fatal("no co-located spawn happened; the test did not exercise the co-location path")
	}
	if !durableAtFirstSpawn {
		t.Fatal("ForkSnapshotTaken was NOT durable at the first co-located activation; a crash here could re-snapshot under an already-restored child and split a multi-replica fork")
	}

	var got v1.Sandbox
	if err := counter.Get(ctx, client.ObjectKey{Name: "fork-2", Namespace: "default"}, &got); err != nil {
		t.Fatalf("get fork: %v", err)
	}
	if got.Status.ReadyReplicas != 2 || got.Status.Phase != v1.SandboxReady {
		t.Fatalf("co-located fork did not reach Ready: ReadyReplicas=%d phase=%s", got.Status.ReadyReplicas, got.Status.Phase)
	}
	for i := range got.Status.Children {
		if got.Status.Children[i].Pod != "src-pod" || got.Status.Children[i].VMID == "" {
			t.Errorf("child %d not recorded as a co-located VM: %+v", i, got.Status.Children[i])
		}
	}
}
