package controller_test

// Envtest coverage for the best-effort husk NetworkPolicy (network-isolation
// blocker, Task 10). ensureHuskNetworkPolicy creates a default-deny egress
// NetworkPolicy selecting the pool's husk pods, owner-referenced to the pool for
// GC, with one egress rule per enforceable IP:port allow plus the DNS allow.
//
// HONEST CAVEAT: this object only enforces on a CNI that implements
// NetworkPolicy; the in-pod nftables filter the husk-stub programs is the
// egress guarantee. This test asserts the OBJECT is created correctly, not that
// any CNI enforces it (envtest has no CNI).

import (
	"context"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	v1alpha1 "mitos.run/mitos/api/v1alpha1"
	"mitos.run/mitos/internal/controller"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestEnsureHuskNetworkPolicyCreatesObject(t *testing.T) {
	c := k8sClient
	ctx := context.Background()

	pool := &v1alpha1.SandboxPool{ObjectMeta: metav1.ObjectMeta{Name: "np-pool", Namespace: "default"}}
	if err := c.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Delete(ctx, pool) })

	// Re-fetch so SetControllerReference has the server UID.
	var live v1alpha1.SandboxPool
	if err := c.Get(ctx, client.ObjectKeyFromObject(pool), &live); err != nil {
		t.Fatal(err)
	}

	r := &controller.SandboxPoolReconciler{Client: c}
	if err := r.EnsureHuskNetworkPolicyForTest(ctx, &live, []string{"10.0.0.5:5432"}); err != nil {
		t.Fatalf("ensureHuskNetworkPolicy: %v", err)
	}

	var np networkingv1.NetworkPolicy
	name := controller.HuskNetworkPolicyNameForTest("np-pool")
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, &np); err != nil {
		t.Fatalf("network policy not created: %v", err)
	}

	// Owner-ref to the pool so it is GC'd with the pool.
	owner := metav1.GetControllerOf(&np)
	if owner == nil || owner.Kind != "SandboxPool" || owner.Name != "np-pool" {
		t.Fatalf("NetworkPolicy owner = %+v, want SandboxPool np-pool", owner)
	}
	// Selects the husk pods.
	if np.Spec.PodSelector.MatchLabels["mitos.run/husk"] != "true" {
		t.Errorf("selector = %v, want mitos.run/husk=true", np.Spec.PodSelector.MatchLabels)
	}
	// Default-deny egress posture.
	var hasEgress bool
	for _, pt := range np.Spec.PolicyTypes {
		if pt == networkingv1.PolicyTypeEgress {
			hasEgress = true
		}
	}
	if !hasEgress {
		t.Error("NetworkPolicy must declare the Egress policy type for default-deny egress")
	}
	// DNS rule + one allow rule.
	if len(np.Spec.Egress) != 2 {
		t.Errorf("egress rules = %d, want 2 (DNS + one allow)", len(np.Spec.Egress))
	}

	// Idempotent: a second ensure with a changed allowlist updates in place.
	if err := r.EnsureHuskNetworkPolicyForTest(ctx, &live, nil); err != nil {
		t.Fatalf("ensureHuskNetworkPolicy (update): %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, &np); err != nil {
		t.Fatalf("network policy missing after update: %v", err)
	}
	if len(np.Spec.Egress) != 1 {
		t.Errorf("egress rules after clearing allows = %d, want 1 (DNS only)", len(np.Spec.Egress))
	}
}
