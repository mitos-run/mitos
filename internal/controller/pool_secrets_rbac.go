package controller

import (
	"context"
	"fmt"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// ControllerServiceAccountName is the controller's ServiceAccount (chart
	// templates/serviceaccount.yaml). It is the subject of the per-pool-namespace
	// Secrets RoleBinding.
	ControllerServiceAccountName = "mitos-controller"
	// PoolSecretsClusterRoleName is a chart-provisioned ClusterRole holding the
	// Secrets verbs the controller needs in a pool namespace. It is never bound
	// cluster-wide; the controller binds it per pool namespace via a RoleBinding,
	// which is what lets the cluster-wide Secrets grant be removed.
	PoolSecretsClusterRoleName = "mitos-pool-secrets"
	// PoolSecretsRoleBindingName is the RoleBinding the controller creates in each
	// pool namespace binding ControllerServiceAccountName to
	// PoolSecretsClusterRoleName.
	PoolSecretsRoleBindingName = "mitos-pool-secrets"
)

// EnsurePoolSecretsRoleBinding grants the controller Secrets access in a pool
// namespace by creating a RoleBinding there that binds the controller
// ServiceAccount to the cluster-wide-defined-but-not-cluster-bound
// PoolSecretsClusterRoleName ClusterRole. It is the namespaced grant that
// replaces the controller's cluster-wide Secrets permission: with these per-pool
// bindings in place, a stolen controller token can read Secrets only in the
// controller namespace and the pool namespaces it has adopted, not cluster-wide.
//
// The controller can create this binding without holding the Secrets verbs
// itself because the chart grants it the `bind` verb scoped to exactly this one
// ClusterRole (the sanctioned escalation-prevention bypass).
//
// Idempotent: a present binding is left as is (RBAC RoleRef is immutable, so a
// blind re-create would fail). Binding into the controller's own namespace is a
// noop; its own Secrets access is a chart-shipped Role there.
func EnsurePoolSecretsRoleBinding(ctx context.Context, c client.Client, controllerNamespace, poolNamespace string) error {
	if poolNamespace == controllerNamespace {
		return nil
	}
	var existing rbacv1.RoleBinding
	err := c.Get(ctx, types.NamespacedName{Namespace: poolNamespace, Name: PoolSecretsRoleBindingName}, &existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("read pool secrets rolebinding %s/%s: %w", poolNamespace, PoolSecretsRoleBindingName, err)
	}
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: poolNamespace, Name: PoolSecretsRoleBindingName},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     PoolSecretsClusterRoleName,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      ControllerServiceAccountName,
			Namespace: controllerNamespace,
		}},
	}
	if err := c.Create(ctx, rb); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create pool secrets rolebinding %s/%s: %w", poolNamespace, PoolSecretsRoleBindingName, err)
	}
	return nil
}
