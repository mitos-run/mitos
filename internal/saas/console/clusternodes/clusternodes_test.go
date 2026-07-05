package clusternodes

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	return s
}

func node(name string, ready bool, kvm, dedicated bool) *corev1.Node {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	labels := map[string]string{}
	if kvm {
		labels[kvmNodeLabel] = "true"
	}
	var taints []corev1.Taint
	if dedicated {
		taints = append(taints, corev1.Taint{Key: dedicatedTaint, Effect: corev1.TaintEffectNoSchedule})
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec:       corev1.NodeSpec{Taints: taints},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("16"),
				corev1.ResourceMemory: resource.MustParse("62Gi"),
			},
		},
	}
}

// TestNodesListsAndMapsFields asserts Nodes reports ready/kvm/dedicated and
// the allocatable quantities as formatted strings for every node.
func TestNodesListsAndMapsFields(t *testing.T) {
	c := fakeclient.NewClientBuilder().
		WithScheme(scheme(t)).
		WithObjects(
			node("kvm-worker-1", true, true, true),
			node("plain-worker-1", false, false, false),
		).
		Build()
	src := New(c)

	views, err := src.Nodes(context.Background())
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("len(views) = %d, want 2", len(views))
	}
	byName := map[string]int{}
	for i, v := range views {
		byName[v.Name] = i
	}

	kvmView := views[byName["kvm-worker-1"]]
	if !kvmView.Ready || !kvmView.KVM || !kvmView.Dedicated {
		t.Errorf("kvm-worker-1 = %+v, want ready=kvm=dedicated=true", kvmView)
	}
	if kvmView.AllocatableCPU != "16" || kvmView.AllocatableMem != "62Gi" {
		t.Errorf("kvm-worker-1 allocatable = cpu=%q mem=%q", kvmView.AllocatableCPU, kvmView.AllocatableMem)
	}

	plainView := views[byName["plain-worker-1"]]
	if plainView.Ready || plainView.KVM || plainView.Dedicated {
		t.Errorf("plain-worker-1 = %+v, want ready=kvm=dedicated=false", plainView)
	}
}

// TestNodesEmptyCluster asserts an empty node list (no error) rather than nil
// or a panic, so the handler's honest-empty-state logic has something
// well-formed to range over.
func TestNodesEmptyCluster(t *testing.T) {
	c := fakeclient.NewClientBuilder().WithScheme(scheme(t)).Build()
	src := New(c)
	views, err := src.Nodes(context.Background())
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	if len(views) != 0 {
		t.Fatalf("len(views) = %d, want 0", len(views))
	}
}
