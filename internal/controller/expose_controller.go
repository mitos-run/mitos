package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "mitos.run/mitos/api/v1"
)

// ExposeRouteReconciler watches Sandbox objects and pushes the full Ready
// route set to the Expose proxy admin endpoint via ExposePoster on every
// reconcile. It is registered with For(&v1.Sandbox{}) so any Sandbox event
// triggers a full rebuild of the route set.
type ExposeRouteReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Poster *ExposePoster
}

// Reconcile rebuilds the full route set from all Sandboxes in the cluster and
// POSTs it to the proxy. A sandbox that is Ready, has Spec.Expose set,
// Status.Endpoint, and a resolvable token Secret is included. A missing token
// Secret for an otherwise-qualifying sandbox causes a requeue (RequeueAfter
// 1s) rather than an error, so the Secret being created slightly after the
// sandbox status flip does not permanently drop the route. A Poster.Sync
// error returns (ctrl.Result{}, err) to trigger backoff-based requeue.
//
// Token values are never logged; only counts, labels, and sandbox IDs appear
// in log output.
func (r *ExposeRouteReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var sandboxList v1.SandboxList
	if err := r.List(ctx, &sandboxList); err != nil {
		return ctrl.Result{}, fmt.Errorf("expose reconcile: list sandboxes: %w", err)
	}

	// Resolve tokens for qualifying sandboxes. Track whether any Secret was
	// missing so we can request a requeue once all routes have been posted.
	missingSecret := false
	tokens := make(map[string]string, len(sandboxList.Items))

	for i := range sandboxList.Items {
		sb := &sandboxList.Items[i]
		if sb.Status.Phase != v1.SandboxReady || sb.Spec.Expose == nil || sb.Status.Endpoint == "" {
			continue
		}
		secretName := sb.Name + tokenSecretSuffix
		var secret corev1.Secret
		if err := r.Get(ctx, client.ObjectKey{Namespace: sb.Namespace, Name: secretName}, &secret); err != nil {
			if apierrors.IsNotFound(err) {
				// Token Secret not yet created: skip this sandbox this pass and
				// requeue so we retry once the Secret appears. Never log the
				// secret name at a level that includes values; label and
				// sandbox ID are safe.
				logger.Info("token secret not found; will requeue",
					"label", sb.Spec.Expose.Label,
					"sandboxID", sb.Status.SandboxID,
				)
				missingSecret = true
				continue
			}
			return ctrl.Result{}, fmt.Errorf("expose reconcile: get token secret %s/%s: %w", sb.Namespace, secretName, err)
		}
		// Key by namespace-qualified name: the tenant model is namespace-per-org
		// (mitos-org-<id>), so two sandboxes of the same name in different
		// namespaces must not collide and inherit each other's bearer token.
		tokens[client.ObjectKeyFromObject(sb).String()] = string(secret.Data["token"])
	}

	tokenFor := func(sb v1.Sandbox) (string, bool) {
		tok, ok := tokens[client.ObjectKeyFromObject(&sb).String()]
		return tok, ok
	}

	routes := BuildExposeRoutes(sandboxList.Items, tokenFor)

	logger.Info("syncing expose routes",
		"total", len(sandboxList.Items),
		"routes", len(routes),
		"missingSecret", missingSecret,
	)

	if err := r.Poster.Sync(ctx, routes); err != nil {
		return ctrl.Result{}, fmt.Errorf("expose reconcile: sync routes: %w", err)
	}

	if missingSecret {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the ExposeRouteReconciler with the manager.
// It watches Sandbox objects (any Sandbox event triggers a full route-set
// rebuild). The .Named call gives the reconciler an isolated name so it does
// not collide with other Sandbox reconcilers in the same manager.
func (r *ExposeRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("exposeroute").
		For(&v1.Sandbox{}).
		Complete(r)
}
