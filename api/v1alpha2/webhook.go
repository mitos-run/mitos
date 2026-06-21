package v1alpha2

import (
	ctrl "sigs.k8s.io/controller-runtime"
)

// SetupSandboxPoolWebhookWithManager registers the SandboxPool conversion
// webhook on the manager (ADR 0007, issue #23). controller-runtime serves the
// conversion at /convert for every Hub/Convertible pair in the scheme; this
// call wires the v1alpha2 SandboxPool (the Convertible) against the v1alpha1
// SandboxPool (the Hub) so the API server can translate between the two served
// versions.
//
// It is GUARDED behind an explicit call (not an init) so envtest and the
// production manager opt in: a deployment that has not yet installed the
// conversion webhook config keeps serving only v1alpha1 unaffected. The
// storage-version flip to v1alpha2 is the staged continuation, not done here.
func SetupSandboxPoolWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &SandboxPool{}).
		Complete()
}
