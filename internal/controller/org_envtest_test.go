package controller_test

import (
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/controller"
	"mitos.run/mitos/internal/tenant"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// newOrgReconciler builds an OrgReconciler bound to the envtest client with
// fixed controller defaults the tests assert against.
func newOrgReconciler() *controller.OrgReconciler {
	return &controller.OrgReconciler{
		Client:               k8sClient,
		PoolSecretsSubject:   "mitos-controller",
		PoolSecretsNamespace: "mitos",
		DefaultMaxSandboxes:  50,
		DefaultCPU:           resource.MustParse("32"),
		DefaultMemory:        resource.MustParse("64Gi"),
	}
}

// reconcileOrg drives the OrgReconciler once for the named org.
func reconcileOrg(t *testing.T, r *controller.OrgReconciler, name string) {
	t.Helper()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile org %s: %v", name, err)
	}
}

// TestOrgReconcilerProvisionsIsolationStack asserts a single Org is reconciled
// into a namespace carrying the org label + PSA privileged labels, with the
// ResourceQuota (defaults), LimitRange, default-deny+DNS NetworkPolicy, and the
// mitos-pool-secrets RoleBinding, and that Status goes Ready with Namespace set.
func TestOrgReconcilerProvisionsIsolationStack(t *testing.T) {
	orgID := fmt.Sprintf("acme-%d", time.Now().UnixNano())
	org := &v1.Org{
		ObjectMeta: metav1.ObjectMeta{Name: orgID},
		Spec:       v1.OrgSpec{DisplayName: "Acme Inc"},
	}
	if err := k8sClient.Create(ctx, org); err != nil {
		t.Fatalf("create org: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, org) })

	r := newOrgReconciler()
	reconcileOrg(t, r, orgID)

	ns := tenant.NamespaceForOrg(orgID)

	// Namespace with org label + PSA labels.
	var got corev1.Namespace
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: ns}, &got); err != nil {
		t.Fatalf("get namespace %s: %v", ns, err)
	}
	if got.Labels[tenant.OrgLabelKey] != orgID {
		t.Errorf("namespace org label = %q, want %q", got.Labels[tenant.OrgLabelKey], orgID)
	}
	for _, k := range []string{
		"pod-security.kubernetes.io/enforce",
		"pod-security.kubernetes.io/audit",
		"pod-security.kubernetes.io/warn",
	} {
		if got.Labels[k] != "privileged" {
			t.Errorf("namespace %s = %q, want privileged", k, got.Labels[k])
		}
	}
	// The namespace is owner-referenced to the cluster-scoped Org for cascade GC.
	if !hasOrgOwner(got.OwnerReferences, orgID) {
		t.Errorf("namespace missing Org owner reference: %+v", got.OwnerReferences)
	}

	// ResourceQuota with the controller defaults.
	var rq corev1.ResourceQuota
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "mitos-org-quota", Namespace: ns}, &rq); err != nil {
		t.Fatalf("get resource quota: %v", err)
	}
	if v := rq.Spec.Hard[corev1.ResourcePods]; v.Value() != 50 {
		t.Errorf("quota pods = %v, want 50", v.Value())
	}
	if v := rq.Spec.Hard["count/sandboxes.mitos.run"]; v.Value() != 50 {
		t.Errorf("quota count/sandboxes = %v, want 50", v.Value())
	}
	wantCPU := resource.MustParse("32")
	if v := rq.Spec.Hard[corev1.ResourceLimitsCPU]; v.Cmp(wantCPU) != 0 {
		t.Errorf("quota limits.cpu = %v, want 32", v.String())
	}

	// LimitRange present.
	var lr corev1.LimitRange
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "mitos-org-limits", Namespace: ns}, &lr); err != nil {
		t.Fatalf("get limit range: %v", err)
	}
	if len(lr.Spec.Limits) == 0 {
		t.Errorf("limit range has no limits")
	}

	// Default-deny NetworkPolicy with both directions and a DNS allow.
	var np networkingv1.NetworkPolicy
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "mitos-org-default-deny", Namespace: ns}, &np); err != nil {
		t.Fatalf("get network policy: %v", err)
	}
	if len(np.Spec.PodSelector.MatchLabels) != 0 {
		t.Errorf("netpol PodSelector should be empty (all pods), got %+v", np.Spec.PodSelector)
	}
	if len(np.Spec.Ingress) != 0 {
		t.Errorf("netpol should have no ingress rules (deny all), got %d", len(np.Spec.Ingress))
	}
	if len(np.Spec.Egress) != 1 || len(np.Spec.Egress[0].Ports) != 2 {
		t.Errorf("netpol should have exactly one egress rule (DNS allow, 2 ports), got %+v", np.Spec.Egress)
	}
	gotTypes := map[networkingv1.PolicyType]bool{}
	for _, pt := range np.Spec.PolicyTypes {
		gotTypes[pt] = true
	}
	if !gotTypes[networkingv1.PolicyTypeIngress] || !gotTypes[networkingv1.PolicyTypeEgress] {
		t.Errorf("netpol must declare both Ingress and Egress, got %+v", np.Spec.PolicyTypes)
	}

	// mitos-pool-secrets RoleBinding to the controller ServiceAccount.
	var rb rbacv1.RoleBinding
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "mitos-pool-secrets", Namespace: ns}, &rb); err != nil {
		t.Fatalf("get rolebinding: %v", err)
	}
	if rb.RoleRef.Kind != "ClusterRole" || rb.RoleRef.Name != "mitos-pool-secrets" {
		t.Errorf("rolebinding roleRef = %+v, want ClusterRole/mitos-pool-secrets", rb.RoleRef)
	}
	if len(rb.Subjects) != 1 || rb.Subjects[0].Name != "mitos-controller" || rb.Subjects[0].Kind != "ServiceAccount" {
		t.Errorf("rolebinding subject = %+v, want SA mitos-controller", rb.Subjects)
	}

	// Status Ready with Namespace set.
	var after v1.Org
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: orgID}, &after); err != nil {
		t.Fatalf("get org: %v", err)
	}
	if after.Status.Phase != v1.OrgReady {
		t.Errorf("org phase = %q, want Ready", after.Status.Phase)
	}
	if after.Status.Namespace != ns {
		t.Errorf("org status namespace = %q, want %q", after.Status.Namespace, ns)
	}
	if !hasReadyCondition(after.Status.Conditions) {
		t.Errorf("org missing Ready=True condition: %+v", after.Status.Conditions)
	}
}

