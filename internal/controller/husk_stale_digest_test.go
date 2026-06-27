package controller_test

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	client "sigs.k8s.io/controller-runtime/pkg/client"

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
