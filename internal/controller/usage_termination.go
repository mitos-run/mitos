package controller

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/tenant"
	"mitos.run/mitos/internal/usage"
)

// This file is the controller half of the issue #682 (was #664) terminate-time
// usage fix: the usage collector scrapes claimed husk pods once a minute, so
// presence between the LAST scrape and terminate was never recorded. The
// reconciler knows the exact release instant, so the terminate paths record a
// usage.Termination per claimed husk pod; the collector's HuskSource turns it
// into a final sample on its next cycle.
//
// Both hooks are BEST-EFFORT and nil-safe: usage recording never blocks, fails,
// or delays a terminate (boring failure behavior; losing an event only
// under-bills a tail, never double-bills, and never wedges deletion). Every
// recorded field is control-plane data: the pod name, the controller's OWN
// stamped labels, and the claim status; nothing the pod reported. No secrets.

// recordHuskTerminations records one termination event per claimed, ORG-LABELED
// husk pod in pods, stamped at the given release instant. An unattributed pod
// (no trusted mitos.run/org label: the self-host path) was never billable and
// records nothing. Callers that already listed the claim's pods (the delete
// path) pass them directly.
//
// ONE EVENT PER CLAIM: a claim whose phase is already Terminated recorded its
// event at the lifetime terminate (terminateLifetime calls the hook BEFORE
// stamping the phase), which is the TRUE instant the VM was reaped, so the
// later object delete records nothing here. A second event would be worse
// than none: it either no-op-consumes the collector's finalized guard (so the
// event actually carrying the tail is guard-dropped) or, past the source's
// retention horizon, synthesizes a phantom pair. The collector's finalized
// guard stays as the second line of defense for a requeued terminate path
// that records twice before the phase persists.
func (r *SandboxReconciler) recordHuskTerminations(claim *v1.Sandbox, pods []corev1.Pod, at time.Time) {
	if r.UsageTerminations == nil {
		return
	}
	if claim.Status.Phase == v1.SandboxTerminated {
		return
	}
	var started time.Time
	if claim.Status.StartedAt != nil {
		started = claim.Status.StartedAt.Time
	}
	for i := range pods {
		pod := &pods[i]
		org := pod.Labels[tenant.OrgLabelKey]
		if org == "" {
			continue
		}
		r.UsageTerminations.Record(usage.Termination{
			VMID:      pod.Name,
			APIID:     pod.Labels[huskClaimLabel],
			OrgID:     org,
			StartedAt: started,
			At:        at,
		})
	}
}

// recordClaimHuskTerminations lists the claim's claimed husk pods (by the
// mitos.run/claim label the controller stamped) and records a termination for
// each org-labeled one, stamped now. It is the hook for terminate paths that
// have not already listed the pods (lifetime/idle expiry). A list failure is
// logged at V(1) and skipped: best-effort, the terminate must proceed and the
// cost is one under-billed tail.
func (r *SandboxReconciler) recordClaimHuskTerminations(ctx context.Context, claim *v1.Sandbox) {
	if r.UsageTerminations == nil {
		return
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(claim.Namespace), client.MatchingLabels{huskClaimLabel: claim.Name}); err != nil {
		log.FromContext(ctx).V(1).Info("list husk pods for usage termination; tail window not recorded", "claim", claim.Name)
		return
	}
	r.recordHuskTerminations(claim, pods.Items, time.Now())
}
