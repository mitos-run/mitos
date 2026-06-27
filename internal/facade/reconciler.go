// Package facade implements the agents.x-k8s.io conformance facade (issue #19).
//
// It presents sandboxes via the upstream SIG agent-sandbox API
// (agents.x-k8s.io/v1beta1 Sandbox) and fulfils them on our fork engine by
// mapping each upstream Sandbox onto our husk-backed run path: a mitos.run/v1
// Sandbox with source.poolRef, bound to one of our pools. After the API v1
// consolidation (ADR 0007) the run-path object is the consolidated v1 Sandbox
// (source.poolRef), which folded the former SandboxClaim and
// SandboxFork into one kind.
//
// Toolchain note (ADR 0001): we vendor the upstream Go types
// (sigs.k8s.io/agent-sandbox) directly. That module declares go 1.26, so the
// faithful path required bumping our toolchain from go 1.24 to go 1.26. Both
// golangci-lint runs (darwin + GOOS=linux) analyzed the go 1.26 module cleanly
// with golangci-lint v1.64.8, so we kept the vendor instead of re-declaring the
// CRD by hand. See docs/adr/0001-facade-and-naming.md.
//
// The facade is opt-in: it runs as a separate binary (cmd/facade) with its own
// manager and is not entangled with cmd/controller. Extras (pools, warm pools,
// templates) stay in our mitos.run group; the single bridge annotation
// mitos.run/pool links an upstream Sandbox to one of our pools.
package facade

import (
	"context"
	"fmt"
	"net"

	agentsv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	runv1 "mitos.run/mitos/api/v1"
)

const (
	// PoolAnnotation is the single bridge annotation that links an upstream
	// agents.x-k8s.io Sandbox to one of our mitos.run pools (the warm-pool
	// source for the husk run path). When unset the facade falls back to its
	// configured default pool. Documented in docs/adr/0001-facade-and-naming.md.
	PoolAnnotation = "mitos.run/pool"

	// SandboxConditionType is the upstream Ready condition the facade mirrors
	// our SandboxClaim readiness into.
	SandboxConditionType = string(agentsv1beta1.SandboxConditionReady)
)

// SandboxReconciler reconciles an upstream agents.x-k8s.io/v1beta1 Sandbox
// onto our husk-backed run path. It owns exactly one of our consolidated
// mitos.run/v1 Sandbox objects per upstream Sandbox (same name + namespace,
// owner-referenced for GC), mirrors the run-path object's readiness into the
// upstream Sandbox status, and terminates the run-path object when the upstream
// Sandbox is deleted or set to operatingMode Suspended. The run-path object is a
// v1 Sandbox with source.poolRef (the consolidated successor to the old
// SandboxClaim, ADR 0007).
type SandboxReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// DefaultPool is the mitos.run pool a Sandbox binds to when it carries no
	// mitos.run/pool bridge annotation. Required: the facade cannot fulfil a
	// Sandbox without a pool to draw a husk from.
	DefaultPool string

	// ClusterDomain is the DNS domain used to derive the upstream
	// status.serviceFQDN (defaults to cluster.local upstream). Empty disables
	// the derived FQDN.
	ClusterDomain string
}

// isSuspended reports whether the upstream Sandbox is in the Suspended
// operating mode. Empty operatingMode defaults to Running (the upstream
// +kubebuilder:default=Running semantics).
func isSuspended(sb *agentsv1beta1.Sandbox) bool {
	return sb.Spec.OperatingMode == agentsv1beta1.SandboxOperatingModeSuspended
}

// poolFor resolves the mitos.run pool a Sandbox binds to: the bridge
// annotation mitos.run/pool if present, else the configured default pool.
func (r *SandboxReconciler) poolFor(sb *agentsv1beta1.Sandbox) string {
	if p := sb.Annotations[PoolAnnotation]; p != "" {
		return p
	}
	return r.DefaultPool
}

