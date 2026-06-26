// Package orgprovision implements the onboarding.OrgProvisioner seam over a
// controller-runtime client: a verified signup creates the cluster-scoped Org
// custom resource (api/v1.Org, name = org id), which the OrgReconciler turns into
// a per-org isolation namespace (issue #288). It is the signup -> namespace
// integration, gated by the same cluster-mode config that enables org tenancy.
//
// The provisioner is deliberately small and idempotent: an already-existing Org
// is a success (a re-verify or a controller retry must not error), and only the
// presentational DisplayName is reconciled on update. The org id, the namespace,
// the quota, and the default-deny policy are all derived server-side by the
// reconciler; this package never trusts client input for them.
package orgprovision

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "mitos.run/mitos/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Provisioner creates and updates the tenant Org custom resource over a
// controller-runtime client. It satisfies onboarding.OrgProvisioner.
type Provisioner struct {
	client client.Client
}

// New builds a Provisioner over c. c must have api/v1 registered in its scheme.
func New(c client.Client) *Provisioner {
	return &Provisioner{client: c}
}

// Provision ensures the cluster-scoped Org with metadata.name == orgID exists,
// carrying displayName in its spec. It is idempotent:
//
//   - if the Org does not exist, it is created;
//   - if it already exists, an AlreadyExists on create is treated as success, and
//     the displayName is reconciled with an update only if it drifted.
//
// The orgID is the trusted server-derived id (the Personal org id minted at
// signup); it is used verbatim as the resource name so NamespaceForOrg maps it to
// mitos-org-<id>. Provision never returns an error for an existing org.
func (p *Provisioner) Provision(ctx context.Context, orgID, displayName string) error {
	if orgID == "" {
		return fmt.Errorf("orgprovision: org id is required")
	}

	org := &v1.Org{
		ObjectMeta: metav1.ObjectMeta{Name: orgID},
		Spec:       v1.OrgSpec{DisplayName: displayName},
	}
	err := p.client.Create(ctx, org)
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("orgprovision: create org %q: %w", orgID, err)
	}

	// Already exists: reconcile the display name only if it drifted, so a re-verify
	// converges without thrashing the resourceVersion.
	existing := &v1.Org{}
	if gerr := p.client.Get(ctx, client.ObjectKey{Name: orgID}, existing); gerr != nil {
		return fmt.Errorf("orgprovision: load existing org %q: %w", orgID, gerr)
	}
	if existing.Spec.DisplayName == displayName {
		return nil
	}
	existing.Spec.DisplayName = displayName
	if uerr := p.client.Update(ctx, existing); uerr != nil {
		return fmt.Errorf("orgprovision: update org %q display name: %w", orgID, uerr)
	}
	return nil
}
