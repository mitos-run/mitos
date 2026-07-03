package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/tenant"
)

// orgStampScheme builds the scheme the fake-client org-stamp tests use.
func orgStampScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

// TestMarkHuskPodClaimedStampsClaimOrgInSharedNamespace is the issue #602
// attribution fix: in a SHARED (non-org) namespace the husk pods carry no
// namespace-derived mitos.run/org label, so every scraped sample was
// unattributed and dropped from billable usage. The hosted gateway stamps the
// Sandbox object's own labels with the org (controlplane/forward.go), so the
// claim path must copy that trusted label onto the pod it claims.
func TestMarkHuskPodClaimedStampsClaimOrgInSharedNamespace(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "husk-1",
		Namespace: "mitos",
		Labels:    map[string]string{huskLabel: "true"},
	}}
	claim := &v1.Sandbox{ObjectMeta: metav1.ObjectMeta{
		Name:      "claim-1",
		Namespace: "mitos",
		Labels:    map[string]string{tenant.OrgLabelKey: "org-x"},
	}}
	c := fakeclient.NewClientBuilder().WithScheme(orgStampScheme(t)).WithObjects(pod).Build()
	r := &SandboxReconciler{Client: c}
	ctx := context.Background()

	var live corev1.Pod
	if err := c.Get(ctx, client.ObjectKeyFromObject(pod), &live); err != nil {
		t.Fatal(err)
	}
	if err := r.markHuskPodClaimed(ctx, &live, claim); err != nil {
		t.Fatalf("markHuskPodClaimed: %v", err)
	}

	var got corev1.Pod
	if err := c.Get(ctx, client.ObjectKeyFromObject(pod), &got); err != nil {
		t.Fatal(err)
	}
	if got.Labels[huskClaimLabel] != "claim-1" {
		t.Errorf("claim label = %q, want claim-1", got.Labels[huskClaimLabel])
	}
	if got.Labels[tenant.OrgLabelKey] != "org-x" {
		t.Errorf("pod org label = %q, want org-x (the claim's trusted org label must be stamped at claim time in a shared namespace)", got.Labels[tenant.OrgLabelKey])
	}
}

// TestMarkHuskPodClaimedOverridesStaleClaimStampedOrg proves a stale org label
// left by an earlier claim (a failed activation of another org's claim) is
// OVERRIDDEN by the winning claim's org, never billed to the earlier org.
func TestMarkHuskPodClaimedOverridesStaleClaimStampedOrg(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "husk-2",
		Namespace: "mitos",
		Labels:    map[string]string{huskLabel: "true", tenant.OrgLabelKey: "org-old"},
	}}
	claim := &v1.Sandbox{ObjectMeta: metav1.ObjectMeta{
		Name:      "claim-2",
		Namespace: "mitos",
		Labels:    map[string]string{tenant.OrgLabelKey: "org-new"},
	}}
	c := fakeclient.NewClientBuilder().WithScheme(orgStampScheme(t)).WithObjects(pod).Build()
	r := &SandboxReconciler{Client: c}
	ctx := context.Background()

	var live corev1.Pod
	if err := c.Get(ctx, client.ObjectKeyFromObject(pod), &live); err != nil {
		t.Fatal(err)
	}
	if err := r.markHuskPodClaimed(ctx, &live, claim); err != nil {
		t.Fatalf("markHuskPodClaimed: %v", err)
	}

	var got corev1.Pod
	if err := c.Get(ctx, client.ObjectKeyFromObject(pod), &got); err != nil {
		t.Fatal(err)
	}
	if got.Labels[tenant.OrgLabelKey] != "org-new" {
		t.Errorf("pod org label = %q, want org-new (a differing org label must be replaced by the claiming sandbox's org)", got.Labels[tenant.OrgLabelKey])
	}
}

// TestMarkHuskPodClaimedNoClaimOrgLeavesPodUnattributed proves a claim without
// an org label (self-host, direct cluster mode) stamps nothing: the pod stays
// unattributed rather than being forced into a default org.
func TestMarkHuskPodClaimedNoClaimOrgLeavesPodUnattributed(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "husk-3",
		Namespace: "mitos",
		Labels:    map[string]string{huskLabel: "true"},
	}}
	claim := &v1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "claim-3", Namespace: "mitos"}}
	c := fakeclient.NewClientBuilder().WithScheme(orgStampScheme(t)).WithObjects(pod).Build()
	r := &SandboxReconciler{Client: c}
	ctx := context.Background()

	var live corev1.Pod
	if err := c.Get(ctx, client.ObjectKeyFromObject(pod), &live); err != nil {
		t.Fatal(err)
	}
	if err := r.markHuskPodClaimed(ctx, &live, claim); err != nil {
		t.Fatalf("markHuskPodClaimed: %v", err)
	}

	var got corev1.Pod
	if err := c.Get(ctx, client.ObjectKeyFromObject(pod), &got); err != nil {
		t.Fatal(err)
	}
	if v, ok := got.Labels[tenant.OrgLabelKey]; ok {
		t.Errorf("pod org label = %q, want absent (no claim org means unattributed, never a default org)", v)
	}
}

