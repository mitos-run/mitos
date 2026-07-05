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

// listClaimedHuskPods lists a claim's claimed husk pods the way the terminate
// paths do (by the mitos.run/claim label the controller stamped), so tests can
// drive recordHuskTerminations through a real list-then-record path instead of
// hand-assembling a pod slice.
func listClaimedHuskPods(t *testing.T, cl client.Client, claim *v1.Sandbox) []corev1.Pod {
	t.Helper()
	var pods corev1.PodList
	if err := cl.List(context.Background(), &pods, client.InNamespace(claim.Namespace), client.MatchingLabels{huskClaimLabel: claim.Name}); err != nil {
		t.Fatalf("list claimed husk pods: %v", err)
	}
	return pods.Items
}

// TestRecordHuskTerminations asserts the claim-release usage hook (issue #682,
// was #664) records one Termination per claimed, ORG-LABELED husk pod: vm-id
// from the pod name, API-visible id from the claim, org from the trusted
// controller-stamped label (never client input), StartedAt from the claim
// status, and a non-zero release instant. An unattributed pod (no org label,
// the self-host path) records nothing: it was never billable.
func TestRecordHuskTerminations(t *testing.T) {
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
				huskLabel:             "true",
				huskClaimLabel:        "sb-82813f5c",
				"mitos.run/org":       "acme",
				tenant.RegionLabelKey: "fra",
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

	r.recordHuskTerminations(claim, listClaimedHuskPods(t, cl, claim), r.now())

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
	if got.Region != "fra" {
		t.Errorf("Region = %q, want fra (issue #712 phase 0, best-effort from the trusted pod label)", got.Region)
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
	r.recordHuskTerminations(claim, listClaimedHuskPods(t, cl, claim), r.now())

	// terminateLifetime then stamps the terminal phase; the later object delete
	// reconciles the claim in this state and must not record a second event.
	claim.Status.Phase = v1.SandboxTerminated
	r.recordHuskTerminations(claim, []corev1.Pod{*pod}, time.Now())
	r.recordHuskTerminations(claim, listClaimedHuskPods(t, cl, claim), r.now())

	terms := r.UsageTerminations.Drain()
	if len(terms) != 1 {
		t.Fatalf("want exactly 1 termination for a lifetime-terminated then deleted claim, got %d: %+v", len(terms), terms)
	}
	if terms[0].VMID != "python-husk-once" {
		t.Errorf("VMID = %q, want python-husk-once", terms[0].VMID)
	}
}

// TestRecordHuskTerminationsNilLogIsNoOp asserts a reconciler without the
// usage wiring (self-host, collector off) records nothing and never panics:
// the hook must be invisible when metering is disabled, even when handed a
// non-empty, org-labeled pod list.
func TestRecordHuskTerminationsNilLogIsNoOp(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fakeclient.NewClientBuilder().WithScheme(scheme).Build()
	r := &SandboxReconciler{Client: cl}

	claim := &v1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sb-x", Namespace: "default"}}
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "python-husk-x",
			Namespace: "default",
			Labels: map[string]string{
				huskLabel:          "true",
				huskClaimLabel:     "sb-x",
				tenant.OrgLabelKey: "acme",
			},
		},
	}
	r.recordHuskTerminations(claim, []corev1.Pod{pod}, time.Now())
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