// Reconcile drives the Sandbox -> husk run-path lifecycle. The upstream
// pause/resume contract is the spec.operatingMode Running/Suspended toggle
// (v1beta1 graduated the v1alpha1 spec.replicas 0/1 into a named enum). We map
// it onto the husk warm pool:
//
//   - Running (default empty) and not deleting (create OR resume): ensure our
//     run-path Sandbox. On resume this re-activates a dormant warm husk pod via
//     the same fast path as the initial create. Mirror the claim readiness, and
//     on Ready set the serving observables (serviceFQDN, podIPs).
//   - Suspended (pause): RELEASE the run path to the warm pool by deleting our
//     run-path Sandbox, so the bound husk pod returns dormant to the pool. Clear
//     the serving observables (Ready False, Suspended True, serviceFQDN + podIPs
//     cleared, no serving endpoint).
//   - deletion: the owner reference garbage-collects our run-path Sandbox; we
//     just observe and return.
//
// The mapping is idempotent and stable under Running->Suspended->Running->Suspended
// toggling: pause is a no-op when the claim is already released, resume
// re-creates the same named claim, and the status writes are conditional (no
// write when nothing changed).
func (r *SandboxReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var sb agentsv1beta1.Sandbox
	if err := r.Get(ctx, req.NamespacedName, &sb); err != nil {
		// Not found: the Sandbox is gone; owner-ref GC removes our claim.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion: the run-path Sandbox carries an owner reference to the upstream
	// Sandbox, so the apiserver garbage-collects it. Nothing for the facade to do.
	if !sb.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	pool := r.poolFor(&sb)
	if pool == "" {
		// No pool to bind to and no default configured: surface a not-ready
		// condition with actionable remediation rather than creating an
		// unbindable claim.
		return ctrl.Result{}, r.mirror(ctx, &sb, statusUpdate{
			status:  metav1.ConditionFalse,
			reason:  "NoPool",
			message: fmt.Sprintf("no %s annotation and no --default-pool configured; set the bridge annotation or the facade default pool", PoolAnnotation),
		})
	}

	// Suspended (pause): release the run path to the warm pool. Delete our
	// run-path Sandbox so the bound husk pod returns dormant to the pool, and
	// set the Suspended condition + clear the serving observables (serviceFQDN
	// + podIPs).
	if isSuspended(&sb) {
		if err := r.deleteClaim(ctx, &sb); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.mirror(ctx, &sb, statusUpdate{
			status:    metav1.ConditionFalse,
			reason:    "Suspended",
			message:   "operatingMode is Suspended; the husk run-path object is released to the warm pool",
			suspended: true,
		})
	}

	// Running (create or resume): ensure our run-path Sandbox exists. On a
	// resume after a Suspended this re-activates a dormant warm husk pod via
	// the same fast path as create. Mirror the claim readiness.
	claim, err := r.ensureClaim(ctx, &sb, pool)
	if err != nil {
		return ctrl.Result{}, err
	}

	if claim.Status.Phase == runv1.SandboxReady {
		logger.V(1).Info("sandbox ready", "sandbox", req.NamespacedName, "runPath", claim.Name, "endpoint", claim.Status.Endpoint)
		return ctrl.Result{}, r.mirror(ctx, &sb, statusUpdate{
			status:   metav1.ConditionTrue,
			reason:   "ClaimReady",
			message:  fmt.Sprintf("husk run-path object %q is Ready", claim.Name),
			endpoint: claim.Status.Endpoint,
		})
	}

	return ctrl.Result{}, r.mirror(ctx, &sb, statusUpdate{
		status:  metav1.ConditionFalse,
		reason:  "Claim" + string(claim.Status.Phase),
		message: fmt.Sprintf("husk run-path object %q is in phase %q", claim.Name, claim.Status.Phase),
	})
}

// ensureClaim creates or returns our run-path Sandbox for an upstream Sandbox.
// The run-path object is a consolidated v1 Sandbox with source.poolRef (the old
// SandboxClaim, ADR 0007): named after the upstream Sandbox, in the same
// namespace, owner-referenced to it (for GC + the watch back-link), and bound to
// the resolved pool via source.poolRef.
//
// podTemplate mapping: the Sandbox spec.podTemplate.spec.containers[*].env is
// reconciled onto the run-path object's env (the husk run path applies env into
// the guest). Other podTemplate fields (images, resources, volumes, security
// context) are a documented conformance exception for a later slice; see
// docs/facade-conformance.md. The husk pool already pins the image + resources
// at pool build time, so the per-Sandbox podTemplate image is not yet honored.
func (r *SandboxReconciler) ensureClaim(ctx context.Context, sb *agentsv1beta1.Sandbox, pool string) (*runv1.Sandbox, error) {
	claim := &runv1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sb.Name,
			Namespace: sb.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, claim, func() error {
		if claim.Annotations == nil {
			claim.Annotations = map[string]string{}
		}
		claim.Annotations[PoolAnnotation] = pool
		claim.Spec.Source = runv1.SandboxSource{PoolRef: &runv1.LocalObjectReference{Name: pool}}
		claim.Spec.Env = podTemplateEnv(sb)
		// Owner reference: GC our run-path object when the upstream Sandbox is
		// deleted, and set the controller back-link so a status change re-queues
		// the upstream Sandbox.
		return controllerutil.SetControllerReference(sb, claim, r.Scheme)
	})
	if err != nil {
		return nil, fmt.Errorf("ensure run-path Sandbox for sandbox %s/%s: %w", sb.Namespace, sb.Name, err)
	}
	return claim, nil
}

// podTemplateEnv extracts the union of container env vars from the upstream
// Sandbox podTemplate. The husk run path applies these into the guest. We take
// the first container's env as the canonical set (sandboxes are single-workload
// by construction); additional containers are a later-slice exception.
func podTemplateEnv(sb *agentsv1beta1.Sandbox) []corev1.EnvVar {
	containers := sb.Spec.PodTemplate.Spec.Containers
	if len(containers) == 0 {
		return nil
	}
	return containers[0].Env
}

