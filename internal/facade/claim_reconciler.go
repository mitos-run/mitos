package facade

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	extv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"

	runv1 "mitos.run/mitos/api/v1"
)

const (
	// WarmPoolPolicyAnnotation records the upstream warmpool policy
	// (none/default/<name>) the bridged claim was created under. The value none is
	// a documented justified exception: our fork-from-snapshot engine has no
	// pool-less run path, so a none claim is still forked from the resolved
	// template's pool snapshot. See docs/facade-conformance.md.
	WarmPoolPolicyAnnotation = "mitos.run/warmpool-policy"

	// claimReadyConditionType is the upstream SandboxClaim condition the facade
	// mirrors our claim's readiness into. Upstream uses Ready/Bound style
	// conditions; we surface a Ready condition reflecting our claim phase.
	claimReadyConditionType = "Ready"

	// poolRetryInterval is the requeue backoff when no pool resolves yet for a
	// claim (e.g. the warm pool reconciler has not created our pool yet). Short
	// enough to bind promptly once the pool appears, long enough not to hot-loop.
	poolRetryInterval = 2 * time.Second
)

// SandboxClaimReconciler maps an upstream extensions.agents.x-k8s.io/v1alpha1
// SandboxClaim onto our consolidated mitos.run/v1 Sandbox with source.poolRef
// (the fork-from-snapshot run path, #18; ADR 0007 folded the old mitos.run
// SandboxClaim into the v1 Sandbox). It owns exactly one of our Sandbox objects
// per upstream claim (same name + namespace, owner-referenced for GC), resolving
// the pool to fork from per the upstream warmpool policy:
//
//   - none: the upstream contract is "always create fresh sandboxes, no warm
//     pool". Our engine has NO pool-less run path (every sandbox forks from a
//     pool's template snapshot), so a none claim is forked from the resolved
//     template's pool. This is a documented justified exception
//     (docs/facade-conformance.md), recorded via the WarmPoolPolicyAnnotation; it
//     is not a silent remap.
//   - default (the upstream default): bind from any of our pools whose
//     templateRef matches the resolved template (deterministic: lowest pool name).
//   - <name>: bind from that specific warm pool. The pool is our pool created by
//     the warm pool reconciler under the same name (bridge annotation
//     mitos.run/warmpool).
//
// It mirrors the upstream status (the bound sandbox name, podIPs, and a Ready
// condition derived from our claim phase) and handles deletion via the owner
// reference (their claim deleted => our claim GC'd).
type SandboxClaimReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// ClusterDomain is the DNS domain used to derive a stable identity for the
	// bound sandbox (matching the core Sandbox reconciler's serviceFQDN
	// derivation). Empty disables the derived identity annotation.
	ClusterDomain string
}

// +kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxclaims/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mitos.run,resources=sandboxes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mitos.run,resources=sandboxpools,verbs=get;list;watch

// Reconcile drives the upstream SandboxClaim -> our fork-from-snapshot claim
// lifecycle. Deletion is handled by the owner-reference garbage collector.
func (r *SandboxClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var src extv1alpha1.SandboxClaim
	if err := r.Get(ctx, req.NamespacedName, &src); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !src.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	policy := warmPoolPolicy(&src)
	pool, err := r.resolvePool(ctx, &src, policy)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pool == "" {
		// No pool resolved for the policy: surface a not-ready condition with
		// actionable remediation rather than creating an unbindable claim, and
		// requeue so a pool created moments later (e.g. by the warm pool
		// reconciler in the same manager) is picked up without an external nudge.
		if err := r.mirror(ctx, &src, claimStatusUpdate{
			status:  metav1.ConditionFalse,
			reason:  "NoPool",
			message: fmt.Sprintf("no mitos.run pool resolves for template %q under warmpool policy %q; create a SandboxWarmPool (or our SandboxPool) for the template", src.Spec.TemplateRef.Name, policy),
		}); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: poolRetryInterval}, nil
	}

	claim, err := r.ensureClaim(ctx, &src, pool, policy)
	if err != nil {
		return ctrl.Result{}, err
	}
	logger.V(1).Info("mirrored upstream SandboxClaim", "claim", req.NamespacedName, "pool", pool, "policy", policy)

	if claim.Status.Phase == runv1.SandboxReady {
		return ctrl.Result{}, r.mirror(ctx, &src, claimStatusUpdate{
			status:      metav1.ConditionTrue,
			reason:      "Bound",
			message:     fmt.Sprintf("forked sandbox is Ready on pool %q", pool),
			sandboxName: claim.Name,
			endpoint:    claim.Status.Endpoint,
		})
	}

	return ctrl.Result{}, r.mirror(ctx, &src, claimStatusUpdate{
		status:  metav1.ConditionFalse,
		reason:  "Claim" + string(claim.Status.Phase),
		message: fmt.Sprintf("fork-from-snapshot claim is in phase %q on pool %q", claim.Status.Phase, pool),
	})
}

