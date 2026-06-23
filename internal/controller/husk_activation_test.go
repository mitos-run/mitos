package controller_test

// Envtest coverage for the husk-pod claim activation path (issue #18, slice 2).
// With EnableHuskPods, a claim activates a dormant Ready husk pod in place over
// the mTLS control channel instead of forking on a forkd node:
//
//   - a dormant Ready husk pod present  -> the reconciler activates it (the fake
//     activator records the snapshot dir + env + secrets), sets Endpoint/Node,
//     marks the pod claimed (the mitos.run/claim label appears), claim Ready;
//   - no dormant pod                    -> claim Pending;
//   - an Activate failure               -> claim does NOT go Ready;
//   - secret VALUES never appear in the captured suite log.
//
// The suite registers a husk-enabled claim reconciler that handles ONLY claims
// labeled mitos.run/husk-test, with a swappable activator (setHuskTest
// activator); the raw forkd reconciler skips those, so the two never race.

import (
	"context"
	"crypto/tls"
	v1 "mitos.run/mitos/api/v1"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"mitos.run/mitos/internal/controller"
	"mitos.run/mitos/internal/husk"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// fakeActivator records the requests it is asked to send and returns a scripted
// result. It is the seam the reconciler dials through instead of the real
// ActivateHuskPod.
type fakeActivator struct {
	mu      sync.Mutex
	reqs    []husk.ActivateRequest
	result  husk.ActivateResult
	err     error
	tlsSeen bool
}

func (f *fakeActivator) activate(_ context.Context, _ string, tlsConf *tls.Config, req husk.ActivateRequest) (husk.ActivateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	f.tlsSeen = tlsConf != nil
	return f.result, f.err
}

func (f *fakeActivator) lastReq() (husk.ActivateRequest, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		return husk.ActivateRequest{}, false
	}
	return f.reqs[len(f.reqs)-1], true
}

func (f *fakeActivator) sawTLS() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tlsSeen
}

func (f *fakeActivator) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

// makeDormantHuskPod creates a husk pod and forces it Running+Ready with a
// PodIP, simulating a warm dormant slot (envtest has no kubelet, so the test
// drives the status directly).
func makeDormantHuskPod(t *testing.T, poolName, podIP string) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: poolName + "-husk-",
			Namespace:    "default",
			Labels: map[string]string{
				"mitos.run/pool": poolName,
				"mitos.run/husk": "true",
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "kvm-node-1",
			Containers: []corev1.Container{{
				Name:  "husk-stub",
				Image: "mitos-husk-stub:test",
			}},
		},
	}
	// reconcileHuskPods stamps a controller owner reference to the pool on every
	// husk pod it creates, and selectDormantHuskPod now REQUIRES it before a pod
	// is an activation target (a tenant-planted decoy carrying only the labels
	// must never be selected). Mirror that here so the warm slot is selectable.
	// When the pool object exists, use its real UID; otherwise stamp a
	// pool-named reference with a placeholder UID (used by decoy-style fixtures
	// for pools that are never reconciled).
	var pool v1.SandboxPool
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: poolName, Namespace: "default"}, &pool); err == nil {
		if err := controllerutil.SetControllerReference(&pool, pod, k8sClient.Scheme()); err != nil {
			t.Fatalf("set husk pod owner reference: %v", err)
		}
	} else {
		yes := true
		pod.OwnerReferences = []metav1.OwnerReference{{
			APIVersion:         v1.GroupVersion.String(),
			Kind:               "SandboxPool",
			Name:               poolName,
			UID:                types.UID("placeholder-" + poolName),
			Controller:         &yes,
			BlockOwnerDeletion: &yes,
		}}
	}
	if err := k8sClient.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.PodIP = podIP
	pod.Status.Conditions = []corev1.PodCondition{{
		Type:   corev1.PodReady,
		Status: corev1.ConditionTrue,
	}}
	if err := k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, pod) })
	return pod
}

