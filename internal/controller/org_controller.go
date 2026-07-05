package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/tenant"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Org-namespace stack object names. They are FIXED per org namespace (one of
// each), so the reconcile is a create-or-update keyed by name, not a fan-out.
const (
	orgQuotaName       = "mitos-org-quota"
	orgLimitRangeName  = "mitos-org-limits"
	orgDenyPolicyName  = "mitos-org-default-deny"
	orgPoolSecretsName = "mitos-pool-secrets"
)

// orgReadyCondition is the condition type the reconciler stamps on an Org once
// its namespace stack is fully provisioned (or Failed mid-way).
const orgReadyCondition = "Ready"

// OrgReconciler provisions and self-heals the per-org isolation namespace
// (issue #288, the hosted-SaaS multi-tenant boundary). For each Org it ensures
// mitos-org-<id> exists with the full isolation stack: PSA privileged labels, a
// ResourceQuota ceiling, a LimitRange, a default-deny NetworkPolicy with a DNS
// egress allow, and the mitos-pool-secrets RoleBinding. Every object is
// owner-referenced to the cluster-scoped Org so org deletion cascades.
//
// SECURITY: this is the cross-tenant isolation boundary. Two orgs land in two
// distinct namespaces; the default-deny NetworkPolicy plus separate namespaces
// mean cross-org pods cannot reach each other, and the per-org ResourceQuota
// bounds the blast radius of one org's abuse. The microVM remains the in-pod
// isolation boundary; the namespace is the cross-tenant control-plane boundary.
type OrgReconciler struct {
	client.Client

	// PoolSecretsSubject is the ServiceAccount the per-org mitos-pool-secrets
	// RoleBinding grants the mitos-pool-secrets ClusterRole to, so the controller
	// can manage pool Secrets inside the org namespace. Empty defaults to
	// mitos-controller in PoolSecretsNamespace.
	PoolSecretsSubject string
	// PoolSecretsNamespace is the namespace of PoolSecretsSubject (the controller's
	// own namespace). Empty defaults to "mitos".
	PoolSecretsNamespace string

	// DefaultMaxSandboxes is the per-org sandbox/pods count ceiling applied when an
	// Org sets no Quota override. It is the abuse-control default.
	DefaultMaxSandboxes int32
	// DefaultCPU is the per-org aggregate CPU limit ceiling applied when an Org sets
	// no Quota override.
	DefaultCPU resource.Quantity
	// DefaultMemory is the per-org aggregate memory limit ceiling applied when an
	// Org sets no Quota override.
	DefaultMemory resource.Quantity

	// PoolTemplateName is the name of a reference SandboxPool (in
	// PoolTemplateNamespace) whose spec is cloned into every org namespace so a
	// per-org create has a pool to fork from. Cloning the full spec (template,
	// resources, placement, snapshots, network) from ONE source of truth is what
	// makes the per-org pool schedulable on the same nodes as the reference pool,
	// which a bare image cannot. Empty disables per-org pool provisioning (the
	// self-host / bring-your-own-pool posture), so a single-tenant install is
	// unaffected.
	PoolTemplateName string
	// PoolTemplateNamespace is where the reference pool lives. Empty defaults to
	// the controller's own namespace (PoolSecretsNamespace, else "mitos").
	PoolTemplateNamespace string
	// OrgPoolName is the name the cloned pool is created under in each org
	// namespace. It MUST match the pool name a create resolves (the SDK passes it
	// as the image/pool argument). Empty defaults to PoolTemplateName.
	OrgPoolName string
	// OrgPoolWarmMin overrides warm.min on the cloned per-org pool. Default 0: no
	// dormant warm husks are held, so N per-org pools cost nothing at rest (the
	// snapshot is content-addressed and deduped per node); the first fork per org
	// cold-starts and the pool warms on demand.
	OrgPoolWarmMin int32
}

const (
	// defaultPoolSecretsSubject is the controller ServiceAccount the per-org
	// RoleBinding binds when no subject is configured.
	defaultPoolSecretsSubject = "mitos-controller"
	// defaultControllerNamespace is the controller's own namespace fallback.
	defaultControllerNamespace = "mitos"
)

// +kubebuilder:rbac:groups=mitos.run,resources=orgs,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=mitos.run,resources=orgs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=create;delete;get;list;patch;update;watch
// +kubebuilder:rbac:groups="",resources=resourcequotas;limitranges,verbs=create;delete;get;list;patch;update;watch