// TestReconcilePoolRefSweepsLingeringHuskPodOnTerminalPhase covers the review
// finding on issue #688: if the claimed husk pod delete inside terminateLifetime
// ever fails with a transient non-NotFound error AFTER the Terminated status
// already persisted, nothing previously retried the delete, because
// reconcilePoolRef's terminal-phase early return skipped straight past
// terminateLifetime forever. The still-Running, still-labeled pod kept being
// scraped and billed as live usage until the claim's GC TTL deleted the object.
// This asserts the terminal-phase branch itself now sweeps a lingering claimed
// pod, and that doing so records NO new termination event (the claim already
// closed its billing tail when it first went Terminated; recordHuskTerminations'
// one-event phase guard must swallow a second call).
func TestReconcilePoolRefSweepsLingeringHuskPodOnTerminalPhase(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	started := metav1.NewTime(time.Date(2026, 7, 4, 11, 0, 0, 0, time.UTC))
	finished := metav1.NewTime(time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC))
	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-688-sweep", Namespace: "mitos-org-acme"},
		Spec: v1.SandboxSpec{
			Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: "sweep-pool"}},
		},
		Status: v1.SandboxStatus{
			Phase:      v1.SandboxTerminated,
			StartedAt:  &started,
			FinishedAt: &finished,
		},
	}
	// A lingering claimed husk pod: the earlier terminate's delete transiently
	// failed (or never ran), so the VM is still Running even though the claim
	// already reads Terminated.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "python-husk-688-sweep",
			Namespace: "mitos-org-acme",
			Labels: map[string]string{
				huskLabel:          "true",
				huskClaimLabel:     "sb-688-sweep",
				tenant.OrgLabelKey: "acme",
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	cl := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(claim, pod).
		WithStatusSubresource(&v1.Sandbox{}).
		Build()
	frozen := time.Date(2026, 7, 4, 13, 0, 0, 0, time.UTC)
	r := &SandboxReconciler{
		Client:            cl,
		UsageTerminations: usage.NewTerminationLog(),
		Now:               func() time.Time { return frozen },
	}

	if _, err := r.reconcilePoolRef(context.Background(), claim); err != nil {
		t.Fatalf("reconcilePoolRef: %v", err)
	}

	var pods corev1.PodList
	if err := cl.List(context.Background(), &pods, client.InNamespace("mitos-org-acme")); err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 0 {
		t.Fatalf("lingering claimed husk pod not swept on terminal-phase reconcile; %d pods remain", len(pods.Items))
	}

	if got := r.UsageTerminations.Drain(); len(got) != 0 {
		t.Fatalf("terminal-phase sweep recorded %d new termination(s), want 0 (phase guard should swallow it): %+v", len(got), got)
	}
}

// TestFailedClaimSweepRecordsOneTailTermination closes the Failed-branch
// billing gap alongside TestReconcilePoolRefSweepsLingeringHuskPodOnTerminalPhase:
// the husk activation path can fail a claim (SandboxFailed) AFTER the pod is
// already claimed and running, for example the token-secret-write failure in
// reconcileHuskClaim, with no usage tail ever recorded for that pod. Unlike a
// lifetime-terminated claim, a Failed claim never went through
// terminateLifetime, so recordHuskTerminations' Terminated-only phase guard
// does NOT swallow it here: this sweep is what actually closes the billing
// window. Asserts exactly one tail termination is recorded and the lingering
// pod is deleted.
func TestFailedClaimSweepRecordsOneTailTermination(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	started := metav1.NewTime(time.Date(2026, 7, 4, 11, 0, 0, 0, time.UTC))
	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-688-failed", Namespace: "mitos-org-acme"},
		Spec: v1.SandboxSpec{
			Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: "sweep-pool"}},
		},
		Status: v1.SandboxStatus{
			Phase:     v1.SandboxFailed,
			StartedAt: &started,
		},
	}
	// A lingering claimed husk pod: the husk activation path failed the claim
	// after the pod was already claimed and running, so no terminate hook ever
	// recorded a tail for it.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "python-husk-688-failed",
			Namespace: "mitos-org-acme",
			Labels: map[string]string{
				huskLabel:          "true",
				huskClaimLabel:     "sb-688-failed",
				tenant.OrgLabelKey: "acme",
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	cl := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(claim, pod).
		WithStatusSubresource(&v1.Sandbox{}).
		Build()
	frozen := time.Date(2026, 7, 4, 13, 0, 0, 0, time.UTC)
	r := &SandboxReconciler{
		Client:            cl,
		UsageTerminations: usage.NewTerminationLog(),
		Now:               func() time.Time { return frozen },
	}

	if _, err := r.reconcilePoolRef(context.Background(), claim); err != nil {
		t.Fatalf("reconcilePoolRef: %v", err)
	}

	var pods corev1.PodList
	if err := cl.List(context.Background(), &pods, client.InNamespace("mitos-org-acme")); err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 0 {
		t.Fatalf("lingering claimed husk pod not swept on Failed-phase reconcile; %d pods remain", len(pods.Items))
	}

	got := r.UsageTerminations.Drain()
	if len(got) != 1 {
		t.Fatalf("terminations = %d, want exactly 1 (the Failed-branch tail closure), got %+v", len(got), got)
	}
	if got[0].VMID != "python-husk-688-failed" || got[0].OrgID != "acme" || !got[0].At.Equal(frozen) {
		t.Fatalf("termination = %+v, want vm python-husk-688-failed org acme at %v", got[0], frozen)
	}
}