// warmPoolPolicy returns the effective upstream warmpool policy, applying the
// upstream default (default) when unset.
func warmPoolPolicy(src *extv1alpha1.SandboxClaim) extv1alpha1.WarmPoolPolicy {
	if src.Spec.WarmPool == nil || *src.Spec.WarmPool == "" {
		return extv1alpha1.WarmPoolPolicyDefault
	}
	return *src.Spec.WarmPool
}

// resolvePool resolves the mitos.run pool a claim forks from per the upstream
// warmpool policy. Returns the empty string when no pool resolves (the caller
// surfaces a not-ready condition).
//
//   - named: the named warm pool maps to our pool of the same name (the bridge).
//   - default and none: any of our pools whose templateRef matches the resolved
//     template (deterministic: lowest pool name). none has no pool-less path in
//     our engine, so it resolves the same as default; the distinction is recorded
//     in the claim annotation and documented as a justified exception.
func (r *SandboxClaimReconciler) resolvePool(ctx context.Context, src *extv1alpha1.SandboxClaim, policy extv1alpha1.WarmPoolPolicy) (string, error) {
	if policy.IsSpecificPool() {
		// Named pool: our pool of that name (created by the warm pool reconciler).
		var pool runv1.SandboxPool
		err := r.Get(ctx, client.ObjectKey{Namespace: src.Namespace, Name: string(policy)}, &pool)
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("resolve named warm pool %q for claim %s/%s: %w", policy, src.Namespace, src.Name, err)
		}
		return pool.Name, nil
	}

	// default / none: any of our pools matching the resolved template. A pool
	// matches when its spec.templateRef points at the resolved template name (the
	// warm pool reconciler stamps it, ADR 0007); a pool with an inline template
	// only (no templateRef) is the template-pool itself and does not match.
	var pools runv1.SandboxPoolList
	if err := r.List(ctx, &pools, client.InNamespace(src.Namespace)); err != nil {
		return "", fmt.Errorf("list pools for claim %s/%s: %w", src.Namespace, src.Name, err)
	}
	var matches []string
	for i := range pools.Items {
		ref := pools.Items[i].Spec.TemplateRef
		if ref != nil && ref.Name == src.Spec.TemplateRef.Name {
			matches = append(matches, pools.Items[i].Name)
		}
	}
	if len(matches) == 0 {
		return "", nil
	}
	sort.Strings(matches)
	return matches[0], nil
}

// ensureClaim creates or updates our run-path Sandbox for an upstream claim. Our
// run-path object is a consolidated v1 Sandbox with source.poolRef (the old
// SandboxClaim, ADR 0007): named after the upstream claim, in the same
// namespace, owner-referenced to it, and bound to the resolved pool via
// source.poolRef. From the upstream lifecycle, ttlSecondsAfterFinished maps onto
// our sandbox's lifetime.ttlSecondsAfterFinished; shutdownTime is recorded via
// the mitos.run/shutdown-time annotation (not mapped to a Timeout).
// additionalPodMetadata annotations are propagated where our object supports
// them.
func (r *SandboxClaimReconciler) ensureClaim(ctx context.Context, src *extv1alpha1.SandboxClaim, pool string, policy extv1alpha1.WarmPoolPolicy) (*runv1.Sandbox, error) {
	claim := &runv1.Sandbox{
		ObjectMeta: metaName(src.Name, src.Namespace),
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, claim, func() error {
		if claim.Annotations == nil {
			claim.Annotations = map[string]string{}
		}
		claim.Annotations[PoolAnnotation] = pool
		claim.Annotations[TemplateAnnotation] = src.Spec.TemplateRef.Name
		claim.Annotations[WarmPoolPolicyAnnotation] = string(policy)
		// Propagate the upstream additionalPodMetadata annotations onto our object
		// (a documented best-effort: our Sandbox has no per-pod metadata field, so
		// the upstream metadata is recorded as annotations for traceability).
		for k, v := range src.Spec.AdditionalPodMetadata.Annotations {
			claim.Annotations[k] = v
		}

		claim.Spec.Source = runv1.SandboxSource{PoolRef: &runv1.LocalObjectReference{Name: pool}}
		claim.Spec.Env = claimEnv(src)
		applyLifecycle(claim, src.Spec.Lifecycle)
		return controllerutil.SetControllerReference(src, claim, r.Scheme)
	})
	if err != nil {
		return nil, fmt.Errorf("ensure run-path Sandbox for upstream claim %s/%s: %w", src.Namespace, src.Name, err)
	}
	return claim, nil
}

