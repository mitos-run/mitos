package controller_test

// Envtest coverage for the husk-pod live fork path (live SandboxFork on the husk
// pod-native path). With EnableHuskPods a husk-backed source claim forked with
// replicas=N snapshots the source pod's running VM once and activates N child
// husk pods from that fork snapshot, each reaching Ready through the same
// warm-pod Activate path (which runs the fail-closed RNG/clock reseed handshake).
//
// envtest has no kubelet, so the test forces each created child Running+Ready and
// drives the fork-snapshot / activate / remove transports through the suite's
// swappable fakes (setForkSnapshotter / setForkActivator / setForkSnapshotRemover).

import (
	"context"
	"crypto/tls"
	v1 "mitos.run/mitos/api/v1"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"mitos.run/mitos/internal/controller"
	"mitos.run/mitos/internal/husk"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// forceHuskPodReady forces a husk pod Running+Ready with its PodIP set, so the
// husk fork reconciler can dial it (envtest has no kubelet to do this).
func forceHuskPodReady(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	var got corev1.Pod
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, &got); err != nil {
		t.Fatalf("get pod %s: %v", pod.Name, err)
	}
	if got.Status.Phase == corev1.PodRunning && got.Status.PodIP != "" {
		return
	}
	got.Status.Phase = corev1.PodRunning
	if got.Status.PodIP == "" {
		got.Status.PodIP = "10.0.1.1"
	}
	got.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := k8sClient.Status().Update(ctx, &got); err != nil {
		t.Fatalf("force pod %s ready: %v", pod.Name, err)
	}
}

// listForkChildren matches the husk pods owned by a fork.
func listForkChildren(forkName string) client.ListOption {
	return client.MatchingLabels{"mitos.run/fork": forkName}
}

// waitUntilForkReady polls cond until true or the deadline elapses.
func waitUntilForkReady(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", d)
}

// makeForkSourceClaim creates a Ready husk-backed source sandbox pointing at srcPod.
func makeForkSourceClaim(t *testing.T, name, poolName string, srcPod *corev1.Pod) *v1.Sandbox {
	t.Helper()
	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       v1.SandboxSpec{Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: poolName}}},
	}
	if err := k8sClient.Create(ctx, claim); err != nil {
		t.Fatalf("create claim: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, claim) })
	// Retry-on-conflict: the claim reconciler pends this claim (its pool object
	// does not exist), so a stale-resourceVersion status write races it.
	updateSandboxStatusWithRetry(t, claim.Name, claim.Namespace, func(sb *v1.Sandbox) {
		sb.Status.Phase = v1.SandboxReady
		sb.Status.Node = srcPod.Spec.NodeName
		sb.Status.SandboxID = srcPod.Name
	})
	return claim
}

func TestHuskForkProducesReadyChildren(t *testing.T) {
	srcPod := makeDormantHuskPod(t, "pool-hf", "10.0.0.5")
	makeForkSourceClaim(t, "src-claim", "pool-hf", srcPod)

	var snapCalls int
	setForkSnapshotter(func(_ context.Context, _ string, _ *tls.Config, req husk.ForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
		snapCalls++
		return husk.ForkSnapshotResult{OK: true, SnapshotDir: req.SnapshotDir}, nil
	})
	t.Cleanup(func() { setForkSnapshotter(nil) })
	setForkActivator(func(_ context.Context, _ string, _ *tls.Config, _ husk.ActivateRequest) (husk.ActivateResult, error) {
		return husk.ActivateResult{OK: true, VsockPath: "/run/husk/vsock.sock"}, nil
	})
	t.Cleanup(func() { setForkActivator(nil) })

	fork := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "hf-1",
			Namespace: "default",
			Labels:    map[string]string{controller.HuskForkTestLabel: "true"},
		},
		Spec: v1.SandboxSpec{Source: v1.SandboxSource{FromSandbox: &v1.FromSandboxSource{Name: "src-claim"}}, Replicas: 2},
	}
	if err := k8sClient.Create(ctx, fork); err != nil {
		t.Fatalf("create fork: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, fork) })

	waitUntilForkReady(t, 15*time.Second, func() bool {
		var pods corev1.PodList
		_ = k8sClient.List(ctx, &pods, listForkChildren("hf-1"))
		for i := range pods.Items {
			forceHuskPodReady(t, &pods.Items[i])
		}
		var got v1.Sandbox
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "hf-1", Namespace: "default"}, &got); err != nil {
			return false
		}
		return got.Status.ReadyReplicas == 2
	})

	if snapCalls < 1 {
		t.Fatalf("expected at least one fork-snapshot call, got %d", snapCalls)
	}
}