// Reconcile provisions (or self-heals) the org namespace stack for one Org.
func (r *OrgReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var org v1.Org
	if err := r.Get(ctx, req.NamespacedName, &org); err != nil {
		// Not found: the Org was deleted. The namespace and its stack are
		// owner-referenced to the Org, so Kubernetes garbage collection reaps them;
		// nothing to do here.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	ns := tenant.NamespaceForOrg(org.Name)

	if err := r.ensureStack(ctx, &org, ns); err != nil {
		logger.Error(err, "provision org namespace stack failed", "org", org.Name, "namespace", ns)
		// Surface a Failed phase + condition and requeue. The error already carries
		// actionable context (which object failed); it carries no secret values.
		if serr := r.writeStatus(ctx, &org, ns, v1.OrgFailed, metav1.ConditionFalse, "ProvisionFailed", err.Error()); serr != nil {
			return ctrl.Result{}, fmt.Errorf("write failed status for org %s: %w", org.Name, serr)
		}
		return ctrl.Result{}, fmt.Errorf("provision org namespace stack for %s: %w", org.Name, err)
	}

	if err := r.writeStatus(ctx, &org, ns, v1.OrgReady, metav1.ConditionTrue, "Provisioned", "org namespace and isolation stack are provisioned"); err != nil {
		return ctrl.Result{}, fmt.Errorf("write ready status for org %s: %w", org.Name, err)
	}
	return ctrl.Result{}, nil
}

// ensureStack idempotently provisions every object in the org isolation stack,
// each owner-referenced to the Org for cascade deletion.
func (r *OrgReconciler) ensureStack(ctx context.Context, org *v1.Org, ns string) error {
	if err := r.ensureNamespace(ctx, org, ns); err != nil {
		return err
	}
	if err := r.ensureResourceQuota(ctx, org, ns); err != nil {
		return err
	}
	if err := r.ensureLimitRange(ctx, org, ns); err != nil {
		return err
	}
	if err := r.ensureDenyPolicy(ctx, org, ns); err != nil {
		return err
	}
	if err := r.ensurePoolSecretsRoleBinding(ctx, org, ns); err != nil {
		return err
	}
	if err := r.ensureOrgPool(ctx, org, ns); err != nil {
		return err
	}
	return nil
}

// ensureOrgPool clones the reference SandboxPool (PoolTemplateNamespace/
// PoolTemplateName) into the org namespace when a template is configured. It is a
// no-op otherwise (the self-host / bring-your-own-pool posture), so a
// single-tenant install is unaffected. Cloning the full spec carries placement,
// resources, snapshots, and network from one source of truth, so the per-org
// pool schedules exactly where the reference pool does. The pool is created ONLY
// (never updated): once it exists the reconciler leaves its spec alone, so a
// warm.min bumped by the autoscaler or an operator persists. Owner-referenced to
// the Org for cascade deletion.
func (r *OrgReconciler) ensureOrgPool(ctx context.Context, org *v1.Org, ns string) error {
	if r.PoolTemplateName == "" {
		return nil
	}
	tns := r.PoolTemplateNamespace
	if tns == "" {
		tns = r.PoolSecretsNamespace
	}
	if tns == "" {
		tns = defaultControllerNamespace
	}
	name := r.OrgPoolName
	if name == "" {
		name = r.PoolTemplateName
	}

	// Create-once: if the per-org pool already exists, leave it alone.
	existing := &v1.SandboxPool{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	if err := r.Get(ctx, client.ObjectKeyFromObject(existing), existing); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get org pool %s/%s: %w", ns, name, err)
	}

	var tmpl v1.SandboxPool
	if err := r.Get(ctx, client.ObjectKey{Namespace: tns, Name: r.PoolTemplateName}, &tmpl); err != nil {
		// A missing template is an operator misconfiguration, not a transient: fail
		// the reconcile so it surfaces on the Org status with actionable context.
		return fmt.Errorf("get org pool template %s/%s: %w", tns, r.PoolTemplateName, err)
	}

	desired := buildOrgPoolFromTemplate(org, ns, name, &tmpl, r.OrgPoolWarmMin)
	if err := r.setOwner(org, desired); err != nil {
		return err
	}
	if err := r.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create org pool %s/%s: %w", ns, name, err)
	}
	return nil
}