// makeHuskClaim creates a pool (inline template) and a husk-test-labeled claim
// and returns the claim.
func makeHuskClaim(t *testing.T, prefix string, spec v1.SandboxSpec) *v1.Sandbox {
	t.Helper()
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: prefix + "-pool", Namespace: "default"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}
	spec.Source.PoolRef = &v1.LocalObjectReference{Name: prefix + "-pool"}
	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      prefix + "-claim",
			Namespace: "default",
			Labels:    map[string]string{controller.HuskTestClaimLabel: "true"},
		},
		Spec: spec,
	}
	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := k8sClient.Create(ctx, claim); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, claim)
		_ = k8sClient.Delete(ctx, pool)
	})
	return claim
}

// waitClaimPhase polls until the named claim reaches one of the wanted phases.
func waitClaimPhase(t *testing.T, name string, want func(*v1.Sandbox) bool) *v1.Sandbox {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var got v1.Sandbox
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, &got); err == nil {
			if want(&got) {
				return &got
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("claim %s never reached the wanted state", name)
	return nil
}

func TestHuskClaimActivatesDormantPod(t *testing.T) {
	pod := makeDormantHuskPod(t, "husk-a-pool", "10.1.2.3")

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "husk-a-secret", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("super-secret-value-XYZ")},
	}
	if err := k8sClient.Create(ctx, secret); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, secret) })

	act := &fakeActivator{result: husk.ActivateResult{OK: true, VsockPath: "/run/husk/vm/vsock", LatencyMs: 1.2}}
	setHuskTestActivator(act.activate)
	t.Cleanup(func() { setHuskTestActivator(nil) })

	logStart := len(logBuf.Bytes())

	claim := makeHuskClaim(t, "husk-a", v1.SandboxSpec{
		Env: []corev1.EnvVar{{Name: "FOO", Value: "bar"}},
		Secrets: []v1.SecretMount{{
			Name: "API_TOKEN",
			SecretRef: corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "husk-a-secret"},
				Key:                  "token",
			},
		}},
	})

	got := waitClaimPhase(t, claim.Name, func(c *v1.Sandbox) bool {
		return c.Status.Phase == v1.SandboxReady
	})

	if got.Status.Endpoint != "10.1.2.3:9091" {
		t.Errorf("endpoint = %q, want 10.1.2.3:9091", got.Status.Endpoint)
	}
	if got.Status.Node != "kvm-node-1" {
		t.Errorf("node = %q, want kvm-node-1", got.Status.Node)
	}

	req, ok := act.lastReq()
	if !ok {
		t.Fatal("activator was never called")
	}
	if req.SnapshotDir == "" {
		t.Error("activate request carried no snapshot dir")
	}
	if req.Env["FOO"] != "bar" {
		t.Errorf("env FOO = %q, want bar", req.Env["FOO"])
	}
	if req.Secrets["API_TOKEN"] != "super-secret-value-XYZ" {
		t.Error("secret not delivered to the activate request")
	}
	if !act.sawTLS() {
		t.Error("activator was called without a TLS config")
	}

	// The pod is marked claimed.
	var claimedPod corev1.Pod
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: "default"}, &claimedPod); err != nil {
		t.Fatal(err)
	}
	if claimedPod.Labels["mitos.run/claim"] != claim.Name {
		t.Errorf("husk pod claim label = %q, want %q", claimedPod.Labels["mitos.run/claim"], claim.Name)
	}

	// Secret value never logged.
	if strings.Contains(string(logBuf.Bytes()[logStart:]), "super-secret-value-XYZ") {
		t.Error("secret value leaked into the controller log")
	}
}