// TestHuskForkSnapshotTakenExactlyOnce is the BUG 2 regression: the fork
// snapshot must be taken EXACTLY ONCE for a SandboxFork and reused for all
// children across reconcile passes. Children take several passes to reach Ready;
// re-snapshotting on each pass re-pauses the source and overwrites the fork
// mem/vmstate, so a child activated in a later pass would restore a NEWER source
// memory state than an earlier child: the N children would not be a coherent
// single fork point. The children are deliberately NOT forced Ready until after
// several reconcile passes have elapsed, so the bug (per-pass re-snapshot) would
// show snapCalls > 1.
func TestHuskForkSnapshotTakenExactlyOnce(t *testing.T) {
	srcPod := makeDormantHuskPod(t, "pool-once", "10.0.0.7")
	makeForkSourceClaim(t, "src-claim-once", "pool-once", srcPod)

	var snapCalls int32
	setForkSnapshotter(func(_ context.Context, _ string, _ *tls.Config, req husk.ForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
		atomic.AddInt32(&snapCalls, 1)
		return husk.ForkSnapshotResult{OK: true, SnapshotDir: req.SnapshotDir}, nil
	})
	t.Cleanup(func() { setForkSnapshotter(nil) })
	setForkActivator(func(_ context.Context, _ string, _ *tls.Config, _ husk.ActivateRequest) (husk.ActivateResult, error) {
		return husk.ActivateResult{OK: true, VsockPath: "/run/husk/vsock.sock"}, nil
	})
	t.Cleanup(func() { setForkActivator(nil) })

	fork := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "hf-once",
			Namespace: "default",
			Labels:    map[string]string{controller.HuskForkTestLabel: "true"},
		},
		Spec: v1.SandboxSpec{Source: v1.SandboxSource{FromSandbox: &v1.FromSandboxSource{Name: "src-claim-once"}}, Replicas: 2},
	}
	if err := k8sClient.Create(ctx, fork); err != nil {
		t.Fatalf("create fork: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, fork) })

	// Wait for the child pods to be created (this guarantees the snapshot op has
	// run at least once) WITHOUT forcing them Ready, so the reconciler requeues
	// repeatedly with children pending. A per-pass re-snapshot would bump
	// snapCalls above 1 during this window.
	waitUntilForkReady(t, 15*time.Second, func() bool {
		var pods corev1.PodList
		_ = k8sClient.List(ctx, &pods, listForkChildren("hf-once"))
		return len(pods.Items) == 2
	})
	// Let several requeue passes elapse with children still pending.
	time.Sleep(3 * time.Second)

	if got := atomic.LoadInt32(&snapCalls); got != 1 {
		t.Fatalf("fork snapshot must be taken exactly once across passes; got %d calls", got)
	}

	// Now drive the children Ready and confirm the fork still completes WITHOUT
	// any further snapshot calls (reuse of the single snapshot).
	waitUntilForkReady(t, 15*time.Second, func() bool {
		var pods corev1.PodList
		_ = k8sClient.List(ctx, &pods, listForkChildren("hf-once"))
		for i := range pods.Items {
			forceHuskPodReady(t, &pods.Items[i])
		}
		var got v1.Sandbox
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "hf-once", Namespace: "default"}, &got); err != nil {
			return false
		}
		return got.Status.ReadyReplicas == 2
	})

	if got := atomic.LoadInt32(&snapCalls); got != 1 {
		t.Fatalf("fork snapshot re-taken after children came Ready; got %d calls, want 1", got)
	}
}