// TestTerminateLifetimeThenDeleteRecordsExactlyOneTail pins the issue #688
// coupling warning called out alongside #687: a lifetime terminate records the
// one true tail event and deletes the claimed husk pod, and the later object
// delete's own record step, which reconciles the claim after it already reads
// Terminated, must not synthesize a second billing event for the same claim.
// To make step 2 exercise the phase guard rather than an empty pod list (the
// terminate already deleted the fixture pod), the test plants a fresh
// LINGERING claimed husk pod before the re-record, simulating a pod that
// survived a failed delete: with a non-empty pod list, only the
// claim.Status.Phase == SandboxTerminated guard in recordHuskTerminations
// stands between the claim and a double-billed tail.
func TestTerminateLifetimeThenDeleteRecordsExactlyOneTail(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	started := metav1.NewTime(time.Date(2026, 7, 4, 11, 0, 0, 0, time.UTC))
	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-688b", Namespace: "mitos-org-acme"},
		Status: v1.SandboxStatus{
			Phase:     v1.SandboxReady,
			StartedAt: &started,
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "python-husk-688b",
			Namespace: "mitos-org-acme",
			Labels: map[string]string{
				huskLabel:          "true",
				huskClaimLabel:     "sb-688b",
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

	// 1. Lifetime terminate: records the one tail event and deletes the pod.
	if _, err := r.terminateLifetime(context.Background(), claim, "IdleTimeout", "idle"); err != nil {
		t.Fatalf("terminateLifetime: %v", err)
	}
	if n := len(r.UsageTerminations.Drain()); n != 1 {
		t.Fatalf("tail events after terminate = %d, want 1", n)
	}

	// 2. Plant a fresh LINGERING claimed husk pod (same labels the fixture pod
	// carried), simulating a pod that survived a failed delete. Without it the
	// re-record below would trivially record nothing because the pod list is
	// empty; with it, only the Terminated phase guard prevents a second event.
	lingering := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "python-husk-688b-lingering",
			Namespace: "mitos-org-acme",
			Labels: map[string]string{
				huskLabel:          "true",
				huskClaimLabel:     "sb-688b",
				tenant.OrgLabelKey: "acme",
			},
		},
	}
	if err := cl.Create(context.Background(), lingering); err != nil {
		t.Fatalf("create lingering pod: %v", err)
	}

	// 3. Simulate the later object delete's record step on the now-Terminated
	// claim: list the claim's claimed husk pods the way reconcileDelete does,
	// then feed that NON-EMPTY list (the lingering pod) into recordHuskTerminations
	// directly, so the phase guard alone must swallow the re-record (one claim,
	// one event).
	var after v1.Sandbox
	if err := cl.Get(context.Background(), types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}, &after); err != nil {
		t.Fatal(err)
	}
	if after.Status.Phase != v1.SandboxTerminated {
		t.Fatalf("phase = %q, want Terminated before the re-record", after.Status.Phase)
	}
	r.recordHuskTerminations(&after, listClaimedHuskPods(t, cl, &after), r.now())
	if n := len(r.UsageTerminations.Drain()); n != 0 {
		t.Fatalf("tail events after delete-time re-record = %d, want 0", n)
	}
}
