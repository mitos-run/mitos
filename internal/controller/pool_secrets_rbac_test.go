package controller_test

import (
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/types"
	"mitos.run/mitos/internal/controller"
)

// TestEnsurePoolSecretsRoleBindingCreatesNamespacedBinding proves the controller
// grants ITSELF Secrets access in a pool namespace by creating a namespaced
// RoleBinding to the pre-provisioned mitos-pool-secrets ClusterRole, bound to the
// controller ServiceAccount. This is what lets the cluster-wide Secrets grant be
// removed: the controller reaches Secrets only in namespaces it has adopted.
func TestEnsurePoolSecretsRoleBindingCreatesNamespacedBinding(t *testing.T) {
	c := newCoreClient(t)
	ctrlNs := newPKINamespace(t, c)
	poolNs := newPKINamespace(t, c)

	if err := controller.EnsurePoolSecretsRoleBinding(ctx, c, ctrlNs, poolNs); err != nil {
		t.Fatalf("EnsurePoolSecretsRoleBinding: %v", err)
	}

	var rb rbacv1.RoleBinding
	if err := c.Get(ctx, types.NamespacedName{Namespace: poolNs, Name: controller.PoolSecretsRoleBindingName}, &rb); err != nil {
		t.Fatalf("rolebinding not created in pool namespace: %v", err)
	}
	if rb.RoleRef.Kind != "ClusterRole" || rb.RoleRef.Name != controller.PoolSecretsClusterRoleName {
		t.Errorf("roleRef = %+v, want ClusterRole/%s", rb.RoleRef, controller.PoolSecretsClusterRoleName)
	}
	if len(rb.Subjects) != 1 {
		t.Fatalf("subjects = %+v, want exactly 1", rb.Subjects)
	}
	s := rb.Subjects[0]
	if s.Kind != "ServiceAccount" || s.Name != controller.ControllerServiceAccountName || s.Namespace != ctrlNs {
		t.Errorf("subject = %+v, want ServiceAccount %s/%s", s, ctrlNs, controller.ControllerServiceAccountName)
	}

	// Idempotent: a second call must not error (RoleRef is immutable, so a blind
	// re-create would fail; the function must tolerate the existing binding).
	if err := controller.EnsurePoolSecretsRoleBinding(ctx, c, ctrlNs, poolNs); err != nil {
		t.Fatalf("second EnsurePoolSecretsRoleBinding: %v", err)
	}
}

// TestEnsurePoolSecretsRoleBindingSameNamespaceIsNoop proves the controller does
// not bind itself in its own namespace (its own Secrets access is granted by a
// chart-shipped Role there, not by this per-pool path).
func TestEnsurePoolSecretsRoleBindingSameNamespaceIsNoop(t *testing.T) {
	c := newCoreClient(t)
	ns := newPKINamespace(t, c)
	if err := controller.EnsurePoolSecretsRoleBinding(ctx, c, ns, ns); err != nil {
		t.Fatalf("same-namespace should be a noop, got %v", err)
	}
	var rb rbacv1.RoleBinding
	err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: controller.PoolSecretsRoleBindingName}, &rb)
	if err == nil {
		t.Error("expected no rolebinding in the controller's own namespace")
	}
}