// TestHuskForkNeverOverCreatesChildren is the regression for the live KVM
// over-creation bug: a SandboxFork with Replicas=2 driven through MANY reconcile
// passes while its children stay NOT Ready must create EXACTLY 2 child pods, never
// 3+. The old loop derived child names from (TotalForks + i) and the iteration
// count from (Replicas - ReadyForks); once a child became Ready mid-loop it bumped
// TotalForks, shifting the next index to a NEW name (fork-2, fork-3, ...) so
// ensureForkChildPod created an EXTRA pod instead of reusing a slot, overcommitting
// the single node. The fixed-slot loop uses stable names ("<fork>-fork-<i>" for i
// in [0, Replicas)) so the child count can never exceed Replicas. Children are kept
// pending across passes here to exercise the many-pass path that produced fork-2.
func TestHuskForkNeverOverCreatesChildren(t *testing.T) {
	srcPod := makeDormantHuskPod(t, "pool-noover", "10.0.0.11")
	makeForkSourceClaim(t, "src-claim-noover", "pool-noover", srcPod)

	setForkSnapshotter(func(_ context.Context, _ string, _ *tls.Config, req husk.ForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
		return husk.ForkSnapshotResult{OK: true, SnapshotDir: req.SnapshotDir}, nil
	})
	t.Cleanup(func() { setForkSnapshotter(nil) })
	setForkActivator(func(_ context.Context, _ string, _ *tls.Config, _ husk.ActivateRequest) (husk.ActivateResult, error) {
		return husk.ActivateResult{OK: true, VsockPath: "/run/husk/vsock.sock"}, nil
	})
	t.Cleanup(func() { setForkActivator(nil) })

	fork := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "noover-1",
			Namespace: "default",
			Labels:    map[string]string{controller.HuskForkTestLabel: "true"},
		},
		Spec: v1.SandboxSpec{Source: v1.SandboxSource{FromSandbox: &v1.FromSandboxSource{Name: "src-claim-noover"}}, Replicas: 2},
	}
	if err := k8sClient.Create(ctx, fork); err != nil {
		t.Fatalf("create fork: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, fork) })

	// Wait until both slots exist, then let many requeue passes elapse with the
	// children deliberately LEFT not-Ready (forceHuskPodReady is never called), so
	// the reconciler runs the full slot loop repeatedly.
	waitUntilForkReady(t, 15*time.Second, func() bool {
		var pods corev1.PodList
		_ = k8sClient.List(ctx, &pods, listForkChildren("noover-1"))
		return len(pods.Items) >= 2
	})
	time.Sleep(3 * time.Second)

	var pods corev1.PodList
	if err := k8sClient.List(ctx, &pods, listForkChildren("noover-1")); err != nil {
		t.Fatalf("list children: %v", err)
	}
	if len(pods.Items) != 2 {
		names := make([]string, 0, len(pods.Items))
		for i := range pods.Items {
			names = append(names, pods.Items[i].Name)
		}
		t.Fatalf("expected EXACTLY 2 child pods for Replicas=2, got %d: %v", len(pods.Items), names)
	}

	// Now drive them Ready and confirm the fork completes at exactly 2, still
	// without spawning a third pod.
	waitUntilForkReady(t, 15*time.Second, func() bool {
		var p corev1.PodList
		_ = k8sClient.List(ctx, &p, listForkChildren("noover-1"))
		for i := range p.Items {
			forceHuskPodReady(t, &p.Items[i])
		}
		var got v1.Sandbox
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "noover-1", Namespace: "default"}, &got); err != nil {
			return false
		}
		return got.Status.ReadyReplicas == 2
	})

	var after corev1.PodList
	if err := k8sClient.List(ctx, &after, listForkChildren("noover-1")); err != nil {
		t.Fatalf("list children after ready: %v", err)
	}
	if len(after.Items) != 2 {
		t.Fatalf("expected EXACTLY 2 child pods after completion, got %d", len(after.Items))
	}
}