// claimEnv maps the upstream claim's env list (the extension EnvVar shape) onto
// our claim's corev1 env list. The upstream containerName targeting is a
// documented exception: our run path applies env globally into the guest.
func claimEnv(src *extv1alpha1.SandboxClaim) []corev1.EnvVar {
	if len(src.Spec.Env) == 0 {
		return nil
	}
	out := make([]corev1.EnvVar, 0, len(src.Spec.Env))
	for _, e := range src.Spec.Env {
		out = append(out, corev1.EnvVar{Name: e.Name, Value: e.Value})
	}
	return out
}

// applyLifecycle maps the upstream claim lifecycle (shutdownTime,
// ttlSecondsAfterFinished) onto our run-path Sandbox. ttlSecondsAfterFinished
// re-homes under spec.lifetime.ttlSecondsAfterFinished (ADR 0007). The upstream
// shutdownPolicy (Delete/DeleteForeground/Retain) governs the UPSTREAM claim
// object only and is enforced by the owner-reference cascade (deleting their
// claim GCs ours); it is a documented exception that our engine does not
// separately honor the Retain vs Delete distinction at the our-object level.
func applyLifecycle(claim *runv1.Sandbox, lc *extv1alpha1.Lifecycle) {
	if lc == nil {
		return
	}
	if lc.TTLSecondsAfterFinished != nil {
		ttl := *lc.TTLSecondsAfterFinished
		if claim.Spec.Lifetime == nil {
			claim.Spec.Lifetime = &runv1.SandboxLifetime{}
		}
		claim.Spec.Lifetime.TTLSecondsAfterFinished = &ttl
	}
	if lc.ShutdownTime != nil {
		// shutdownTime is an absolute expiry; our sandbox's lifetime.ttl is a
		// wall-clock budget from start. We record the absolute time via the
		// lifecycle annotation so it is not silently dropped. Keeping the mapping
		// simple and honest: stamp the requested expiry.
		if claim.Annotations == nil {
			claim.Annotations = map[string]string{}
		}
		claim.Annotations["mitos.run/shutdown-time"] = lc.ShutdownTime.UTC().Format("2006-01-02T15:04:05Z")
	}
}

// claimStatusUpdate is the set of upstream SandboxClaim status facts the facade
// mirrors in one reconcile: a Ready/Bound condition, the bound sandbox name, and
// (when Ready with an endpoint) the podIPs.
type claimStatusUpdate struct {
	status      metav1.ConditionStatus
	reason      string
	message     string
	sandboxName string
	endpoint    string
}

// mirror writes one claimStatusUpdate onto the upstream SandboxClaim status
// subresource, idempotently (no write when nothing changed). The bound sandbox
// name + podIPs are set only on a Ready-with-endpoint path and cleared
// otherwise, so a not-bound claim never advertises a stale sandbox.
func (r *SandboxClaimReconciler) mirror(ctx context.Context, src *extv1alpha1.SandboxClaim, u claimStatusUpdate) error {
	cond := metav1.Condition{
		Type:               claimReadyConditionType,
		Status:             u.status,
		Reason:             u.reason,
		Message:            u.message,
		ObservedGeneration: src.Generation,
	}

	before := src.DeepCopy()
	changed := apimeta.SetStatusCondition(&src.Status.Conditions, cond)

	if u.status == metav1.ConditionTrue && u.endpoint != "" {
		src.Status.SandboxStatus.Name = u.sandboxName
		src.Status.SandboxStatus.PodIPs = podIPsFromEndpoint(u.endpoint)
	} else {
		src.Status.SandboxStatus.Name = ""
		src.Status.SandboxStatus.PodIPs = nil
	}

	if !changed &&
		before.Status.SandboxStatus.Name == src.Status.SandboxStatus.Name &&
		equalStrings(before.Status.SandboxStatus.PodIPs, src.Status.SandboxStatus.PodIPs) {
		return nil
	}
	if err := r.Status().Update(ctx, src); err != nil {
		return fmt.Errorf("mirror status into claim %s/%s: %w", src.Namespace, src.Name, err)
	}
	return nil
}

// SetupWithManager wires the reconciler to watch upstream SandboxClaims and own
// our run-path Sandbox objects so a status change re-queues the upstream claim.
func (r *SandboxClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&extv1alpha1.SandboxClaim{}).
		Owns(&runv1.Sandbox{}).
		Complete(r)
}