// TestMarkHuskPodClaimedNamespaceOrgStaysAuthoritative is the billing trust
// boundary under org tenancy: in a per-org namespace the pod's org label is
// derived from the TRUSTED namespace at pod creation, and a claim-side label
// (which a tenant with direct namespace access could set on its Sandbox) must
// NEVER override it; otherwise a tenant could bill another org.
func TestMarkHuskPodClaimedNamespaceOrgStaysAuthoritative(t *testing.T) {
	ns := tenant.NamespaceForOrg("acme")
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "husk-4",
		Namespace: ns,
		Labels:    map[string]string{huskLabel: "true", tenant.OrgLabelKey: "acme"},
	}}
	claim := &v1.Sandbox{ObjectMeta: metav1.ObjectMeta{
		Name:      "claim-4",
		Namespace: ns,
		Labels:    map[string]string{tenant.OrgLabelKey: "evil"},
	}}
	c := fakeclient.NewClientBuilder().WithScheme(orgStampScheme(t)).WithObjects(pod).Build()
	r := &SandboxReconciler{Client: c}
	ctx := context.Background()

	var live corev1.Pod
	if err := c.Get(ctx, client.ObjectKeyFromObject(pod), &live); err != nil {
		t.Fatal(err)
	}
	if err := r.markHuskPodClaimed(ctx, &live, claim); err != nil {
		t.Fatalf("markHuskPodClaimed: %v", err)
	}

	var got corev1.Pod
	if err := c.Get(ctx, client.ObjectKeyFromObject(pod), &got); err != nil {
		t.Fatal(err)
	}
	if got.Labels[tenant.OrgLabelKey] != "acme" {
		t.Errorf("pod org label = %q, want acme (the namespace-derived org is authoritative; a claim label must not override it)", got.Labels[tenant.OrgLabelKey])
	}
}

// TestUnmarkHuskPodClaimedReleasesClaimStampedOrg proves the failed-activation
// release path also clears a CLAIM-STAMPED org label in a shared namespace, so
// a pod returned to the dormant pool never carries the failed claim's org into
// a later claim that has none (a misattribution).
func TestUnmarkHuskPodClaimedReleasesClaimStampedOrg(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "husk-5",
		Namespace: "mitos",
		Labels: map[string]string{
			huskLabel:          "true",
			huskClaimLabel:     "claim-5",
			tenant.OrgLabelKey: "org-x",
		},
	}}
	c := fakeclient.NewClientBuilder().WithScheme(orgStampScheme(t)).WithObjects(pod).Build()
	r := &SandboxReconciler{Client: c}
	ctx := context.Background()

	var live corev1.Pod
	if err := c.Get(ctx, client.ObjectKeyFromObject(pod), &live); err != nil {
		t.Fatal(err)
	}
	if err := r.unmarkHuskPodClaimed(ctx, &live); err != nil {
		t.Fatalf("unmarkHuskPodClaimed: %v", err)
	}

	var got corev1.Pod
	if err := c.Get(ctx, client.ObjectKeyFromObject(pod), &got); err != nil {
		t.Fatal(err)
	}
	if v, ok := got.Labels[huskClaimLabel]; ok && v != "" {
		t.Errorf("claim label = %q, want cleared", v)
	}
	if v, ok := got.Labels[tenant.OrgLabelKey]; ok {
		t.Errorf("pod org label = %q, want cleared on release in a shared namespace", v)
	}
}

// TestUnmarkHuskPodClaimedKeepsNamespaceDerivedOrg proves the release path in a
// per-org namespace KEEPS the namespace-derived org label: that label was
// stamped at pod creation from the trusted namespace and stays valid for the
// dormant pod's next claim.
func TestUnmarkHuskPodClaimedKeepsNamespaceDerivedOrg(t *testing.T) {
	ns := tenant.NamespaceForOrg("acme")
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "husk-6",
		Namespace: ns,
		Labels: map[string]string{
			huskLabel:          "true",
			huskClaimLabel:     "claim-6",
			tenant.OrgLabelKey: "acme",
		},
	}}
	c := fakeclient.NewClientBuilder().WithScheme(orgStampScheme(t)).WithObjects(pod).Build()
	r := &SandboxReconciler{Client: c}
	ctx := context.Background()

	var live corev1.Pod
	if err := c.Get(ctx, client.ObjectKeyFromObject(pod), &live); err != nil {
		t.Fatal(err)
	}
	if err := r.unmarkHuskPodClaimed(ctx, &live); err != nil {
		t.Fatalf("unmarkHuskPodClaimed: %v", err)
	}

	var got corev1.Pod
	if err := c.Get(ctx, client.ObjectKeyFromObject(pod), &got); err != nil {
		t.Fatal(err)
	}
	if got.Labels[tenant.OrgLabelKey] != "acme" {
		t.Errorf("pod org label = %q, want acme kept (namespace-derived attribution survives a release)", got.Labels[tenant.OrgLabelKey])
	}
}
