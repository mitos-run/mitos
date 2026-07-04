package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/usage"
)

// TestRecordClaimHuskTerminations asserts the claim-release usage hook (issue
// #682, was #664) records one Termination per claimed, ORG-LABELED husk pod:
// vm-id from the pod name, API-visible id from the claim, org from the trusted
// controller-stamped label (never client input), StartedAt from the claim
// status, and a non-zero release instant. An unattributed pod (no org label,
// the self-host path) records nothing: it was never billable.
func TestRecordClaimHuskTerminations(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	started := metav1.NewTime(time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC))
	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-82813f5c", Namespace: "mitos-org-acme"},
		Status:     v1.SandboxStatus{StartedAt: &started},
	}

	billable := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "python-husk-2blsp",
			Namespace: "mitos-org-acme",
			Labels: map[string]string{
				huskLabel:       "true",
				huskClaimLabel:  "sb-82813f5c",
				"mitos.run/org": "acme",
			},
		},
	}
	// Claimed by the same claim but carries no org label (self-host
	// single-tenant): never billable, so never recorded.
	unattributed := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "python-husk-noorg",
			Namespace: "mitos-org-acme",
			Labels:    map[string]string{huskLabel: "true", huskClaimLabel: "sb-82813f5c"},
		},
	}
	// A different claim's pod: out of scope for this release.
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

	cl := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(billable, unattributed, other).Build()
	r := &SandboxReconciler{Client: cl, UsageTerminations: usage.NewTerminationLog()}

	r.recordClaimHuskTerminations(context.Background(), claim)

	terms := r.UsageTerminations.Drain()
	if len(terms) != 1 {
		t.Fatalf("want exactly 1 termination (the org-labeled pod of THIS claim), got %+v", terms)
	}
	got := terms[0]
	if got.VMID != "python-husk-2blsp" {
		t.Errorf("VMID = %q, want the husk pod name python-husk-2blsp", got.VMID)
	}
	if got.APIID != "sb-82813f5c" {
		t.Errorf("APIID = %q, want the customer-visible claim name sb-82813f5c", got.APIID)
	}
	if got.OrgID != "acme" {
		t.Errorf("OrgID = %q, want acme (from the trusted controller-stamped label)", got.OrgID)
	}
	if !got.StartedAt.Equal(started.Time) {
		t.Errorf("StartedAt = %v, want the claim's %v", got.StartedAt, started.Time)
	}
	if got.At.IsZero() {
		t.Error("At is zero, want the release instant")
	}
}

// TestRecordClaimHuskTerminationsNilLogIsNoOp asserts a reconciler without the
// usage wiring (self-host, collector off) records nothing and never panics:
// the hook must be invisible when metering is disabled.
func TestRecordClaimHuskTerminationsNilLogIsNoOp(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fakeclient.NewClientBuilder().WithScheme(scheme).Build()
	r := &SandboxReconciler{Client: cl}

	claim := &v1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sb-x", Namespace: "default"}}
	r.recordClaimHuskTerminations(context.Background(), claim)
	r.recordHuskTerminations(claim, nil, time.Now())
}
