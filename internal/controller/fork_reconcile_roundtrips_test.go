package controller

// Unit coverage for the fork reconcile HOT-PATH status round-trip budget: a happy
// single-pass co-located husk fork must reach Ready while spending the MINIMUM
// number of status writes, with NO standalone intermediate ForkSnapshotTaken write.
// The former code persisted ForkSnapshotTaken in its own r.Status().Update before
// the child loop; folding it into the writes the pass already makes drops one
// apiserver round-trip the client waits on without weakening crash recovery (the
// flag is still durable before any child is activated from the snapshot).
//
// The test drives r.reconcileHuskFork directly through a fake client wrapped in a
// status-write counter, so the count is fully deterministic (one working pass, one
// writer, no informer-cache lag).

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
// by the target object's name, so a test can assert the number of status
// round-trips a single reconcile pass spends on a given object.
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

// TestHuskForkHappyPathStatusRoundtrips is the round-trip budget regression: a
// single-replica co-located husk fork must reach Ready in ONE working pass while
// writing status EXACTLY twice (the per-spawn child record, then the pass-boundary
// final write). Before the standalone ForkSnapshotTaken write was folded away this
// same pass wrote status three times; re-introducing a standalone intermediate
// write would make this count 3 and fail the test.
func TestHuskForkHappyPathStatusRoundtrips(t *testing.T) {
	scheme := forkRoundtripScheme(t)
	ctx := context.Background()

	const srcPodName = "src-pod"
	source := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "src", Namespace: "default"},
		Status: v1.SandboxStatus{
			Phase:     v1.SandboxReady,
			Node:      "n1",
			SandboxID: srcPodName,
		},
	}
	// A multi-VM-capable source pod with a memory budget that admits co-location
	// (1280Mi limit / 128Mi per VM = 10 VMs, 9 co-locatable), running with a PodIP
	// so the reconciler can dial it.
	srcPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      srcPodName,
			Namespace: "default",
			Labels:    map[string]string{huskMultiVMLabel: "true"},
		},
		Spec: corev1.PodSpec{
			NodeName: "n1",
			Containers: []corev1.Container{{
				Name: huskContainerName,
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1280Mi")},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.9"},
	}
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

	var snapCalls, spawnCalls int
	r := &SandboxReconciler{
		Client:          counter,
		Scheme:          scheme,
		EnableHuskPods:  true,
		HuskTLS:         &tls.Config{}, //nolint:gosec // unit stub; the fake spawner ignores it
		multiVMForkGate: func() bool { return true },
		forkSnapshot: func(_ context.Context, _ string, _ *tls.Config, req husk.ForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
			snapCalls++
			return husk.ForkSnapshotResult{OK: true, SnapshotDir: req.SnapshotDir}, nil
		},
		spawnVM: func(_ context.Context, _ string, _ *tls.Config, req husk.SpawnVMRequest) (husk.SpawnVMResult, error) {
			spawnCalls++
			return husk.SpawnVMResult{OK: true, VMID: req.VMID, VsockPath: "/run/husk/" + req.VMID + ".sock"}, nil
		},
	}

	if _, err := r.reconcileHuskFork(ctx, fork, source); err != nil {
		t.Fatalf("reconcileHuskFork: %v", err)
	}

	// Gate 1: the fork reached Ready with the child recorded.
	var got v1.Sandbox
	if err := counter.Get(ctx, client.ObjectKey{Name: "fork-1", Namespace: "default"}, &got); err != nil {
		t.Fatalf("get fork: %v", err)
	}
	if got.Status.ReadyReplicas != 1 {
		t.Fatalf("fork did not reach Ready: ReadyReplicas = %d, want 1", got.Status.ReadyReplicas)
	}
	if got.Status.Phase != v1.SandboxReady {
		t.Errorf("fork phase = %s, want Ready", got.Status.Phase)
	}
	if len(got.Status.Children) != 1 {
		t.Fatalf("expected 1 recorded child, got %d", len(got.Status.Children))
	}
	if got.Status.Children[0].Pod != srcPodName || got.Status.Children[0].VMID == "" {
		t.Errorf("child not recorded as a co-located VM in the source pod: %+v", got.Status.Children[0])
	}

	// Correctness preserved: ForkSnapshotTaken is still durably persisted (folded
	// into the writes the pass makes), so a crash cannot re-snapshot the source.
	if !got.Status.ForkSnapshotTaken {
		t.Errorf("ForkSnapshotTaken was not persisted; the folded write must still leave it durable")
	}
	// Timing anchors persisted (the instrumentation must survive the fold).
	if got.Status.ForkStartedAt == nil {
		t.Errorf("ForkStartedAt not persisted; timing instrumentation regressed")
	}
	if got.Status.ForkReconcilePasses != 1 {
		t.Errorf("ForkReconcilePasses = %d, want 1", got.Status.ForkReconcilePasses)
	}

	// The snapshot was taken exactly once and one child was spawned.
	if snapCalls != 1 {
		t.Errorf("fork-snapshot calls = %d, want 1", snapCalls)
	}
	if spawnCalls != 1 {
		t.Errorf("spawn-vm calls = %d, want 1", spawnCalls)
	}

	// The round-trip budget: EXACTLY 2 status writes on the happy single-replica
	// co-located pass (the per-spawn child record + the pass-boundary final write).
	// The former standalone ForkSnapshotTaken write made this 3; folding it away is
	// the round-trip saving this PR proves.
	if n := counter.count("fork-1"); n != 2 {
		t.Fatalf("happy-path fork spent %d status round-trips, want 2 (a standalone ForkSnapshotTaken write regressed the hot path)", n)
	}
}
