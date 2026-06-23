package admission

import (
	"context"
	"encoding/json"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	v1 "mitos.run/mitos/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// sarClient builds a fake client whose SubjectAccessReview create is answered by
// allow: the SAR's ResourceAttributes are passed to allow, which returns the
// decision the apiserver would. It records the last SAR the handler built so the
// test can assert the handler asked the right question.
func sarClient(t *testing.T, allow func(*authzv1.ResourceAttributes) bool, seen **authzv1.SubjectAccessReview) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := authzv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add authz scheme: %v", err)
	}
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatalf("add v1alpha1 scheme: %v", err)
	}
	return fakeclient.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
			sar, ok := obj.(*authzv1.SubjectAccessReview)
			if !ok {
				t.Fatalf("create of non-SAR object: %T", obj)
			}
			if seen != nil {
				*seen = sar
			}
			sar.Status.Allowed = sar.Spec.ResourceAttributes != nil && allow(sar.Spec.ResourceAttributes)
			return nil
		},
	}).Build()
}

func claimRequest(t *testing.T, sa, user string) admission.Request {
	t.Helper()
	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "tenant-a"},
		Spec:       v1.SandboxSpec{ServiceAccount: sa},
	}
	raw, err := json.Marshal(claim)
	if err != nil {
		t.Fatalf("marshal claim: %v", err)
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Namespace: "tenant-a",
		UserInfo:  authnv1.UserInfo{Username: user, Groups: []string{"system:authenticated"}},
		Object:    runtime.RawExtension{Raw: raw},
	}}
}

func newValidatorForTest(t *testing.T, c client.Client) *ClaimServiceAccountValidator {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatalf("add v1alpha1 scheme: %v", err)
	}
	return &ClaimServiceAccountValidator{Client: c, Decoder: admission.NewDecoder(scheme)}
}

func TestClaimPrincipalAllowedWhenUserCanImpersonate(t *testing.T) {
	var seen *authzv1.SubjectAccessReview
	c := sarClient(t, func(ra *authzv1.ResourceAttributes) bool {
		return ra.Verb == "impersonate" && ra.Resource == "serviceaccounts" &&
			ra.Namespace == "tenant-a" && ra.Name == "prod-sa"
	}, &seen)
	v := newValidatorForTest(t, c)

	resp := v.Handle(context.Background(), claimRequest(t, "prod-sa", "alice"))
	if !resp.Allowed {
		t.Fatalf("expected allow, got denied: %s", resp.Result.Message)
	}
	if seen == nil || seen.Spec.User != "alice" {
		t.Fatalf("SAR not built for the request user: %+v", seen)
	}
}

func TestClaimPrincipalDeniedWhenUserCannotImpersonate(t *testing.T) {
	// allow nothing: the creator may not impersonate the named SA.
	c := sarClient(t, func(*authzv1.ResourceAttributes) bool { return false }, nil)
	v := newValidatorForTest(t, c)

	resp := v.Handle(context.Background(), claimRequest(t, "prod-sa", "mallory"))
	if resp.Allowed {
		t.Fatal("expected denial: a user who cannot impersonate prod-sa must not set it as the claim principal")
	}
}

func TestClaimPrincipalAllowedWhenNoServiceAccount(t *testing.T) {
	// No SAR should even be attempted when spec.serviceAccount is empty.
	c := sarClient(t, func(*authzv1.ResourceAttributes) bool {
		t.Fatal("SAR must not be issued when no principal is asserted")
		return false
	}, nil)
	v := newValidatorForTest(t, c)

	resp := v.Handle(context.Background(), claimRequest(t, "", "alice"))
	if !resp.Allowed {
		t.Fatalf("expected allow for empty serviceAccount, got: %s", resp.Result.Message)
	}
}