func TestHuskForkRemovesSnapshotOnDelete(t *testing.T) {
	srcPod := makeDormantHuskPod(t, "pool-hd", "10.0.0.6")
	makeForkSourceClaim(t, "src-claim-d", "pool-hd", srcPod)

	setForkSnapshotter(func(_ context.Context, _ string, _ *tls.Config, req husk.ForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
		return husk.ForkSnapshotResult{OK: true, SnapshotDir: req.SnapshotDir}, nil
	})
	t.Cleanup(func() { setForkSnapshotter(nil) })
	setForkActivator(func(_ context.Context, _ string, _ *tls.Config, _ husk.ActivateRequest) (husk.ActivateResult, error) {
		return husk.ActivateResult{OK: true, VsockPath: "/run/x"}, nil
	})
	t.Cleanup(func() { setForkActivator(nil) })

	removeCalls := make(chan struct{}, 4)
	setForkSnapshotRemover(func(_ context.Context, _ string, _ *tls.Config, _ husk.RemoveForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
		select {
		case removeCalls <- struct{}{}:
		default:
		}
		return husk.ForkSnapshotResult{OK: true}, nil
	})
	t.Cleanup(func() { setForkSnapshotRemover(nil) })

	fork := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "hd-1",
			Namespace: "default",
			Labels:    map[string]string{controller.HuskForkTestLabel: "true"},
		},
		Spec: v1.SandboxSpec{Source: v1.SandboxSource{FromSandbox: &v1.FromSandboxSource{Name: "src-claim-d"}}, Replicas: 1},
	}
	if err := k8sClient.Create(ctx, fork); err != nil {
		t.Fatalf("create fork: %v", err)
	}

	waitUntilForkReady(t, 15*time.Second, func() bool {
		var pods corev1.PodList
		_ = k8sClient.List(ctx, &pods, listForkChildren("hd-1"))
		for i := range pods.Items {
			forceHuskPodReady(t, &pods.Items[i])
		}
		var got v1.Sandbox
		_ = k8sClient.Get(ctx, types.NamespacedName{Name: "hd-1", Namespace: "default"}, &got)
		return got.Status.ReadyReplicas == 1
	})

	if err := k8sClient.Delete(ctx, fork); err != nil {
		t.Fatalf("delete fork: %v", err)
	}
	select {
	case <-removeCalls:
	case <-time.After(15 * time.Second):
		t.Fatalf("remove-fork-snapshot was not called on delete")
	}
}