// TestHuskClaimActivateCarriesExpectedDigest proves the controller threads the
// template's recorded CAS manifest digest (from the NodeRegistry, fed by forkd's
// GetCapacity) into the ActivateRequest, so the husk stub can re-verify the
// snapshot against it before loading (the husk mirror of forkd's verify-on-load
// gate). The digest is a content address, not a secret.
func TestHuskClaimActivateCarriesExpectedDigest(t *testing.T) {
	const wantDigest = "1111111111111111111111111111111111111111111111111111111111111111"
	const otherDigest = "2222222222222222222222222222222222222222222222222222222222222222"
	// Nodes build snapshots independently, so digests differ per node (#175): the
	// activation must verify against the digest of the node the chosen pod runs on
	// (kvm-node-1, per makeDormantHuskPod), NOT a cluster-wide pick. Register a
	// DECOY node with a different digest FIRST so a cluster-wide lookup would
	// resolve the wrong one; the pod's node carries wantDigest (#177).
	//
	// The template id the digest is keyed by is poolTemplateID(pool). In v1 the
	// pool carries an INLINE template (no separate SandboxTemplate object), so
	// poolTemplateID returns the POOL NAME (husk-d-pool), not a referenced
	// template name. Register the per-node digests under the pool name so the
	// lookup in the activation path resolves.
	testRegistry.Register(&controller.NodeInfo{
		Name:            "other-node",
		TemplateIDs:     []string{"husk-d-pool"},
		TemplateDigests: map[string]string{"husk-d-pool": otherDigest},
	})
	testRegistry.Register(&controller.NodeInfo{
		Name:            "kvm-node-1",
		TemplateIDs:     []string{"husk-d-pool"},
		TemplateDigests: map[string]string{"husk-d-pool": wantDigest},
	})
	t.Cleanup(func() { testRegistry.Unregister("other-node"); testRegistry.Unregister("kvm-node-1") })

	pod := makeDormantHuskPod(t, "husk-d-pool", "10.1.2.9")
	_ = pod

	act := &fakeActivator{result: husk.ActivateResult{OK: true, VsockPath: "/run/husk/vm/vsock", LatencyMs: 1.0}}
	setHuskTestActivator(act.activate)
	t.Cleanup(func() { setHuskTestActivator(nil) })

	claim := makeHuskClaim(t, "husk-d", v1.SandboxSpec{})

	waitClaimPhase(t, claim.Name, func(c *v1.Sandbox) bool {
		return c.Status.Phase == v1.SandboxReady
	})

	req, ok := act.lastReq()
	if !ok {
		t.Fatal("activator was never called")
	}
	if req.ExpectedDigest != wantDigest {
		t.Errorf("activate request ExpectedDigest = %q, want %q", req.ExpectedDigest, wantDigest)
	}
}

