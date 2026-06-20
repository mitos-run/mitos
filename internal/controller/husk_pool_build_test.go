package controller_test

// Coverage for the husk-mode pool reconcile (issue #18, pod-native default).
//
// In husk mode a SandboxPool reconcile does BOTH halves, in order:
//   - it FIRST builds the template snapshot on the target nodes via the same
//     forkd CreateTemplate build path the raw-forkd pool uses (the fake forkd's
//     MockEngine records the build, and the registry then reports the node as a
//     holder);
//   - it THEN maintains a warm pool of husk pods pinned to the snapshot-holding
//     nodes, so each husk pod's read-only snapshot hostPath resolves.
//
// The test drives the two halves of a husk-mode reconcile directly (build then
// husk pods) rather than the full Reconcile: the suite runs a manager-level pool
// reconciler that would race the direct one on the pool status subresource. The
// ordering (build first, place pods second) is the production Reconcile order;
// here it is asserted half by half. It asserts the snapshot was built (the node
// becomes a holder), the husk pods exist owned by the pool, and the husk pods
// carry a nodeAffinity pinned to the snapshot-holding node.

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1alpha1 "mitos.run/mitos/api/v1alpha1"
	"mitos.run/mitos/internal/controller"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestHuskPoolBuildsSnapshotAndPlacesPods(t *testing.T) {
	c := k8sClient

	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "huskbuild-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "huskbuild-pool", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "huskbuild-tmpl"},
			Replicas:    1,
		},
	}
	for _, obj := range []client.Object{template, pool} {
		if err := c.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, p := range listHuskPods(t, c, "huskbuild-pool") {
			_ = c.Delete(ctx, &p)
		}
		_ = c.Delete(ctx, pool)
		_ = c.Delete(ctx, template)
	})

	// A fresh registry with no holder of the template yet: the husk reconcile
	// must BUILD it on this fake forkd node before placing husk pods.
	reg := controller.NewNodeRegistry()
	stop, engine, _, err := controller.StartFakeForkdNodeRecording(reg, "kvm-node-A")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)

	r := &controller.SandboxPoolReconciler{
		Client:          c,
		NodeRegistry:    reg,
		EnableHuskPods:  true,
		HuskStubImage:   "mitos-husk-stub:test",
		KVMResourceName: "mitos.run/kvm",
	}

	// Re-fetch the pool so SetControllerReference has the server-populated UID.
	var live v1alpha1.SandboxPool
	if err := c.Get(ctx, client.ObjectKeyFromObject(pool), &live); err != nil {
		t.Fatal(err)
	}

	// First half: build the snapshot. No node holds it yet, so the husk reconcile
	// must BUILD it via the forkd CreateTemplate path.
	if err := r.EnsureTemplateBuiltForTest(ctx, &live, template); err != nil {
		t.Fatalf("ensureTemplateBuilt: %v", err)
	}

	// The snapshot was BUILT on the node: the mock engine recorded the template
	// and the registry now reports the node as a holder.
	if got := engine.GetCapacity().TemplateIDs; len(got) != 1 || got[0] != "huskbuild-tmpl" {
		t.Fatalf("fake forkd templates = %v, want [huskbuild-tmpl] (the snapshot must be built in husk mode)", got)
	}
	if holders := reg.NodesWithTemplate("huskbuild-tmpl"); len(holders) != 1 || holders[0].Name != "kvm-node-A" {
		t.Fatalf("snapshot holders = %v, want [kvm-node-A]", holders)
	}

	// Second half: place the husk pods. They must pin to the snapshot-holding node.
	if _, err := r.ReconcileHuskPodsForTest(ctx, &live, template); err != nil {
		t.Fatalf("reconcileHuskPods: %v", err)
	}

	pods := waitHuskPodCount(t, c, "huskbuild-pool", 1)
	p := pods[0]
	owner := metav1.GetControllerOf(&p)
	if owner == nil || owner.Kind != "SandboxPool" || owner.Name != "huskbuild-pool" {
		t.Fatalf("husk pod owner = %+v, want SandboxPool huskbuild-pool", owner)
	}

	// Placement: the husk pod is pinned to the snapshot-holding node via
	// nodeAffinity (kubernetes.io/hostname In [kvm-node-A]) AND keeps the kvm
	// nodeSelector.
	if p.Spec.NodeSelector["mitos.run/kvm"] != "true" {
		t.Errorf("husk pod nodeSelector = %+v, want mitos.run/kvm=true", p.Spec.NodeSelector)
	}
	aff := p.Spec.Affinity
	if aff == nil || aff.NodeAffinity == nil || aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		t.Fatalf("husk pod has no required nodeAffinity; want a pin to the snapshot-holding node")
	}
	terms := aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 || len(terms[0].MatchExpressions) != 1 {
		t.Fatalf("nodeAffinity terms = %+v, want one hostname match", terms)
	}
	expr := terms[0].MatchExpressions[0]
	if expr.Key != "kubernetes.io/hostname" || expr.Operator != corev1.NodeSelectorOpIn {
		t.Fatalf("nodeAffinity expr = %+v, want kubernetes.io/hostname In", expr)
	}
	if len(expr.Values) != 1 || expr.Values[0] != "kvm-node-A" {
		t.Fatalf("nodeAffinity values = %v, want [kvm-node-A] (the snapshot holder)", expr.Values)
	}
}