// TestOrgReconcilerProvisionsPoolFromTemplate asserts that when a pool template
// is configured the reconciler clones it (full spec, warm.min overridden to 0)
// into the org namespace under OrgPoolName, owner-referenced to the Org, so a
// per-org create has a schedulable pool to fork from (issue #288).
func TestOrgReconcilerProvisionsPoolFromTemplate(t *testing.T) {
	stamp := time.Now().UnixNano()
	// Seed a reference pool in the controller namespace ("mitos"). Ensure the
	// namespace exists first (envtest starts with none).
	mitosNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "mitos"}}
	if err := k8sClient.Create(ctx, mitosNS); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create mitos namespace: %v", err)
	}
	tmplName := fmt.Sprintf("python-%d", stamp)
	tmpl := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: tmplName, Namespace: "mitos"},
		Spec: v1.SandboxPoolSpec{
			Template:  &v1.PoolTemplateSpec{Image: "ghcr.io/mitos-run/mitos-python:v1.13.0"},
			Warm:      &v1.PoolWarm{Min: 8},
			Placement: &v1.PoolPlacement{NodeSelector: map[string]string{"mitos.run/kvm": "true"}},
		},
	}
	if err := k8sClient.Create(ctx, tmpl); err != nil {
		t.Fatalf("create template pool: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, tmpl) })

	orgID := fmt.Sprintf("pooled-%d", stamp)
	org := &v1.Org{ObjectMeta: metav1.ObjectMeta{Name: orgID}}
	if err := k8sClient.Create(ctx, org); err != nil {
		t.Fatalf("create org: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, org) })

	r := newOrgReconciler()
	r.PoolTemplateName = tmplName
	r.PoolTemplateNamespace = "mitos"
	r.OrgPoolName = "python"
	r.OrgPoolWarmMin = 0
	reconcileOrg(t, r, orgID)

	ns := tenant.NamespaceForOrg(orgID)
	var pool v1.SandboxPool
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "python", Namespace: ns}, &pool); err != nil {
		t.Fatalf("get org pool: %v", err)
	}
	if pool.Spec.Template == nil || pool.Spec.Template.Image != "ghcr.io/mitos-run/mitos-python:v1.13.0" {
		t.Errorf("pool template image = %+v, want cloned image", pool.Spec.Template)
	}
	if pool.Spec.Placement == nil || pool.Spec.Placement.NodeSelector["mitos.run/kvm"] != "true" {
		t.Errorf("pool placement not cloned: %+v", pool.Spec.Placement)
	}
	if pool.Spec.Warm == nil || pool.Spec.Warm.Min != 0 {
		t.Errorf("pool warm = %+v, want Min 0 (overridden)", pool.Spec.Warm)
	}
	if pool.Labels[tenant.OrgLabelKey] != orgID {
		t.Errorf("pool org label = %q, want %q", pool.Labels[tenant.OrgLabelKey], orgID)
	}
	if !hasOrgOwner(pool.OwnerReferences, orgID) {
		t.Errorf("pool missing Org owner reference: %+v", pool.OwnerReferences)
	}
}

// TestOrgReconcilerSkipsPoolWhenUnconfigured asserts that with no pool template
// (the self-host / bring-your-own-pool posture) NO pool is created, so a
// single-tenant install is unaffected.
func TestOrgReconcilerSkipsPoolWhenUnconfigured(t *testing.T) {
	orgID := fmt.Sprintf("nopool-%d", time.Now().UnixNano())
	org := &v1.Org{ObjectMeta: metav1.ObjectMeta{Name: orgID}}
	if err := k8sClient.Create(ctx, org); err != nil {
		t.Fatalf("create org: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, org) })

	r := newOrgReconciler() // PoolTemplateName empty
	reconcileOrg(t, r, orgID)

	ns := tenant.NamespaceForOrg(orgID)
	var list v1.SandboxPoolList
	if err := k8sClient.List(ctx, &list, client.InNamespace(ns)); err != nil {
		t.Fatalf("list pools: %v", err)
	}
	if len(list.Items) != 0 {
		t.Fatalf("a pool was created despite no template: %+v", list.Items)
	}
}

// TestOrgReconcilerTwoOrgsAreIsolated asserts two Orgs land in two DISTINCT
// namespaces, each with its own full stack and ONLY its own org label.
func TestOrgReconcilerTwoOrgsAreIsolated(t *testing.T) {
	stamp := time.Now().UnixNano()
	orgA := fmt.Sprintf("orga-%d", stamp)
	orgB := fmt.Sprintf("orgb-%d", stamp)

	for _, id := range []string{orgA, orgB} {
		o := &v1.Org{ObjectMeta: metav1.ObjectMeta{Name: id}}
		if err := k8sClient.Create(ctx, o); err != nil {
			t.Fatalf("create org %s: %v", id, err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, o) })
	}

	r := newOrgReconciler()
	reconcileOrg(t, r, orgA)
	reconcileOrg(t, r, orgB)

	nsA := tenant.NamespaceForOrg(orgA)
	nsB := tenant.NamespaceForOrg(orgB)
	if nsA == nsB {
		t.Fatalf("two orgs mapped to the same namespace %s", nsA)
	}

	// Each namespace exists with its own stack and carries ONLY its own org label.
	for id, ns := range map[string]string{orgA: nsA, orgB: nsB} {
		var got corev1.Namespace
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: ns}, &got); err != nil {
			t.Fatalf("get namespace %s: %v", ns, err)
		}
		if got.Labels[tenant.OrgLabelKey] != id {
			t.Errorf("namespace %s org label = %q, want %q", ns, got.Labels[tenant.OrgLabelKey], id)
		}
		// Its own stack objects exist.
		for _, check := range []struct {
			obj  client.Object
			name string
		}{
			{&corev1.ResourceQuota{}, "mitos-org-quota"},
			{&networkingv1.NetworkPolicy{}, "mitos-org-default-deny"},
			{&rbacv1.RoleBinding{}, "mitos-pool-secrets"},
		} {
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: check.name, Namespace: ns}, check.obj); err != nil {
				t.Errorf("namespace %s missing %s: %v", ns, check.name, err)
			}
		}
	}

	// Org A's namespace does NOT carry org B's label, and vice versa.
	var nsAObj, nsBObj corev1.Namespace
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: nsA}, &nsAObj); err != nil {
		t.Fatalf("get nsA: %v", err)
	}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: nsB}, &nsBObj); err != nil {
		t.Fatalf("get nsB: %v", err)
	}
	if nsAObj.Labels[tenant.OrgLabelKey] == orgB {
		t.Errorf("org A namespace carries org B's label")
	}
	if nsBObj.Labels[tenant.OrgLabelKey] == orgA {
		t.Errorf("org B namespace carries org A's label")
	}
}