// buildOrgPoolFromTemplate builds a per-org SandboxPool by deep-copying the
// reference pool's spec and overriding only warm.min. Placement, resources,
// snapshots, template image, and network are carried verbatim so the per-org
// pool schedules exactly like the reference. warmMin defaults the pool to no
// idle warm husks (0) so N per-org pools cost nothing at rest.
func buildOrgPoolFromTemplate(org *v1.Org, ns, name string, tmpl *v1.SandboxPool, warmMin int32) *v1.SandboxPool {
	spec := *tmpl.Spec.DeepCopy()
	if spec.Warm == nil {
		spec.Warm = &v1.PoolWarm{}
	}
	spec.Warm.Min = warmMin
	return &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    tenant.OrgLabels(org.Name),
		},
		Spec: spec,
	}
}

// ensureNamespace creates or updates the org namespace with the org label and
// the PSA privileged labels.
//
// PSA NOTE: enforce=privileged is about what a POD in this namespace may
// REQUEST (a husk pod needs /dev/kvm via the device plugin, NET_ADMIN for the
// in-pod egress firewall, and a short-lived privileged init container for
// name-egress pools). It is NOT a cross-tenant access control: the cross-org
// boundary is the separate namespace + the default-deny NetworkPolicy + the
// microVM, none of which privileged PSA weakens. See docs/threat-model.md.
func (r *OrgReconciler) ensureNamespace(ctx context.Context, org *v1.Org, name string) error {
	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, nsObj, func() error {
		if nsObj.Labels == nil {
			nsObj.Labels = map[string]string{}
		}
		for k, v := range orgNamespaceLabels(org.Name) {
			nsObj.Labels[k] = v
		}
		return r.setOwner(org, nsObj)
	})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("ensure namespace %s: %w", name, err)
	}
	return nil
}

// orgNamespaceLabels is the label set on an org namespace: the org ownership
// label plus the three PSA labels (enforce/audit/warn = privileged).
func orgNamespaceLabels(orgID string) map[string]string {
	l := tenant.OrgLabels(orgID)
	l["pod-security.kubernetes.io/enforce"] = "privileged"
	l["pod-security.kubernetes.io/audit"] = "privileged"
	l["pod-security.kubernetes.io/warn"] = "privileged"
	return l
}

// ensureResourceQuota creates or updates the per-org ResourceQuota: the
// abuse-control ceiling. Counts and limits come from the Org's Quota override or
// the controller defaults.
func (r *OrgReconciler) ensureResourceQuota(ctx context.Context, org *v1.Org, ns string) error {
	desired := buildOrgResourceQuota(org, ns, r.DefaultMaxSandboxes, r.DefaultCPU, r.DefaultMemory)
	rq := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: ns}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, rq, func() error {
		rq.Labels = desired.Labels
		rq.Spec = desired.Spec
		return r.setOwner(org, rq)
	})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("ensure resource quota in %s: %w", ns, err)
	}
	return nil
}

// buildOrgResourceQuota builds the per-org ResourceQuota. A field set on the
// Org's Quota override wins; an unset (zero) field falls back to the controller
// default, so a partial override keeps the defaults for the rest. This is the
// per-org sandbox/CPU/memory ceiling and the abuse-control primitive.
func buildOrgResourceQuota(org *v1.Org, ns string, defMaxSandboxes int32, defCPU, defMemory resource.Quantity) *corev1.ResourceQuota {
	maxSandboxes := defMaxSandboxes
	maxPods := defMaxSandboxes
	cpu := defCPU
	mem := defMemory
	if q := org.Spec.Quota; q != nil {
		if q.MaxSandboxes > 0 {
			maxSandboxes = q.MaxSandboxes
		}
		if q.MaxPods > 0 {
			maxPods = q.MaxPods
		} else if q.MaxSandboxes > 0 {
			// No explicit MaxPods but an explicit MaxSandboxes: keep pods aligned to
			// the sandbox ceiling rather than the (now overridden) default.
			maxPods = q.MaxSandboxes
		}
		if !q.CPU.IsZero() {
			cpu = q.CPU
		}
		if !q.Memory.IsZero() {
			mem = q.Memory
		}
	}

	hard := corev1.ResourceList{
		corev1.ResourcePods:         *resource.NewQuantity(int64(maxPods), resource.DecimalSI),
		"count/sandboxes.mitos.run": *resource.NewQuantity(int64(maxSandboxes), resource.DecimalSI),
		corev1.ResourceLimitsCPU:    cpu,
		corev1.ResourceLimitsMemory: mem,
	}

	return &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      orgQuotaName,
			Namespace: ns,
			Labels:    tenant.OrgLabels(org.Name),
		},
		Spec: corev1.ResourceQuotaSpec{Hard: hard},
	}
}

