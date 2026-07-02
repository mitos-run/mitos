package controller

// Self-healing template rebuild (issue #584).
//
// A husk pod's dormant VMM restores the pool's template snapshot at start. A
// snapshot can be structurally present (the build "succeeded", a digest was
// recorded) yet fail to RESTORE, for example a corrupt memory file or a kernel
// mismatch introduced by a bad init step. Every dormant pod bound to that
// snapshot then CrashLoopBackOffs forever: the warm pool never converges and no
// existing reconcile path rebuilds the snapshot, because from the controller's
// point of view the build already succeeded and the holder count is satisfied.
//
// This file detects that condition from pod status (templateRestoreFailing) and
// drives a backoff-bounded forced rebuild (driveTemplateHealth), reusing the
// force-rebuild plumbing forkd's reuse-or-rebuild gate exposes (Task 1,
// bf9590df): a forced CreateTemplate always rebuilds even when the on-disk
// snapshot looks reusable, so the controller's presumption that the snapshot is
// broken cannot be silently ignored by the node.

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "mitos.run/mitos/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// crashloopThreshold is how many restarts a dormant husk bound to the
	// CURRENT template digest must accumulate before the template is presumed
	// restore-broken (kubelet backoff means 3 restarts is already minutes of
	// failure, and a healthy activation never restarts).
	crashloopThreshold = 3

	// minFailingHusks is how many DISTINCT crashlooping dormant husks on the
	// current digest must be observed before the template is presumed broken.
	// One flaky pod (a transient node hiccup, an evicted node) must not trigger
	// a rebuild; two independent pods hitting the same crashloop on the same
	// digest is evidence of a real template defect rather than pod-local noise.
	minFailingHusks = 2
)

// rebuildBackoff returns how long after LastRebuildTime the next automatic
// rebuild may run: 1m << (attempts-1), capped at 30m. attempts 0 or 1 both
// return the 1m base (attempts is pre-increment: it is 0 before the first
// rebuild and 1 immediately after it, so both must wait the same base interval
// before a second attempt is allowed).
func rebuildBackoff(attempts int32) time.Duration {
	const base = time.Minute
	const capped = 30 * time.Minute
	if attempts <= 1 {
		return base
	}
	shift := uint(attempts - 1)
	// 1m<<5 = 32m already exceeds the cap; avoid the shift entirely once it
	// would, both to stay correct and to never risk an overflow on a runaway
	// attempts counter.
	if shift >= 5 {
		return capped
	}
	d := base << shift
	if d > capped {
		return capped
	}
	return d
}

// podCrashlooping reports whether p's container is in CrashLoopBackOff with at
// least crashloopThreshold restarts. A pod with no container status yet (still
// scheduling) or fewer restarts is not crashlooping.
func podCrashlooping(p *corev1.Pod) bool {
	for _, cs := range p.Status.ContainerStatuses {
		if cs.RestartCount < crashloopThreshold {
			continue
		}
		if cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" {
			return true
		}
	}
	return false
}

// templateRestoreFailing reports whether the pool's current template is
// presumed broken: at least minFailingHusks (2) dormant husk pods whose
// template-digest annotation equals digest are in CrashLoopBackOff with
// restartCount >= crashloopThreshold. An empty digest (no build has completed
// yet) never counts as failing: there is nothing to rebuild from a worse state.
// Callers pass ONLY the pool's current dormant husk pods (already filtered for
// claimed/stale-digest by reconcileHuskPods), so a pod bound to an old digest a
// prior rebuild already replaced, or a pod that was already reaped, never
// contributes to the count.
func templateRestoreFailing(pods []corev1.Pod, digest string) bool {
	if digest == "" {
		return false
	}
	var failing int
	for i := range pods {
		p := &pods[i]
		if p.Annotations[huskTemplateDigestAnnotation] != digest {
			continue
		}
		if podCrashlooping(p) {
			failing++
		}
	}
	return failing >= minFailingHusks
}