// TestOrgReconcilerQuotaOverride asserts an Org's spec.quota override drives the
// ResourceQuota instead of the controller defaults.
func TestOrgReconcilerQuotaOverride(t *testing.T) {
	orgID := fmt.Sprintf("bigco-%d", time.Now().UnixNano())
	org := &v1.Org{
		ObjectMeta: metav1.ObjectMeta{Name: orgID},
		Spec: v1.OrgSpec{
			Tier: "enterprise",
			Quota: &v1.OrgQuota{
				MaxSandboxes: 500,
				MaxPods:      600,
				CPU:          resource.MustParse("256"),
				Memory:       resource.MustParse("512Gi"),
			},
		},
	}
	if err := k8sClient.Create(ctx, org); err != nil {
		t.Fatalf("create org: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, org) })

	r := newOrgReconciler()
	reconcileOrg(t, r, orgID)

	ns := tenant.NamespaceForOrg(orgID)
	var rq corev1.ResourceQuota
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "mitos-org-quota", Namespace: ns}, &rq); err != nil {
		t.Fatalf("get resource quota: %v", err)
	}
	if v := rq.Spec.Hard["count/sandboxes.mitos.run"]; v.Value() != 500 {
		t.Errorf("quota count/sandboxes = %v, want 500 (override)", v.Value())
	}
	if v := rq.Spec.Hard[corev1.ResourcePods]; v.Value() != 600 {
		t.Errorf("quota pods = %v, want 600 (override)", v.Value())
	}
	wantCPU := resource.MustParse("256")
	if v := rq.Spec.Hard[corev1.ResourceLimitsCPU]; v.Cmp(wantCPU) != 0 {
		t.Errorf("quota limits.cpu = %v, want 256 (override)", v.String())
	}
}

