package controller

import (
	"context"
	"fmt"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/runmanifest"
)

// RegistryChecker resolves the latest concrete image reference for a watch and
// channel (for example an image pinned to its current digest, or the newest tag
// in a semver range). It is injected so the auto-update reconciler is testable
// without a registry or network.
type RegistryChecker interface {
	Latest(ctx context.Context, watch, channel string) (ref string, err error)
}

// AutoUpdateReconciler implements the Run with Mitos auto-update spine (#340/#440).
// It watches golden pools that declare a source.track policy (carried as
// annotations by runmanifest.GoldenPool) and re-snapshots them when upstream
// publishes a new release, by pointing the golden template at the freshly resolved
// reference. The existing pool reconcile then rebuilds the template snapshot.
//
// Rebasing running instances onto the new golden (re-fork plus Workspace reattach)
// is gated by the on_new_release policy; this reconciler performs the re-snapshot
// and records the policy decision. The instance-rebase executor is a follow-up
// (it forks the new golden and migrates each instance's Workspace), called out in
// the PR as needing envtest plus a live cluster and a security review.
//
// SECURITY: this controller mutates golden pool images. Per CLAUDE.md it requires
// a named human reviewer. It never logs or carries secret values; pools hold none.
type AutoUpdateReconciler struct {
	client.Client
	Registry RegistryChecker
	// Interval is how often a tracked pool is re-checked against upstream. Zero
	// disables periodic requeue (event-driven only).
	Interval time.Duration
}

// SetupWithManager registers the reconciler against SandboxPool. It is intentionally
// NOT called from cmd/controller yet: enabling it needs the RegistryChecker
// implementation, the instance-rebase executor, and a security review.
func (r *AutoUpdateReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.SandboxPool{}).
		Named("run-with-mitos-autoupdate").
		Complete(r)
}

// requeue returns the periodic re-check result.
func (r *AutoUpdateReconciler) requeue() ctrl.Result {
	if r.Interval <= 0 {
		return ctrl.Result{}
	}
	return ctrl.Result{RequeueAfter: r.Interval}
}

// Reconcile checks a tracked golden pool against upstream and re-snapshots on a new
// release. Untracked pools are ignored.
func (r *AutoUpdateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pool v1.SandboxPool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	watch := pool.Annotations[runmanifest.AnnTrackWatch]
	if watch == "" {
		return ctrl.Result{}, nil // not a tracked Run with Mitos pool
	}
	channel := pool.Annotations[runmanifest.AnnTrackChannel]

	latest, err := r.Registry.Latest(ctx, watch, channel)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("auto-update %s/%s: resolve latest: %w", pool.Namespace, pool.Name, err)
	}
	current := pool.Annotations[runmanifest.AnnResolvedImage]
	if latest == "" || latest == current {
		return r.requeue(), nil // up to date
	}

	// New release: re-snapshot by pointing the golden at the new reference. The
	// pool reconcile rebuilds the template snapshot from this image.
	if pool.Spec.Template != nil {
		pool.Spec.Template.Image = latest
	}
	if pool.Annotations == nil {
		pool.Annotations = map[string]string{}
	}
	pool.Annotations[runmanifest.AnnResolvedImage] = latest
	if err := r.Update(ctx, &pool); err != nil {
		return ctrl.Result{}, fmt.Errorf("auto-update %s/%s: re-snapshot: %w", pool.Namespace, pool.Name, err)
	}
	return r.requeue(), nil
}

// RebasePlan is the decoded effect of an on_new_release policy on running
// instances of a freshly re-snapshotted golden.
type RebasePlan struct {
	// AutoRebase re-forks every running instance onto the new golden now.
	AutoRebase bool
	// Offer surfaces an "update available" affordance and waits for the user.
	Offer bool
}

// rebasePlan decodes the on_new_release annotation. The conservative default
// (offer, not auto) applies when the policy is absent or unrecognized.
func rebasePlan(action string) RebasePlan {
	switch action {
	case string(runmanifest.ResnapshotAutoRebase):
		return RebasePlan{AutoRebase: true}
	case string(runmanifest.ResnapshotOnly):
		return RebasePlan{}
	default: // ResnapshotOfferRebase and anything unrecognized
		return RebasePlan{Offer: true}
	}
}
