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