// deleteClaim terminates our run-path Sandbox for an upstream Sandbox
// (Suspended path). It is a no-op when the run-path object is already gone.
func (r *SandboxReconciler) deleteClaim(ctx context.Context, sb *agentsv1beta1.Sandbox) error {
	claim := &runv1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: sb.Name, Namespace: sb.Namespace},
	}
	if err := r.Delete(ctx, claim); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("terminate run-path Sandbox for sandbox %s/%s: %w", sb.Namespace, sb.Name, err)
	}
	return nil
}

// statusUpdate is the set of upstream Sandbox status facts the facade mirrors in
// one reconcile: the Ready condition, the Suspended condition, and (when running
// with a serving endpoint) the serviceFQDN + podIPs. The serving observables are
// populated only when status is True with an endpoint; on every other path
// (Suspended, pending, error) they are CLEARED so a suspended or not-ready
// Sandbox never advertises a stale endpoint.
type statusUpdate struct {
	status   metav1.ConditionStatus
	reason   string
	message  string
	suspended bool
	// endpoint is the husk run-path endpoint (host:port) when Ready, used to
	// derive podIPs. Empty on every not-serving path.
	endpoint string
}

// mirror writes one statusUpdate onto the upstream Sandbox status subresource.
// It is idempotent (no write when nothing changed) and is the single place the
// serving observables (serviceFQDN, podIPs) are set or cleared, so Suspended
// always clears them and resume always re-populates them.
func (r *SandboxReconciler) mirror(ctx context.Context, sb *agentsv1beta1.Sandbox, u statusUpdate) error {
	cond := metav1.Condition{
		Type:               SandboxConditionType,
		Status:             u.status,
		Reason:             u.reason,
		Message:            u.message,
		ObservedGeneration: sb.Generation,
	}

	before := sb.DeepCopy()
	changed := apimeta.SetStatusCondition(&sb.Status.Conditions, cond)

	// Mirror the Suspended condition (v1beta1 replaces the v1alpha1 Replicas
	// field for tracking the suspended/running state).
	suspStatus := metav1.ConditionFalse
	suspReason := "Running"
	suspMessage := "sandbox is in running operating mode"
	if u.suspended {
		suspStatus = metav1.ConditionTrue
		suspReason = "Suspended"
		suspMessage = "operatingMode is Suspended; the husk run-path object is released to the warm pool"
	}
	suspCond := metav1.Condition{
		Type:               string(agentsv1beta1.SandboxConditionSuspended),
		Status:             suspStatus,
		Reason:             suspReason,
		Message:            suspMessage,
		ObservedGeneration: sb.Generation,
	}
	if apimeta.SetStatusCondition(&sb.Status.Conditions, suspCond) {
		changed = true
	}

	// Serving observables: set on Ready-with-endpoint, cleared otherwise.
	if u.status == metav1.ConditionTrue && u.endpoint != "" {
		sb.Status.ServiceFQDN = r.serviceFQDN(sb)
		sb.Status.PodIPs = podIPsFromEndpoint(u.endpoint)
	} else {
		sb.Status.ServiceFQDN = ""
		sb.Status.PodIPs = nil
	}

	if !changed &&
		before.Status.ServiceFQDN == sb.Status.ServiceFQDN &&
		equalStrings(before.Status.PodIPs, sb.Status.PodIPs) {
		return nil
	}
	if err := r.Status().Update(ctx, sb); err != nil {
		return fmt.Errorf("mirror status into sandbox %s/%s: %w", sb.Namespace, sb.Name, err)
	}
	return nil
}

// podIPsFromEndpoint derives the upstream status.podIPs from the husk run-path
// endpoint (host:port). The host portion is the serving pod IP; a bare host
// without a port is taken as-is. Returns nil when no IP can be parsed.
func podIPsFromEndpoint(endpoint string) []string {
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		host = endpoint
	}
	if host == "" {
		return nil
	}
	return []string{host}
}

// equalStrings reports whether two string slices are element-wise equal.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// serviceFQDN derives the upstream status.serviceFQDN for a Sandbox using the
// configured cluster domain, matching the upstream headless-Service naming
// (<name>.<namespace>.svc.<cluster-domain>). Empty when no cluster domain is
// configured.
func (r *SandboxReconciler) serviceFQDN(sb *agentsv1beta1.Sandbox) string {
	if r.ClusterDomain == "" {
		return ""
	}
	return fmt.Sprintf("%s.%s.svc.%s", sb.Name, sb.Namespace, r.ClusterDomain)
}

// SetupWithManager wires the reconciler to watch upstream Sandboxes and own our
// run-path Sandbox objects so a status change re-queues the owning upstream
// Sandbox.
func (r *SandboxReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1beta1.Sandbox{}).
		Owns(&runv1.Sandbox{}).
		Complete(r)
}