// TestHuskClaimSingleDormantPodNoDoubleAssign proves the isolation guarantee:
// with exactly ONE dormant husk pod and TWO claims racing for it, the
// optimistic-lock claim-before-activate path lets exactly ONE claim win the pod
// (activate it) and the other never activates the same pod. Concretely:
//   - the fake activator (the only dormant pod, so any activate targets it) is
//     called exactly once: only the winner reaches Activate;
//   - the pod's mitos.run/claim label names exactly one of the two claims;
//   - that named claim is Ready and the other is NOT Ready (Pending/requeued).
//
// Without the optimistic lock (a plain MergeFrom carries no resourceVersion),
// both claims could claim+activate the SAME pod, putting two tenants on one VM.
func TestHuskClaimSingleDormantPodNoDoubleAssign(t *testing.T) {
	// One pool (inline template), one dormant pod, two claims on that pool.
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "husk-race-pool", Namespace: "default"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}
	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, pool)
	})

	pod := makeDormantHuskPod(t, "husk-race-pool", "10.9.9.9")

	act := &fakeActivator{result: husk.ActivateResult{OK: true, VsockPath: "/run/husk/vm/vsock", LatencyMs: 1.0}}
	setHuskTestActivator(act.activate)
	t.Cleanup(func() { setHuskTestActivator(nil) })

	newClaim := func(name string) *v1.Sandbox {
		c := &v1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				Labels:    map[string]string{controller.HuskTestClaimLabel: "true"},
			},
			Spec: v1.SandboxSpec{Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: "husk-race-pool"}}},
		}
		if err := k8sClient.Create(ctx, c); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, c) })
		return c
	}
	c1 := newClaim("husk-race-claim-1")
	c2 := newClaim("husk-race-claim-2")

	// Wait until both claims have settled: one Ready, the other not Ready.
	deadline := time.Now().Add(20 * time.Second)
	var ready, other *v1.Sandbox
	for time.Now().Before(deadline) {
		var g1, g2 v1.Sandbox
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: c1.Name, Namespace: "default"}, &g1); err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: c2.Name, Namespace: "default"}, &g2); err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		r1 := g1.Status.Phase == v1.SandboxReady
		r2 := g2.Status.Phase == v1.SandboxReady
		// Exactly one Ready and the other pending (not Ready).
		if r1 != r2 && g1.Status.Phase != "" && g2.Status.Phase != "" {
			if r1 {
				ready, other = &g1, &g2
			} else {
				ready, other = &g2, &g1
			}
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if ready == nil {
		t.Fatal("the two racing claims never settled to exactly one Ready and one not-Ready")
	}
	if other.Status.Phase == v1.SandboxReady {
		t.Fatalf("both claims went Ready on a single dormant pod (double assignment): %s and %s", ready.Name, other.Name)
	}

	// Give the loser a moment; it must never flip to Ready on this same pod.
	time.Sleep(500 * time.Millisecond)
	var loser v1.Sandbox
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: other.Name, Namespace: "default"}, &loser); err != nil {
		t.Fatal(err)
	}
	if loser.Status.Phase == v1.SandboxReady {
		t.Fatalf("the losing claim %s eventually went Ready on the already-claimed pod (double assignment)", other.Name)
	}

	// The pod's claim label names EXACTLY the winner.
	var claimedPod corev1.Pod
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: "default"}, &claimedPod); err != nil {
		t.Fatal(err)
	}
	if got := claimedPod.Labels["mitos.run/claim"]; got != ready.Name {
		t.Fatalf("pod claim label = %q, want the winning claim %q", got, ready.Name)
	}

	// The activator (only one dormant pod exists) was called exactly once: only
	// the winner reached Activate.
	if n := act.callCount(); n != 1 {
		t.Fatalf("activator called %d times, want exactly 1 (the single dormant pod must be activated by exactly one claim)", n)
	}
}

func TestHuskClaimNoDormantPodPends(t *testing.T) {
	// No husk pod for this pool.
	act := &fakeActivator{result: husk.ActivateResult{OK: true}}
	setHuskTestActivator(act.activate)
	t.Cleanup(func() { setHuskTestActivator(nil) })

	claim := makeHuskClaim(t, "husk-b", v1.SandboxSpec{})

	got := waitClaimPhase(t, claim.Name, func(c *v1.Sandbox) bool {
		return c.Status.Phase == v1.SandboxPending
	})
	if got.Status.Phase != v1.SandboxPending {
		t.Errorf("phase = %q, want Pending", got.Status.Phase)
	}
	if _, ok := act.lastReq(); ok {
		t.Error("activator should not be called when no dormant pod exists")
	}
}

func TestHuskClaimActivateFailureNotReady(t *testing.T) {
	makeDormantHuskPod(t, "husk-c-pool", "10.4.5.6")

	act := &fakeActivator{result: husk.ActivateResult{OK: false, Error: "load snapshot: boom"}}
	setHuskTestActivator(act.activate)
	t.Cleanup(func() { setHuskTestActivator(nil) })

	claim := makeHuskClaim(t, "husk-c", v1.SandboxSpec{})

	// It should settle into Pending (fail closed, retryable) and never Ready.
	got := waitClaimPhase(t, claim.Name, func(c *v1.Sandbox) bool {
		return c.Status.Phase == v1.SandboxPending || c.Status.Phase == v1.SandboxFailed
	})
	if got.Status.Phase == v1.SandboxReady {
		t.Errorf("claim went Ready despite an activate failure: %+v", got.Status)
	}

	// Give the controller a moment; it must not flip to Ready.
	time.Sleep(500 * time.Millisecond)
	var again v1.Sandbox
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: claim.Name, Namespace: "default"}, &again); err != nil {
		t.Fatal(err)
	}
	if again.Status.Phase == v1.SandboxReady {
		t.Errorf("claim eventually went Ready despite repeated activate failures: %+v", again.Status)
	}
}