// ensureLimitRange creates or updates the per-org LimitRange: a sane default
// container CPU/memory limit so pods that omit limits still count against the
// ResourceQuota (a ResourceQuota on limits.* requires every pod to carry limits).
func (r *OrgReconciler) ensureLimitRange(ctx context.Context, org *v1.Org, ns string) error {
	desired := buildOrgLimitRange(org, ns)
	lr := &corev1.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: ns}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, lr, func() error {
		lr.Labels = desired.Labels
		lr.Spec = desired.Spec
		return r.setOwner(org, lr)
	})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("ensure limit range in %s: %w", ns, err)
	}
	return nil
}

// buildOrgLimitRange builds the per-org default container LimitRange.
func buildOrgLimitRange(org *v1.Org, ns string) *corev1.LimitRange {
	return &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{
			Name:      orgLimitRangeName,
			Namespace: ns,
			Labels:    tenant.OrgLabels(org.Name),
		},
		Spec: corev1.LimitRangeSpec{
			Limits: []corev1.LimitRangeItem{{
				Type: corev1.LimitTypeContainer,
				Default: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
				DefaultRequest: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
			}},
		},
	}
}

// ensureDenyPolicy creates or updates the per-org default-deny NetworkPolicy.
func (r *OrgReconciler) ensureDenyPolicy(ctx context.Context, org *v1.Org, ns string) error {
	desired := buildOrgDefaultDenyPolicy(org, ns)
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: ns}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		np.Labels = desired.Labels
		np.Spec = desired.Spec
		return r.setOwner(org, np)
	})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("ensure default-deny network policy in %s: %w", ns, err)
	}
	return nil
}

// buildOrgDefaultDenyPolicy builds the org's default-deny NetworkPolicy: it
// selects ALL pods in the namespace (empty PodSelector) and declares both
// Ingress and Egress policy types with NO ingress rules (deny all ingress) and a
// single egress rule that allows only cluster DNS (UDP/TCP 53). Pods can resolve
// names but cannot otherwise egress or be reached, so cross-org pods (in separate
// namespaces) cannot reach each other.
//
// HONEST CNI CAVEAT: a NetworkPolicy only enforces if the cluster CNI implements
// it (Calico, Cilium, etc.). On a CNI without NetworkPolicy support this object
// is inert. For the per-org boundary it is the primary cross-namespace control,
// so a NetworkPolicy-enforcing CNI is REQUIRED in a hosted multi-tenant cluster;
// the in-pod nftables filter the husk-stub programs remains the per-pod egress
// guarantee regardless of CNI. Documented in docs/threat-model.md (mirrors the
// pattern in husknetworkpolicy.go).
func buildOrgDefaultDenyPolicy(org *v1.Org, ns string) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP
	dnsPort := intstr.FromInt(53)

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      orgDenyPolicyName,
			Namespace: ns,
			Labels:    tenant.OrgLabels(org.Name),
		},
		Spec: networkingv1.NetworkPolicySpec{
			// Empty selector: applies to every pod in the org namespace.
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			// No ingress rules: deny all ingress.
			Ingress: nil,
			// Egress: allow only cluster DNS so pods can still resolve; everything
			// else is denied by the otherwise-empty egress rule set.
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				Ports: []networkingv1.NetworkPolicyPort{
					{Protocol: &udp, Port: &dnsPort},
					{Protocol: &tcp, Port: &dnsPort},
				},
			}},
		},
	}
}