// driveTemplateHealth evaluates the pool's dormant husk pods against the pool's
// current template digest and drives the TemplateHealthy condition plus,
// backoff permitting, a backoff-bounded forced in-place rebuild (#584). It must
// run after the pool's Status.TemplateDigest and the dormant pod list for this
// reconcile are both known, and before the status write, so its condition and
// bookkeeping mutations land in the same write as the rest of the husk-path
// reconcile.
//
// Idempotent per backoff window: LastRebuildTime + rebuildBackoff(RebuildAttempts)
// is checked BEFORE any rebuild is triggered, and LastRebuildTime is stamped in
// the SAME call that triggers one, so a hot reconcile loop (for example the
// husk pod watch firing repeatedly while the crashloopers keep restarting) never
// double-triggers a rebuild inside one backoff window.
func (r *SandboxPoolReconciler) driveTemplateHealth(ctx context.Context, pool *v1.SandboxPool, template *v1.PoolTemplateSpec, templateID string, nodeFilter map[string]bool, dormantPods []corev1.Pod, warmReady bool, now metav1.Time) {
	logger := log.FromContext(ctx)
	digest := pool.Status.TemplateDigest
	failing := templateRestoreFailing(dormantPods, digest)

	if !failing {
		// Healthy again only when the warm pool is genuinely ready (warm husks
		// Ready, per the caller's warmReady) AND there are no crashloopers on the
		// current digest (failing is already false here). A pool still filling
		// its warm target is left alone: no Healthy claim, no attempts reset,
		// until it actually converges.
		if warmReady {
			setCondition(&pool.Status.Conditions, metav1.Condition{
				Type:               "TemplateHealthy",
				Status:             metav1.ConditionTrue,
				LastTransitionTime: now,
				Reason:             "Healthy",
				Message:            "warm husk pods are ready and no crashloopers on the current template digest",
			})
			if pool.Status.RebuildAttempts != 0 {
				pool.Status.RebuildAttempts = 0
			}
		}
		return
	}

	setCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               "TemplateHealthy",
		Status:             metav1.ConditionFalse,
		LastTransitionTime: now,
		Reason:             "RestoreFailing",
		Message:            fmt.Sprintf("%d or more dormant husk pods bound to template digest %s are crashlooping; the template is presumed restore-broken", minFailingHusks, digest),
	})

	var last time.Time
	if pool.Status.LastRebuildTime != nil {
		last = pool.Status.LastRebuildTime.Time
	}
	if !last.IsZero() && now.Time.Before(last.Add(rebuildBackoff(pool.Status.RebuildAttempts))) {
		// Still inside the backoff window for the last attempt: report the
		// condition only, do not trigger another rebuild this reconcile.
		return
	}

	wrappedDEK, kekID, err := r.templateEncKey(ctx, pool, template, templateID)
	if err != nil {
		logger.Error(err, "resolve encryption key to force-rebuild a restore-failing template")
		return
	}
	if err := r.rebuildStaleSnapshots(ctx, templateID, template, wrappedDEK, kekID, nodeFilter, true); err != nil {
		logger.Error(err, "force-rebuild restore-failing template")
	}

	// Delete only the crashlooping pods carrying the BAD (current, presumed
	// broken) digest annotation, so they recreate against the fresh snapshot.
	// A pod that is dormant but not (yet) crashlooping is left alone.
	for i := range dormantPods {
		p := &dormantPods[i]
		if p.Annotations[huskTemplateDigestAnnotation] != digest || !podCrashlooping(p) {
			continue
		}
		if err := r.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
			logger.Error(err, "delete crashlooping husk pod after forced rebuild", "pod", p.Name)
		}
	}

	pool.Status.RebuildAttempts++
	pool.Status.LastRebuildTime = &now
	setCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               "TemplateHealthy",
		Status:             metav1.ConditionFalse,
		LastTransitionTime: now,
		Reason:             "Rebuilding",
		Message:            fmt.Sprintf("forced rebuild attempt %d triggered for restore-failing template digest %s", pool.Status.RebuildAttempts, digest),
	})
}