// TestHuskForkChildPodHasFullHuskShape is the regression for the live KVM crash
// "husk-stub: read --tls-cert: open /etc/husk/tls/tls.crt: no such file or
// directory": the fork child pod the controller emits must carry EVERYTHING a
// warm husk pod carries (the husk PKI mTLS Secret volumes, the kernel, forks, and
// writable rootfs-CoW hostPaths, the kvm device resource, the POD_NAME downward
// API, and the locked-down securityContext), so the child stub finds its TLS
// material and all the files it activates from. The previous bug threaded the
// fork-specific opts but DROPPED TLSSecretName/CASecretName, so buildHuskPod
// skipped the TLS/CA volumes and the child crash-looped. This drives the real
// controller opts path (not a direct buildForkChildPod call), so it catches the
// wiring, not just the builder.
func TestHuskForkChildPodHasFullHuskShape(t *testing.T) {
	// Per-run unique names so repeated runs on the shared apiserver never collide.
	// The fork name in particular is finalizer-gated on delete (the husk-fork
	// reconciler stamps mitos.run/husk-fork-snapshot and clears it asynchronously),
	// so a fixed name races the prior run's still-terminating object on Create
	// (object is being deleted) under -count: the pre-existing flake. A unique fork
	// name also scopes listForkChildren (keyed on mitos.run/fork) to this run.
	poolName := uniqueName("pool-shape")
	srcClaimName := uniqueName("src-claim-shape")
	forkName := uniqueName("shape")
	srcPod := makeDormantHuskPod(t, poolName, "10.0.0.9")
	makeForkSourceClaim(t, srcClaimName, poolName, srcPod)

	setForkSnapshotter(func(_ context.Context, _ string, _ *tls.Config, req husk.ForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
		return husk.ForkSnapshotResult{OK: true, SnapshotDir: req.SnapshotDir}, nil
	})
	t.Cleanup(func() { setForkSnapshotter(nil) })
	setForkActivator(func(_ context.Context, _ string, _ *tls.Config, _ husk.ActivateRequest) (husk.ActivateResult, error) {
		return husk.ActivateResult{OK: true, VsockPath: "/run/x"}, nil
	})
	t.Cleanup(func() { setForkActivator(nil) })

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

	// Wait for the controller to emit the child pod (forcing it Ready so the
	// reconcile keeps progressing).
	var child corev1.Pod
	waitUntilForkReady(t, 15*time.Second, func() bool {
		var pods corev1.PodList
		_ = k8sClient.List(ctx, &pods, listForkChildren(forkName))
		for i := range pods.Items {
			forceHuskPodReady(t, &pods.Items[i])
		}
		if len(pods.Items) == 0 {
			return false
		}
		child = pods.Items[0]
		return true
	})

	// Index the pod's volumes and the stub container's mounts.
	vols := map[string]corev1.Volume{}
	for _, v := range child.Spec.Volumes {
		vols[v.Name] = v
	}
	var stub corev1.Container
	for i := range child.Spec.Containers {
		if child.Spec.Containers[i].Name == "husk-stub" {
			stub = child.Spec.Containers[i]
		}
	}
	if stub.Name == "" {
		t.Fatalf("fork child has no husk-stub container: %+v", child.Spec.Containers)
	}
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range stub.VolumeMounts {
		mounts[m.Name] = m
	}

	// The husk PKI TLS Secret volumes + mounts (the crash). The leaf Secret backs
	// /etc/husk/tls (tls.crt + tls.key); the CA Secret backs /etc/husk/ca (ca.crt).
	tlsVol, ok := vols["husk-tls"]
	if !ok || tlsVol.Secret == nil || tlsVol.Secret.SecretName != "mitos-husk-tls" {
		t.Fatalf("fork child missing husk-tls leaf Secret volume (mitos-husk-tls): %+v", child.Spec.Volumes)
	}
	if m, ok := mounts["husk-tls"]; !ok || !m.ReadOnly || m.MountPath != "/etc/husk/tls" {
		t.Fatalf("fork child husk-tls mount missing/wrong (want RO /etc/husk/tls): %+v", mounts)
	}
	caVol, ok := vols["husk-ca"]
	if !ok || caVol.Secret == nil || caVol.Secret.SecretName != "mitos-ca" {
		t.Fatalf("fork child missing husk-ca Secret volume (mitos-ca): %+v", child.Spec.Volumes)
	}
	if m, ok := mounts["husk-ca"]; !ok || !m.ReadOnly || m.MountPath != "/etc/husk/ca" {
		t.Fatalf("fork child husk-ca mount missing/wrong (want RO /etc/husk/ca): %+v", mounts)
	}

	// The kernel hostPath + mount.
	if _, ok := vols["kernel"]; !ok {
		t.Fatalf("fork child missing kernel hostPath volume: %+v", child.Spec.Volumes)
	}
	if m, ok := mounts["kernel"]; !ok || m.MountPath != "/var/lib/mitos/kernel/vmlinux" {
		t.Fatalf("fork child kernel mount missing/wrong: %+v", mounts)
	}

	// The forks hostPath dir (read-write so the child can itself be re-forked).
	if _, ok := vols["husk-forks"]; !ok {
		t.Fatalf("fork child missing husk-forks hostPath volume: %+v", child.Spec.Volumes)
	}
	if m, ok := mounts["husk-forks"]; !ok || m.ReadOnly {
		t.Fatalf("fork child husk-forks mount missing or read-only: %+v", mounts)
	}

	// The writable per-activation rootfs-CoW hostPath dir (the child writes its
	// own clone here, so it must NOT be read-only).
	if _, ok := vols["husk-rootfs-cow"]; !ok {
		t.Fatalf("fork child missing husk-rootfs-cow hostPath volume: %+v", child.Spec.Volumes)
	}
	if m, ok := mounts["husk-rootfs-cow"]; !ok || m.ReadOnly {
		t.Fatalf("fork child husk-rootfs-cow mount missing or read-only (child writes its CoW clone): %+v", mounts)
	}

	// The fork snapshot mount itself (read-only), pointing at the fork snapshot.
	if m, ok := mounts["snapshot"]; !ok || !m.ReadOnly {
		t.Fatalf("fork child snapshot mount missing or not read-only: %+v", mounts)
	}

	// The kvm device resource: request == limit == 1.
	req := stub.Resources.Requests[corev1.ResourceName("mitos.run/kvm")]
	lim := stub.Resources.Limits[corev1.ResourceName("mitos.run/kvm")]
	if req.Value() != 1 || lim.Value() != 1 {
		t.Fatalf("fork child kvm device resource not 1/1: req=%s lim=%s", req.String(), lim.String())
	}

	// POD_NAME downward API (scopes the per-pod CoW clone path).
	var hasPodName bool
	for _, e := range stub.Env {
		if e.Name == "POD_NAME" && e.ValueFrom != nil && e.ValueFrom.FieldRef != nil && e.ValueFrom.FieldRef.FieldPath == "metadata.name" {
			hasPodName = true
		}
	}
	if !hasPodName {
		t.Fatalf("fork child missing POD_NAME downward API env: %+v", stub.Env)
	}

	// SecurityContext: same lockdown as a warm pod (no privilege, no escalation,
	// drop ALL caps, RuntimeDefault seccomp).
	sc := stub.SecurityContext
	if sc == nil || sc.Privileged == nil || *sc.Privileged {
		t.Fatalf("fork child must not be privileged: %+v", sc)
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Fatalf("fork child must deny privilege escalation: %+v", sc)
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("fork child must drop ALL caps: %+v", sc.Capabilities)
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("fork child must use RuntimeDefault seccomp: %+v", sc.SeccompProfile)
	}

	// Fork-specific bits remain: pinned to the source node, and --template-rootfs
	// (the CoW clone source) is the FROZEN source rootfs the source stub captured
	// inside the fork snapshot's paused window (SnapshotDir/rootfs.ext4 on the
	// read-only snapshot mount), NOT the source pod's LIVE rootfs under the
	// husk-rootfs CoW dir. Cloning from the live rootfs would let the resumed
	// source drift the child's disk out of sync with its memory checkpoint.
	if child.Spec.Affinity == nil || child.Spec.Affinity.NodeAffinity == nil {
		t.Fatalf("fork child must be pinned to the source node via affinity")
	}
	args := strings.Join(stub.Args, " ")
	if !strings.Contains(args, "--template-rootfs /var/lib/mitos/snapshot/rootfs.ext4") {
		t.Fatalf("fork child --template-rootfs must be the frozen snapshot rootfs; args=%v", stub.Args)
	}
	// It must NOT clone from the source's live rootfs (the resumed source keeps
	// writing that file).
	if strings.Contains(args, "--template-rootfs /var/lib/mitos/husk-rootfs/"+srcPod.Name+"/rootfs.ext4") {
		t.Fatalf("fork child must not clone from the source's live rootfs; args=%v", stub.Args)
	}
}