// TestOrgReconcilerDeletionCascades asserts that deleting the Org marks the
// namespace for garbage collection via the owner reference. envtest does not run
// the GC controller, so we assert the owner reference (the GC trigger) rather
// than waiting for the namespace to disappear.
func TestOrgReconcilerDeletionCascades(t *testing.T) {
	orgID := fmt.Sprintf("gone-%d", time.Now().UnixNano())
	org := &v1.Org{ObjectMeta: metav1.ObjectMeta{Name: orgID}}
	if err := k8sClient.Create(ctx, org); err != nil {
		t.Fatalf("create org: %v", err)
	}

	r := newOrgReconciler()
	reconcileOrg(t, r, orgID)

	ns := tenant.NamespaceForOrg(orgID)
	var nsObj corev1.Namespace
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: ns}, &nsObj); err != nil {
		t.Fatalf("get namespace: %v", err)
	}
	// The owner reference to the cluster-scoped Org is what makes the namespace
	// garbage-collected when the Org is deleted.
	ref := orgOwnerRef(nsObj.OwnerReferences, orgID)
	if ref == nil {
		t.Fatalf("namespace has no Org owner reference; deletion would not cascade: %+v", nsObj.OwnerReferences)
	}
	if ref.BlockOwnerDeletion == nil || !*ref.BlockOwnerDeletion {
		// controllerutil.SetControllerReference sets BlockOwnerDeletion + Controller;
		// both are expected on a controller owner ref.
		t.Errorf("owner ref BlockOwnerDeletion not set: %+v", ref)
	}
	if ref.Controller == nil || !*ref.Controller {
		t.Errorf("owner ref Controller not set: %+v", ref)
	}

	// Deleting the Org succeeds; a second reconcile after deletion is a no-op (the
	// reconciler ignores NotFound and relies on owner-ref GC).
	if err := k8sClient.Delete(ctx, org); err != nil {
		t.Fatalf("delete org: %v", err)
	}
	reconcileOrg(t, r, orgID) // NotFound path: must not error.
}

func hasOrgOwner(refs []metav1.OwnerReference, orgID string) bool {
	return orgOwnerRef(refs, orgID) != nil
}

func orgOwnerRef(refs []metav1.OwnerReference, orgID string) *metav1.OwnerReference {
	for i := range refs {
		if refs[i].Kind == "Org" && refs[i].Name == orgID {
			return &refs[i]
		}
	}
	return nil
}

func hasReadyCondition(conds []metav1.Condition) bool {
	for _, c := range conds {
		if c.Type == "Ready" && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}
