package controller

// Unit coverage for reapClaimHuskPods, the shared claim-release reaper (issue
// #688): a husk-backed claim's terminate paths must actually STOP the in-pod
// VM by deleting the claimed husk pods, and must record the usage termination
// tail BEFORE the delete (order matters: the collector needs the event to bill
// the [last scrape, terminate] window, and recordHuskTerminations reads the
// pod labels that the delete removes).

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/usage"
)

// TestReapClaimHuskPodsRecordsTailThenDeletes asserts the reaper records ONE
// termination for the claim's org-labeled pod and then deletes that pod, while
// another claim's pod is left alone. This is the issue #688 fix shape: the
// scrape lister selects on labels plus Running, so only deleting the pod stops
// the sandbox from being scraped and billed as live.
func TestReapClaimHuskPodsRecordsTailThenDeletes(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	started := metav1.NewTime(time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC))
	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-reap", Namespace: "mitos-org-acme"},
		Status:     v1.SandboxStatus{Phase: v1.SandboxReady, StartedAt: &started},
	}

	claimed := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "python-husk-reap",
			Namespace: "mitos-org-acme",
			Labels: map[string]string{
				huskLabel:       "true",
				huskClaimLabel:  "sb-reap",
				"mitos.run/org": "acme",
			},
		},
	}
	other := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "python-husk-other",
			Namespace: "mitos-org-acme",
			Labels: map[string]string{
				huskLabel:       "true",
				huskClaimLabel:  "sb-other",
				"mitos.run/org": "acme",
			},
		},
	}

	cl := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(claimed, other).Build()
	frozen := time.Date(2026, 7, 4, 10, 2, 0, 0, time.UTC)
	r := &SandboxReconciler{
		Client:            cl,
		UsageTerminations: usage.NewTerminationLog(),
		Now:               func() time.Time { return frozen },
	}

	if err := r.reapClaimHuskPods(context.Background(), claim); err != nil {
		t.Fatalf("reapClaimHuskPods: %v", err)
	}

	terms := r.UsageTerminations.Drain()
	if len(terms) != 1 {
		t.Fatalf("want exactly 1 termination event, got %d: %+v", len(terms), terms)
	}
	if terms[0].VMID != "python-husk-reap" {
		t.Errorf("VMID = %q, want python-husk-reap", terms[0].VMID)
	}
	if !terms[0].At.Equal(frozen) {
		t.Errorf("At = %v, want the frozen reconciler clock %v", terms[0].At, frozen)
	}

	var gone corev1.Pod
	err := cl.Get(context.Background(), types.NamespacedName{Name: "python-husk-reap", Namespace: "mitos-org-acme"}, &gone)
	if !apierrors.IsNotFound(err) {
		t.Errorf("claimed husk pod still exists after reap (err=%v); the in-pod VM keeps running and billing (issue #688)", err)
	}
	var kept corev1.Pod
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "python-husk-other", Namespace: "mitos-org-acme"}, &kept); err != nil {
		t.Errorf("another claim's pod was deleted by this claim's reap: %v", err)
	}
}

// TestReapClaimHuskPodsTerminatedClaimDeletesWithoutSecondEvent pins the
// one-event-per-claim contract through the reaper: a claim already Terminated
// recorded its event at the lifetime terminate, so a later reap (the object
// delete, or a retry after a failed pod delete) must still delete the pod but
// record NOTHING.
func TestReapClaimHuskPodsTerminatedClaimDeletesWithoutSecondEvent(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-done", Namespace: "mitos-org-acme"},
		Status:     v1.SandboxStatus{Phase: v1.SandboxTerminated},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "python-husk-done",
			Namespace: "mitos-org-acme",
			Labels: map[string]string{
				huskLabel:       "true",
				huskClaimLabel:  "sb-done",
				"mitos.run/org": "acme",
			},
		},
	}
	cl := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := &SandboxReconciler{Client: cl, UsageTerminations: usage.NewTerminationLog()}

	if err := r.reapClaimHuskPods(context.Background(), claim); err != nil {
		t.Fatalf("reapClaimHuskPods: %v", err)
	}

	if terms := r.UsageTerminations.Drain(); len(terms) != 0 {
		t.Fatalf("a Terminated claim's reap recorded %d events, want 0 (one claim, one event): %+v", len(terms), terms)
	}
	var gone corev1.Pod
	err := cl.Get(context.Background(), types.NamespacedName{Name: "python-husk-done", Namespace: "mitos-org-acme"}, &gone)
	if !apierrors.IsNotFound(err) {
		t.Errorf("husk pod still exists after reap of a Terminated claim (err=%v)", err)
	}
}

// TestReapClaimHuskPodsNilUsageLogStillDeletes asserts the reap is lifecycle,
// not billing: a reconciler without usage wiring (self-host, collector off)
// must still delete the claimed pod, and never panic.
func TestReapClaimHuskPodsNilUsageLogStillDeletes(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-selfhost", Namespace: "default"},
		Status:     v1.SandboxStatus{Phase: v1.SandboxReady},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "python-husk-selfhost",
			Namespace: "default",
			Labels:    map[string]string{huskLabel: "true", huskClaimLabel: "sb-selfhost"},
		},
	}
	cl := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := &SandboxReconciler{Client: cl}

	if err := r.reapClaimHuskPods(context.Background(), claim); err != nil {
		t.Fatalf("reapClaimHuskPods: %v", err)
	}
	var gone corev1.Pod
	err := cl.Get(context.Background(), types.NamespacedName{Name: "python-husk-selfhost", Namespace: "default"}, &gone)
	if !apierrors.IsNotFound(err) {
		t.Errorf("husk pod still exists after reap without usage wiring (err=%v); stopping the VM must not depend on billing", err)
	}
}