// TestHuskForkAdoptsAlreadyActiveChild is the #183 regression. Activation is not
// transactional: a prior Activate can succeed (VM active) while its ack or the
// controller's post-activate bookkeeping is lost, so the child is never recorded.
// On the next pass the stub refuses to re-activate a non-dormant VM and returns
// AlreadyActive (OK=false). Before the fix the controller logged "will retry" and
// looped forever (ReadyForks stuck below Replicas); the fix ADOPTS an
// AlreadyActive child (the VM is up with the stable, persisted token) so the fork
// converges. The fake activator below ALWAYS reports AlreadyActive, so without the
// adoption this test times out.
func TestHuskForkAdoptsAlreadyActiveChild(t *testing.T) {
	srcPod := makeDormantHuskPod(t, "pool-hfaa", "10.0.2.5")
	makeForkSourceClaim(t, "src-claim-aa", "pool-hfaa", srcPod)

	setForkSnapshotter(func(_ context.Context, _ string, _ *tls.Config, req husk.ForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
		return husk.ForkSnapshotResult{OK: true, SnapshotDir: req.SnapshotDir}, nil
	})
	t.Cleanup(func() { setForkSnapshotter(nil) })
	setForkActivator(func(_ context.Context, _ string, _ *tls.Config, _ husk.ActivateRequest) (husk.ActivateResult, error) {
		return husk.ActivateResult{OK: false, AlreadyActive: true, Error: "activate in state active: must be dormant"}, nil
	})
	t.Cleanup(func() { setForkActivator(nil) })

	fork := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "hfaa-1",
			Namespace: "default",
			Labels:    map[string]string{controller.HuskForkTestLabel: "true"},
		},
		Spec: v1.SandboxSpec{Source: v1.SandboxSource{FromSandbox: &v1.FromSandboxSource{Name: "src-claim-aa"}}, Replicas: 2},
	}
	if err := k8sClient.Create(ctx, fork); err != nil {
		t.Fatalf("create fork: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, fork) })

	waitUntilForkReady(t, 15*time.Second, func() bool {
		var pods corev1.PodList
		_ = k8sClient.List(ctx, &pods, listForkChildren("hfaa-1"))
		for i := range pods.Items {
			forceHuskPodReady(t, &pods.Items[i])
		}
		var got v1.Sandbox
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "hfaa-1", Namespace: "default"}, &got); err != nil {
			return false
		}
		return got.Status.ReadyReplicas == 2
	})
}

