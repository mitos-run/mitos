// Package admission holds the controller's validating admission webhooks.
package admission

import (
	"context"
	"fmt"
	"net/http"

	authzv1 "k8s.io/api/authorization/v1"
	v1 "mitos.run/mitos/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// ClaimServiceAccountValidator is the validating webhook that binds a
// SandboxClaim's spec.serviceAccount to real authorization. The
// memory-snapshot resume gate (maybeResumeMemory) serves a workspace head's
// in-RAM memory image only to a claim whose spec.serviceAccount equals the
// snapshot's MemorySnapshotPrincipal. spec.serviceAccount is a free-form field
// the claim author sets, so without this webhook a tenant could set it to
// another principal's value and resume that principal's secrets-in-RAM. This
// validator requires the request creator to be authorized to impersonate the
// named ServiceAccount, via a SubjectAccessReview run with the creator's
// identity, before the claim is admitted, so the principal field can be trusted
// as an authorization boundary by the controller.
type ClaimServiceAccountValidator struct {
	// Client creates SubjectAccessReviews against the apiserver.
	Client client.Client
	// Decoder decodes the admission request payload into a SandboxClaim.
	Decoder admission.Decoder
}

// Handle implements admission.Handler. It allows the request when no principal
// is asserted, or when the creator may impersonate the named ServiceAccount;
// otherwise it denies. It fails closed: a SubjectAccessReview error is a denial,
// never a silent allow, so an authorization-service outage cannot be used to
// smuggle an unauthorized principal past the gate.
func (v *ClaimServiceAccountValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	claim := &v1.Sandbox{}
	if err := v.Decoder.Decode(req, claim); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	sa := claim.Spec.ServiceAccount
	if sa == "" {
		// No principal asserted: nothing to authorize. The memory-snapshot gate
		// only honors a non-empty principal, so an empty value cannot resume any
		// principal-bound image.
		return admission.Allowed("no service account principal asserted")
	}

	// Build the SAR with the REQUEST creator's identity (not the controller's),
	// asking whether they may impersonate the named ServiceAccount in the claim's
	// namespace. impersonate on serviceaccounts is the standard verb for "act as
	// this ServiceAccount".
	extra := map[string]authzv1.ExtraValue{}
	for k, vals := range req.UserInfo.Extra {
		extra[k] = authzv1.ExtraValue(vals)
	}
	sar := &authzv1.SubjectAccessReview{
		Spec: authzv1.SubjectAccessReviewSpec{
			User:   req.UserInfo.Username,
			UID:    req.UserInfo.UID,
			Groups: req.UserInfo.Groups,
			Extra:  extra,
			ResourceAttributes: &authzv1.ResourceAttributes{
				Namespace: claim.Namespace,
				Verb:      "impersonate",
				Group:     "",
				Version:   "v1",
				Resource:  "serviceaccounts",
				Name:      sa,
			},
		},
	}
	if err := v.Client.Create(ctx, sar); err != nil {
		// Fail closed: an authorization check that cannot complete is a denial.
		return admission.Denied(fmt.Sprintf(
			"could not authorize service account principal %q (authorization check failed: %v); the claim is refused so an unauthorized principal cannot be admitted",
			sa, err))
	}
	if sar.Status.Allowed {
		return admission.Allowed("creator may impersonate the asserted service account")
	}
	return admission.Denied(fmt.Sprintf(
		"not authorized to set spec.serviceAccount=%q: the claim creator must be permitted to impersonate that ServiceAccount in namespace %q (RBAC verb 'impersonate' on resource 'serviceaccounts'). spec.serviceAccount is the principal a memory-snapshot resume is bound to, so it may only name a ServiceAccount you can act as.",
		sa, claim.Namespace))
}
