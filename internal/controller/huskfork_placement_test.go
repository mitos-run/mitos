package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtimescheme "k8s.io/apimachinery/pkg/runtime"

	v1 "mitos.run/mitos/api/v1"
)

// A fork child is pinned to the SOURCE sandbox's node (ForkSourceNode). For a
// dedicated pool that node is tainted (mitos.run/dedicated, workload=sandbox)
// and selected by a nodeSelector (mitos.run/kvm). buildForkChildPod builds from
// an EMPTY pool template, so without inheriting the source pod's placement the
// child carries no tolerations and is unschedulable (Pending) until the create
// ready deadline elapses. This is the root cause of the hosted live fork
// timeout observed in prod: the child sandbox sits in Restoring while its husk
// pod cannot schedule. The fix threads the source pod's tolerations and
// nodeSelector through HuskPodOptions.
func TestBuildForkChildPodInheritsSourcePlacement(t *testing.T) {
	scheme := runtimescheme.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	sourceTolerations := []corev1.Toleration{
		{Key: "workload", Operator: corev1.TolerationOpEqual, Value: "sandbox", Effect: corev1.TaintEffectNoSchedule},
		{Key: "mitos.run/dedicated", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
	}
	sourceNodeSelector := map[string]string{"mitos.run/kvm": "true", "pool": "dedicated"}

	fork := &v1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sb-child-1", Namespace: "mitos", UID: "uid-fork"}}
	pod := buildForkChildPod(fork, "sb-child-1-0", HuskPodOptions{
		StubImage:              "img",
		SnapshotID:             "tmpl-a",
		DataDir:                "/data",
		KVMResourceName:        "mitos.run/kvm",
		ForkSnapshotID:         "sb-child-1",
		ForkSourceNode:         "mitos-kvm-1",
		ForkSourceTolerations:  sourceTolerations,
		ForkSourceNodeSelector: sourceNodeSelector,
	}, scheme)

	// Every source toleration must be present on the child, else it cannot land
	// on the tainted dedicated node it is pinned to.
	for _, want := range sourceTolerations {
		found := false
		for _, got := range pod.Spec.Tolerations {
			if got.Key == want.Key && got.Value == want.Value && got.Effect == want.Effect && got.Operator == want.Operator {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("fork child missing source toleration %+v; child would sit Pending on the tainted source node. tolerations=%+v", want, pod.Spec.Tolerations)
		}
	}

	// The source nodeSelector labels must be present (merged with any kvm
	// selector buildHuskPod set), so the child targets the same node class.
	for k, v := range sourceNodeSelector {
		if pod.Spec.NodeSelector[k] != v {
			t.Fatalf("fork child nodeSelector[%q] = %q, want %q; selector=%v", k, pod.Spec.NodeSelector[k], v, pod.Spec.NodeSelector)
		}
	}
}

// Guard the regression directly: a fork child built WITHOUT source placement
// (the pre-fix behavior) carries no tolerations, which is what left it Pending.
// This documents that the inheritance, not buildHuskPod, supplies them.
func TestBuildForkChildPodWithoutSourcePlacementHasNoTolerations(t *testing.T) {
	scheme := runtimescheme.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	fork := &v1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sb-child-2", Namespace: "mitos", UID: "uid-fork-2"}}
	pod := buildForkChildPod(fork, "sb-child-2-0", HuskPodOptions{
		StubImage:      "img",
		SnapshotID:     "tmpl-a",
		DataDir:        "/data",
		ForkSnapshotID: "sb-child-2",
		ForkSourceNode: "mitos-kvm-1",
	}, scheme)
	// The pod carries k8s's default not-ready/unreachable NoExecute tolerations,
	// but NONE of the dedicated-node taints a pool pins its husks to. Without
	// inheritance the child cannot land on the tainted source node.
	for _, tol := range pod.Spec.Tolerations {
		if tol.Key == "workload" || tol.Key == "mitos.run/dedicated" {
			t.Fatalf("did not expect a dedicated-node toleration without source placement, got %+v", tol)
		}
	}
}
