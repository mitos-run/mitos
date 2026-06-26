package runservice

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// K8sApplier is the production Applier: it upserts objects through a
// controller-runtime client. Create-or-update keeps a repeat run idempotent: the
// shared golden pool is reused, and re-running an instance reconciles it in place.
type K8sApplier struct {
	Client client.Client
}

// Apply creates each object, or updates it in place if it already exists. Objects
// are applied in the order given (golden pool, then Secret, then Sandbox) so a
// fork never references a pool or secret that is not yet present.
func (a *K8sApplier) Apply(ctx context.Context, objs ...client.Object) error {
	for _, o := range objs {
		if err := a.Client.Create(ctx, o); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("apply %T %s/%s: %w", o, o.GetNamespace(), o.GetName(), err)
			}
			// Already there: carry the live resourceVersion onto the desired
			// object and update it in place.
			existing := o.DeepCopyObject().(client.Object)
			if err := a.Client.Get(ctx, client.ObjectKeyFromObject(o), existing); err != nil {
				return fmt.Errorf("apply %T %s/%s: get existing: %w", o, o.GetNamespace(), o.GetName(), err)
			}
			o.SetResourceVersion(existing.GetResourceVersion())
			if err := a.Client.Update(ctx, o); err != nil {
				return fmt.Errorf("apply %T %s/%s: update: %w", o, o.GetNamespace(), o.GetName(), err)
			}
		}
	}
	return nil
}