// ensurePoolSecretsRoleBinding binds the existing cluster-wide mitos-pool-secrets
// ClusterRole (a DEFINITION shipped by the chart) to the controller's
// ServiceAccount inside the org namespace, so the controller can manage pool
// Secrets there without a cluster-wide Secrets grant. Mirrors the intent of
// deploy/charts/mitos/templates/pool-secrets-rbac.yaml, per org namespace.
func (r *OrgReconciler) ensurePoolSecretsRoleBinding(ctx context.Context, org *v1.Org, ns string) error {
	subject := r.PoolSecretsSubject
	if subject == "" {
		subject = defaultPoolSecretsSubject
	}
	subjectNS := r.PoolSecretsNamespace
	if subjectNS == "" {
		subjectNS = defaultControllerNamespace
	}

	rb := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: orgPoolSecretsName, Namespace: ns}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, rb, func() error {
		rb.Labels = tenant.OrgLabels(org.Name)
		rb.RoleRef = rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     orgPoolSecretsName,
		}
		rb.Subjects = []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      subject,
			Namespace: subjectNS,
		}}
		return r.setOwner(org, rb)
	})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("ensure pool-secrets rolebinding in %s: %w", ns, err)
	}
	return nil
}

// setOwner sets the Org as the controller owner of obj (idempotent for the same
// owner). The cluster-scoped Org owning a cluster-scoped Namespace, and the Org
// owning namespaced objects, are both valid owner edges for cascade GC.
func (r *OrgReconciler) setOwner(org *v1.Org, obj client.Object) error {
	if metav1.GetControllerOf(obj) != nil {
		return nil
	}
	if err := controllerutil.SetControllerReference(org, obj, r.Scheme()); err != nil {
		return fmt.Errorf("set owner on %s/%s: %w", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName(), err)
	}
	return nil
}

// writeStatus updates the Org status (namespace, phase, condition, observed
// generation) only when something changed, to avoid a status-write hot loop.
func (r *OrgReconciler) writeStatus(ctx context.Context, org *v1.Org, ns string, phase v1.OrgPhase, condStatus metav1.ConditionStatus, reason, msg string) error {
	before := org.Status.DeepCopy()

	org.Status.Namespace = ns
	org.Status.Phase = phase
	org.Status.ObservedGeneration = org.Generation
	setOrgCondition(&org.Status, metav1.Condition{
		Type:    orgReadyCondition,
		Status:  condStatus,
		Reason:  reason,
		Message: msg,
	})

	if orgStatusEqual(before, &org.Status) {
		return nil
	}
	if err := r.Status().Update(ctx, org); err != nil {
		return fmt.Errorf("update org status: %w", err)
	}
	return nil
}

// setOrgCondition upserts a condition by Type, preserving LastTransitionTime
// when Status is unchanged (same semantics as meta.SetStatusCondition), so a
// re-asserted condition does not stamp a fresh timestamp and retrigger the
// watch.
func setOrgCondition(status *v1.OrgStatus, cond metav1.Condition) {
	if cond.ObservedGeneration == 0 {
		cond.ObservedGeneration = status.ObservedGeneration
	}
	for i := range status.Conditions {
		if status.Conditions[i].Type != cond.Type {
			continue
		}
		if status.Conditions[i].Status == cond.Status {
			cond.LastTransitionTime = status.Conditions[i].LastTransitionTime
		} else {
			cond.LastTransitionTime = metav1.Now()
		}
		status.Conditions[i] = cond
		return
	}
	cond.LastTransitionTime = metav1.Now()
	status.Conditions = append(status.Conditions, cond)
}

// orgStatusEqual compares two OrgStatus values ignoring condition
// LastTransitionTime (which setOrgCondition already holds stable on no real
// change), so an unchanged reconcile is a no-op write.
func orgStatusEqual(a, b *v1.OrgStatus) bool {
	if a.Namespace != b.Namespace || a.Phase != b.Phase || a.ObservedGeneration != b.ObservedGeneration {
		return false
	}
	if len(a.Conditions) != len(b.Conditions) {
		return false
	}
	for i := range a.Conditions {
		ac, bc := a.Conditions[i], b.Conditions[i]
		if ac.Type != bc.Type || ac.Status != bc.Status || ac.Reason != bc.Reason || ac.Message != bc.Message || ac.ObservedGeneration != bc.ObservedGeneration {
			return false
		}
	}
	return true
}

// SetupWithManager wires the OrgReconciler: it watches Orgs and the stack
// objects it owns, so a manual delete or drift of any stack object enqueues the
// owning Org and self-heals.
func (r *OrgReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.Org{}).
		Owns(&corev1.Namespace{}).
		Owns(&corev1.ResourceQuota{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&rbacv1.RoleBinding{}).
		Complete(r)
}
