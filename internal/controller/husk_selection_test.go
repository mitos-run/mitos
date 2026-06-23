package controller

import (
	"context"
	v1 "mitos.run/mitos/api/v1"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// selectTestScheme is the corev1 + mitos scheme SetControllerReference needs to
// resolve the SandboxPool GVK.
func selectTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 to scheme: %v", err)
	}
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatalf("add v1alpha1 to scheme: %v", err)
	}
	return scheme
}

// huskPoolForSelect returns a pool with a UID so SetControllerReference can
// stamp a controller owner reference the way reconcileHuskPods does in prod.
func huskPoolForSelect(name string) *v1.SandboxPool {
	return &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID("uid-" + name)},
	}
}

// readyHuskPod builds a husk pod that is Running+Ready with a PodIP and the
// pool's husk labels. ownedBy, when non-nil, attaches the controller owner
// reference reconcileHuskPods stamps (Controller=true, BlockOwnerDeletion=true).
func readyHuskPod(t *testing.T, name, poolName, podIP string, ownedBy *v1.SandboxPool) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels:    map[string]string{huskPoolLabel: poolName, huskLabel: "true"},
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			PodIP:      podIP,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	if ownedBy != nil {
		if err := controllerutil.SetControllerReference(ownedBy, pod, selectTestScheme(t)); err != nil {
			t.Fatalf("set controller reference: %v", err)
		}
	}
	return pod
}

// TestSelectDormantHuskPodRequiresControllerOwnerRef proves the warm-slot
// selector only activates husk pods the controller actually created, identified
// by its controller owner reference to the pool. A pod that merely carries the
// husk labels (which any namespace tenant can set) must NOT be selected, because
// activation delivers the claim's secrets and per-sandbox bearer token to the
// pod's self-reported IP: a tenant-planted decoy must never be an activation
// target.
func TestSelectDormantHuskPodRequiresControllerOwnerRef(t *testing.T) {
	pool := huskPoolForSelect("p")

	// A decoy: correct labels, Ready, lower name (would win the name sort), but
	// NO controller owner reference to the pool.
	decoy := readyHuskPod(t, "aaa-decoy", "p", "10.0.0.66", nil)
	// A genuine controller-created warm slot.
	genuine := readyHuskPod(t, "zzz-genuine", "p", "10.0.0.5", pool)

	t.Run("decoy alone is not selected", func(t *testing.T) {
		r := &SandboxReconciler{Client: newAutoscaleFakeClient(t, pool, decoy)}
		got, err := r.selectDormantHuskPod(context.Background(), pool)
		if err != nil {
			t.Fatalf("selectDormantHuskPod: %v", err)
		}
		if got != nil {
			t.Fatalf("selected a pod without the controller owner reference: %s", got.Name)
		}
	})

	t.Run("genuine is selected over a lower-named decoy", func(t *testing.T) {
		r := &SandboxReconciler{Client: newAutoscaleFakeClient(t, pool, decoy, genuine)}
		got, err := r.selectDormantHuskPod(context.Background(), pool)
		if err != nil {
			t.Fatalf("selectDormantHuskPod: %v", err)
		}
		if got == nil || got.Name != "zzz-genuine" {
			t.Fatalf("expected genuine controller-owned pod, got %v", got)
		}
	})
}
