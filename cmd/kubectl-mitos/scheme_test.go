package main

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "mitos.run/mitos/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestProductionSchemeKnowsSecret asserts the package-level scheme that main()
// wires into the real controller-runtime client recognizes corev1.Secret. The
// exec command reads the <sandbox>-sandbox-token Secret through that scheme, so
// a missing corev1 registration fails every exec before it reaches the runtime
// endpoint (issue #528). The exec_test helper builds its OWN scheme with corev1
// added, which masked this gap; this test pins the real one.
func TestProductionSchemeKnowsSecret(t *testing.T) {
	if _, _, err := scheme.ObjectKinds(&corev1.Secret{}); err != nil {
		t.Fatalf("production scheme does not recognize corev1.Secret: %v", err)
	}
}

// TestResolveSandboxAuthUsesProductionScheme exercises resolveSandboxAuth with a
// client built from the PRODUCTION scheme (not a test-local one), reproducing
// exactly what kubectl mitos exec does on a live cluster: read the Sandbox and
// its token Secret. Before the corev1 registration this fails with the "no kind
// is registered for the type v1.Secret" error from issue #528.
func TestResolveSandboxAuthUsesProductionScheme(t *testing.T) {
	sandbox := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "mitos"},
		Status: v1.SandboxStatus{
			Phase:     v1.SandboxReady,
			Endpoint:  "10.0.0.5:9091",
			SandboxID: "sbx-id-42",
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-sandbox-token", Namespace: "mitos"},
		Data:       map[string][]byte{"token": []byte("tkn-99")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox, secret).Build()

	ref, endpoint, token, err := resolveSandboxAuth(context.Background(), c, "mitos", "sbx")
	if err != nil {
		t.Fatalf("resolveSandboxAuth with the production scheme: %v", err)
	}
	if ref != "sbx-id-42" {
		t.Errorf("ref = %q, want sbx-id-42", ref)
	}
	if endpoint != "10.0.0.5:9091" {
		t.Errorf("endpoint = %q, want 10.0.0.5:9091", endpoint)
	}
	if token != "tkn-99" {
		t.Errorf("token = %q, want tkn-99", token)
	}
}
