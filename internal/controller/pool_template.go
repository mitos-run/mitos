package controller

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "mitos.run/mitos/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// resolvePoolTemplate returns the effective inline template for a pool, resolving
// spec.templateRef to a shared template-shaped SandboxPool when spec.template is
// nil (ADR 0007). The common path (inline template) does no extra API read.
func (r *SandboxReconciler) resolvePoolTemplate(ctx context.Context, pool *v1.SandboxPool) (*v1.PoolTemplateSpec, error) {
	if pool.Spec.Template != nil {
		return pool.Spec.Template, nil
	}
	if pool.Spec.TemplateRef != nil {
		var ref v1.SandboxPool
		if err := r.Get(ctx, client.ObjectKey{Namespace: pool.Namespace, Name: pool.Spec.TemplateRef.Name}, &ref); err != nil {
			return nil, fmt.Errorf("resolve templateRef %s for pool %s: %w", pool.Spec.TemplateRef.Name, pool.Name, err)
		}
		return poolTemplate(&ref), nil
	}
	return poolTemplate(pool), nil
}

// poolTemplate resolves the effective template for a v1 SandboxPool. The inline
// spec.template is the common path; when it is nil the pool referenced a shared
// template-shaped object via spec.templateRef, which is resolved by name. This
// returns the inline template directly; callers that must resolve a templateRef
// do so against the cached client. A pool with neither set returns an empty
// template (validation forbids that shape, but the accessor stays nil-safe).
func poolTemplate(pool *v1.SandboxPool) *v1.PoolTemplateSpec {
	if pool.Spec.Template != nil {
		return pool.Spec.Template
	}
	return &v1.PoolTemplateSpec{}
}

// poolTemplateID is the stable identifier the pool's template snapshot is keyed
// by on the nodes. For an inline template the pool name is the key (one inline
// template per pool); for a templateRef the referenced name is the key, so
// several pools sharing one template definition share its snapshot.
func poolTemplateID(pool *v1.SandboxPool) string {
	if pool.Spec.TemplateRef != nil && pool.Spec.TemplateRef.Name != "" {
		return pool.Spec.TemplateRef.Name
	}
	return pool.Name
}

// poolReplicas is the desired per-node snapshot fan-out for a pool: the v1
// snapshots.replicasPerNode, falling back to warm.min for the fixed back-compat
// shape (the v1alpha1 spec.replicas mapped onto warm.min on conversion). Zero
// when neither is set.
func poolReplicas(pool *v1.SandboxPool) int32 {
	if pool.Spec.Snapshots != nil && pool.Spec.Snapshots.ReplicasPerNode > 0 {
		return pool.Spec.Snapshots.ReplicasPerNode
	}
	if pool.Spec.Warm != nil {
		return pool.Spec.Warm.Min
	}
	return 0
}

// poolWarmMin is the warm-pool floor (the fixed dormant husk pod count when no
// autoscaler ceiling is set). It carries the v1alpha1 spec.replicas / autoscale
// minWarm onto warm.min.
func poolWarmMin(pool *v1.SandboxPool) int32 {
	if pool.Spec.Warm != nil {
		return pool.Spec.Warm.Min
	}
	return 0
}

// sandboxOutputs returns the terminate-time output specs for a v1 Sandbox,
// re-homed under spec.lifetime.onTerminate.outputs. Nil-safe: a Sandbox with no
// lifetime or no onTerminate block has no outputs.
func sandboxOutputs(sb *v1.Sandbox) []v1.OutputSpec {
	if sb.Spec.Lifetime == nil || sb.Spec.Lifetime.OnTerminate == nil {
		return nil
	}
	return sb.Spec.Lifetime.OnTerminate.Outputs
}

// sandboxCheckpointOnTerminate reports whether a terminate should pair a memory
// snapshot with the new revision. It generalizes the v1alpha1
// SandboxClaim.checkpointOnTerminate boolean: a non-empty
// lifetime.onTerminate.snapshot retention directive requests the snapshot.
func sandboxCheckpointOnTerminate(sb *v1.Sandbox) bool {
	if sb.Spec.Lifetime == nil || sb.Spec.Lifetime.OnTerminate == nil {
		return false
	}
	return sb.Spec.Lifetime.OnTerminate.Snapshot != ""
}

// sandboxTTL reads the re-homed wall-clock lifetime (lifetime.ttl) nil-safely.
func sandboxTTL(sb *v1.Sandbox) *metav1.Duration {
	if sb.Spec.Lifetime == nil {
		return nil
	}
	return sb.Spec.Lifetime.TTL
}

// sandboxIdleTimeout reads the re-homed idle limit (lifetime.idleTimeout)
// nil-safely.
func sandboxIdleTimeout(sb *v1.Sandbox) *metav1.Duration {
	if sb.Spec.Lifetime == nil {
		return nil
	}
	return sb.Spec.Lifetime.IdleTimeout
}

// sandboxTTLSecondsAfterFinished reads the re-homed finished-sandbox etcd TTL
// (lifetime.ttlSecondsAfterFinished) nil-safely.
func sandboxTTLSecondsAfterFinished(sb *v1.Sandbox) *int32 {
	if sb.Spec.Lifetime == nil {
		return nil
	}
	return sb.Spec.Lifetime.TTLSecondsAfterFinished
}