// TestHuskForkChildPodInheritsSourcePodScheduling is the regression for the
// production fork 504: the fork child pod is pinned to the source node via
// nodeAffinity, but the hosted KVM node carries the mitos.run/dedicated
// NoSchedule taint that warm pods tolerate via the pool's spec.placement
// tolerations. The fork path never carried those tolerations onto the child,
// so the child sat Pending forever with FailedScheduling "1 node(s) had
// untolerated taint {mitos.run/dedicated}". The child must inherit the SOURCE
// pod's own scheduling constraints (tolerations and nodeSelector; the source
// pod's spec is the authoritative record of what it took to land on that
// node), while keeping the exact-node affinity pin. Like the full-shape test
// this drives the real controller opts path, not a direct builder call.
// envtest schedules nothing (no scheduler, untainted fake nodes), so only a
// real-cluster e2e proves scheduling against actual taints; this test pins the
// pod SPEC the controller emits.
func TestHuskForkChildPodInheritsSourcePodScheduling(t *testing.T) {
	poolName := uniqueName("pool-sched")
	srcClaimName := uniqueName("src-claim-sched")
	forkName := uniqueName("sched")

	notReadySecs := int64(60)
	srcPod := makeDormantHuskPod(t, poolName, "10.0.5.5", func(p *corev1.Pod) {
		// The scheduling constraints a real warm husk pod carries on a hosted
		// dedicated KVM node: the KVM+placement nodeSelector and the
		// fast-node-loss pair plus the placement toleration for the node taint.
		p.Spec.NodeSelector = map[string]string{
			"mitos.run/kvm":    "true",
			"mitos.run/tenant": "acme",
		}
		p.Spec.Tolerations = []corev1.Toleration{
			{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &notReadySecs},
			{Key: "node.kubernetes.io/unreachable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &notReadySecs},
			{Key: "mitos.run/dedicated", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
		}
	})
	makeForkSourceClaim(t, srcClaimName, poolName, srcPod)

	setForkSnapshotter(func(_ context.Context, _ string, _ *tls.Config, req husk.ForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
		return husk.ForkSnapshotResult{OK: true, SnapshotDir: req.SnapshotDir}, nil
	})
	t.Cleanup(func() { setForkSnapshotter(nil) })
	setForkActivator(func(_ context.Context, _ string, _ *tls.Config, _ husk.ActivateRequest) (husk.ActivateResult, error) {
		return husk.ActivateResult{OK: true, VsockPath: "/run/x"}, nil
	})
	t.Cleanup(func() { setForkActivator(nil) })

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

	var child corev1.Pod
	waitUntilForkReady(t, 15*time.Second, func() bool {
		var pods corev1.PodList
		_ = k8sClient.List(ctx, &pods, listForkChildren(forkName))
		if len(pods.Items) == 0 {
			return false
		}
		child = pods.Items[0]
		return true
	})

	// The production failure: the child did not tolerate the source node's
	// mitos.run/dedicated NoSchedule taint and could never schedule.
	tolCount := map[string]int{}
	for _, tol := range child.Spec.Tolerations {
		tolCount[tol.Key]++
	}
	if tolCount["mitos.run/dedicated"] != 1 {
		t.Errorf("fork child must carry the source pod's mitos.run/dedicated toleration exactly once, got %d; tolerations=%v", tolCount["mitos.run/dedicated"], child.Spec.Tolerations)
	}
	// Inheritance must not duplicate the fast node-loss pair huskTolerations
	// adds to every husk pod.
	for _, key := range []string{"node.kubernetes.io/not-ready", "node.kubernetes.io/unreachable"} {
		if tolCount[key] != 1 {
			t.Errorf("fork child toleration %s count = %d, want exactly 1 (no duplicates from source inheritance); tolerations=%v", key, tolCount[key], child.Spec.Tolerations)
		}
	}
	// The source pod's merged nodeSelector (KVM + placement) rides along too.
	if child.Spec.NodeSelector["mitos.run/tenant"] != "acme" || child.Spec.NodeSelector["mitos.run/kvm"] != "true" {
		t.Errorf("fork child nodeSelector = %v, want the source pod's kvm=true + tenant=acme", child.Spec.NodeSelector)
	}
	// The exact-node pin stays: the fork snapshot is a node-local hostPath on
	// the source node.
	aff := child.Spec.Affinity
	if aff == nil || aff.NodeAffinity == nil || aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		t.Fatalf("fork child must keep the required nodeAffinity pin to the source node; affinity=%v", aff)
	}
	terms := aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	pinned := false
	for _, term := range terms {
		for _, expr := range term.MatchExpressions {
			if expr.Key == "kubernetes.io/hostname" && len(expr.Values) == 1 && expr.Values[0] == srcPod.Spec.NodeName {
				pinned = true
			}
		}
	}
	if !pinned {
		t.Errorf("fork child nodeAffinity must pin to the source node %q; terms=%v", srcPod.Spec.NodeName, terms)
	}
}
