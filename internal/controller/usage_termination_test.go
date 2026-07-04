package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/tenant"
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
	frozen := time.Date(2026, 7, 4, 10, 1, 40, 0, time.UTC)
	r := &SandboxReconciler{
		Client:            cl,
		UsageTerminations: usage.NewTerminationLog(),
		Now:               func() time.Time { return frozen },
	}

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
	if !got.At.Equal(frozen) {
		t.Errorf("At = %v, want the reconciler clock's release instant %v (the r.now() seam, so tests can freeze it)", got.At, frozen)
	}
}

// TestRecordHuskTerminationsOncePerClaim pins the one-event-per-claim contract:
// a lifetime-expired claim records its termination at the lifetime terminate
// (the TRUE instant the VM was reaped; the hook runs before the Terminated
// phase is stamped), and the later object delete, which sees the claim already
// Terminated, must record NOTHING. Two events for the same claim are worse
// than one: the first no-op-consumes the collector's finalized guard and the
// second (the one carrying the tail) is then guard-dropped, or, past the
// retention horizon, the second synthesizes a phantom pair.
func TestRecordHuskTerminationsOncePerClaim(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	started := metav1.NewTime(time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC))
	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-once", Namespace: "mitos-org-acme"},
		Status:     v1.SandboxStatus{Phase: v1.SandboxReady, StartedAt: &started},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "python-husk-once",
			Namespace: "mitos-org-acme",
			Labels: map[string]string{
				huskLabel:       "true",
				huskClaimLabel:  "sb-once",
				"mitos.run/org": "acme",
			},
		},
	}
	cl := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := &SandboxReconciler{Client: cl, UsageTerminations: usage.NewTerminationLog()}

	// Lifetime expiry: terminateLifetime records BEFORE stamping Terminated.
	r.recordClaimHuskTerminations(context.Background(), claim)

	// terminateLifetime then stamps the terminal phase; the later object delete
	// reconciles the claim in this state and must not record a second event.
	claim.Status.Phase = v1.SandboxTerminated
	r.recordHuskTerminations(claim, []corev1.Pod{*pod}, time.Now())
	r.recordClaimHuskTerminations(context.Background(), claim)

	terms := r.UsageTerminations.Drain()
	if len(terms) != 1 {
		t.Fatalf("want exactly 1 termination for a lifetime-terminated then deleted claim, got %d: %+v", len(terms), terms)
	}
	if terms[0].VMID != "python-husk-once" {
		t.Errorf("VMID = %q, want python-husk-once", terms[0].VMID)
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

// TestTerminateLifetimeDeletesClaimedHuskPodsAndRecordsTail pins the issue
// #688 fix: a lifetime/idle terminate must delete the claim's claimed husk
// pod, not just record the usage tail and stamp the phase. Terminated has to
// mean the VM actually stopped, otherwise the pod keeps running and keeps
// being scraped and billed after the claim already reads Terminated.
func TestTerminateLifetimeDeletesClaimedHuskPodsAndRecordsTail(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	started := metav1.NewTime(time.Date(2026, 7, 4, 11, 0, 0, 0, time.UTC))
	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-688", Namespace: "mitos-org-acme"},
		Status: v1.SandboxStatus{
			Phase:     v1.SandboxReady,
			StartedAt: &started,
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "python-husk-688",
			Namespace: "mitos-org-acme",
			Labels: map[string]string{
				huskLabel:          "true",
				huskClaimLabel:     "sb-688",
				tenant.OrgLabelKey: "acme",
			},
		},
	}
	cl := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(claim, pod).
		WithStatusSubresource(&v1.Sandbox{}).
		Build()
	frozen := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	r := &SandboxReconciler{
		Client:            cl,
		UsageTerminations: usage.NewTerminationLog(),
		Now:               func() time.Time { return frozen },
	}

	if _, err := r.terminateLifetime(context.Background(), claim, "MaxLifetimeExceeded", "ttl expired"); err != nil {
		t.Fatalf("terminateLifetime: %v", err)
	}

	// The claimed husk pod is DELETED: Terminated must mean the VM stopped (#688).
	var pods corev1.PodList
	if err := cl.List(context.Background(), &pods, client.InNamespace("mitos-org-acme")); err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 0 {
		t.Fatalf("claimed husk pod not deleted; %d pods remain", len(pods.Items))
	}

	// Exactly one tail termination, at the terminate instant.
	got := r.UsageTerminations.Drain()
	if len(got) != 1 {
		t.Fatalf("terminations = %d, want 1", len(got))
	}
	if got[0].VMID != "python-husk-688" || got[0].OrgID != "acme" || !got[0].At.Equal(frozen) {
		t.Fatalf("termination = %+v, want vm python-husk-688 org acme at %v", got[0], frozen)
	}

	// The claim is terminal.
	var after v1.Sandbox
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "sb-688", Namespace: "mitos-org-acme"}, &after); err != nil {
		t.Fatal(err)
	}
	if after.Status.Phase != v1.SandboxTerminated {
		t.Fatalf("phase = %q, want Terminated", after.Status.Phase)
	}
}