// TestHuskPoolBuildRespectsDedicatedNodes is the dedicatedNodes (#172) build
// constraint end to end: a pool with Placement.NodeSelector must build its
// template snapshot ONLY on nodes matching that selector. It registers two fake
// forkd nodes, one matching the placement label and one not, and creates the
// matching corev1.Node objects so the controller's nodesMatchingSelector resolves
// the placement set against real labels. The build must land on the dedicated
// node and skip the shared node; otherwise the snapshot would sit on a node the
// placement-pinned husk pods can never schedule onto, and the pool would never
// converge.
func TestHuskPoolBuildRespectsDedicatedNodes(t *testing.T) {
	c := k8sClient
	const dedicated, shared = "ded-node-172", "shared-node-172"
	const tenantLabel = "mitos.run/tenant"

	// Real k8s Node objects so nodesMatchingSelector (a label List) sees them. The
	// dedicated node carries the placement label; the shared node does not.
	dedNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: dedicated, Labels: map[string]string{tenantLabel: "acme"}}}
	sharedNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: shared}}
	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "ded172-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "ded172-pool", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "ded172-tmpl"},
			Replicas:    1,
			Placement:   &v1alpha1.PoolPlacement{NodeSelector: map[string]string{tenantLabel: "acme"}},
		},
	}
	for _, obj := range []client.Object{dedNode, sharedNode, template, pool} {
		if err := c.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = c.Delete(ctx, pool)
		_ = c.Delete(ctx, template)
		_ = c.Delete(ctx, dedNode)
		_ = c.Delete(ctx, sharedNode)
	})

	// Two fake forkd nodes named to match the k8s Nodes; neither holds the template.
	reg := controller.NewNodeRegistry()
	stopD, engineD, _, err := controller.StartFakeForkdNodeRecording(reg, dedicated)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stopD)
	stopS, engineS, _, err := controller.StartFakeForkdNodeRecording(reg, shared)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stopS)

	r := &controller.SandboxPoolReconciler{
		Client:          c,
		NodeRegistry:    reg,
		EnableHuskPods:  true,
		HuskStubImage:   "mitos-husk-stub:test",
		KVMResourceName: "mitos.run/kvm",
	}

	var live v1alpha1.SandboxPool
	if err := c.Get(ctx, client.ObjectKeyFromObject(pool), &live); err != nil {
		t.Fatal(err)
	}
	if err := r.EnsureTemplateBuiltForTest(ctx, &live, template); err != nil {
		t.Fatalf("ensureTemplateBuilt: %v", err)
	}

	// The dedicated node built the snapshot; the shared node did not.
	if got := engineD.GetCapacity().TemplateIDs; len(got) != 1 || got[0] != "ded172-tmpl" {
		t.Fatalf("dedicated node templates = %v, want [ded172-tmpl]", got)
	}
	if got := engineS.GetCapacity().TemplateIDs; len(got) != 0 {
		t.Fatalf("shared node (outside placement) built %v, want nothing", got)
	}
	holders := reg.NodesWithTemplate("ded172-tmpl")
	if len(holders) != 1 || holders[0].Name != dedicated {
		t.Fatalf("snapshot holders = %v, want [%s]", holders, dedicated)
	}
}
