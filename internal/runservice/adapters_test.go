package runservice

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/runmanifest"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestK8sApplierCreateThenUpdate proves the upsert is idempotent: a second run of
// the same app reuses (updates) the golden pool rather than failing on conflict.
func TestK8sApplierCreateThenUpdate(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	ap := &K8sApplier{Client: c}

	m, err := runmanifest.Parse([]byte(openclawYAML))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := m.GoldenPool("ns", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ap.Apply(context.Background(), pool); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	// A fresh object (no resourceVersion), as a second run would build, must update
	// in place, not error.
	pool2, _ := m.GoldenPool("ns", "")
	if err := ap.Apply(context.Background(), pool2); err != nil {
		t.Fatalf("second apply (update): %v", err)
	}

	var got v1.SandboxPool
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "openclaw"}, &got); err != nil {
		t.Fatalf("get pool: %v", err)
	}
	if got.Spec.Template == nil || got.Spec.Template.Image != "ghcr.io/openclaw/openclaw:latest" {
		t.Errorf("pool not applied correctly: %+v", got.Spec.Template)
	}
}

func TestK8sApplierAppliesSecretAndSandbox(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	ap := &K8sApplier{Client: c}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"}, Data: map[string][]byte{"K": []byte("v")}}
	sandbox := &v1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns"}}
	if err := ap.Apply(context.Background(), secret, sandbox); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "inst"}, &v1.Sandbox{}); err != nil {
		t.Errorf("sandbox not applied: %v", err)
	}
}

func TestInstanceLabel(t *testing.T) {
	a, err := instanceLabel("github.com/openclaw/openclaw", "org-123")
	if err != nil {
		t.Fatal(err)
	}
	// Deterministic for the same (src, org).
	b, _ := instanceLabel("github.com/openclaw/openclaw", "org-123")
	if a != b {
		t.Errorf("label not deterministic: %q vs %q", a, b)
	}
	// Different org -> different label (global subdomain uniqueness).
	d, _ := instanceLabel("github.com/openclaw/openclaw", "org-999")
	if a == d {
		t.Error("different orgs produced the same label")
	}
	if !strings.HasPrefix(a, "openclaw-") {
		t.Errorf("label %q should start with the repo name", a)
	}
	// Must be a valid manifest DNS label (Provision will accept it).
	if _, err := runmanifest.Provision(mustManifest(t, openclawYAML), map[string]string{"ANTHROPIC_API_KEY": "x"}, "ns", a, ""); err != nil {
		t.Errorf("instance label not provisionable: %v", err)
	}
}

type fakeAccounts struct {
	org string
	err error
}

func (f fakeAccounts) OrgForEmail(context.Context, string) (string, error) {
	return f.org, f.err
}

func TestTenantResolve(t *testing.T) {
	tr := &TenantResolver{
		CurrentEmail:    func(_ *http.Request) (string, error) { return "jannes@mitos.run", nil },
		Accounts:        fakeAccounts{org: "org-abc"},
		NamespaceForOrg: func(orgID string) string { return "tenant-" + orgID },
	}
	id, err := tr.Resolve(httptest.NewRequest("POST", "/run", nil), "github.com/openclaw/openclaw")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if id.Namespace != "tenant-org-abc" {
		t.Errorf("namespace = %q", id.Namespace)
	}
	if !strings.HasPrefix(id.InstanceLabel, "openclaw-") {
		t.Errorf("label = %q", id.InstanceLabel)
	}
}

func TestContextResolver(t *testing.T) {
	cr := &ContextResolver{
		OrgFromRequest:  func(*http.Request) (string, bool) { return "org-abc", true },
		NamespaceForOrg: func(orgID string) string { return "tenant-" + orgID },
	}
	id, err := cr.Resolve(httptest.NewRequest("POST", "/run", nil), "github.com/openclaw/openclaw")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if id.Namespace != "tenant-org-abc" || !strings.HasPrefix(id.InstanceLabel, "openclaw-") {
		t.Errorf("identity = %+v", id)
	}
	// Unauthenticated -> fail closed.
	cr.OrgFromRequest = func(*http.Request) (string, bool) { return "", false }
	if _, err := cr.Resolve(httptest.NewRequest("POST", "/run", nil), "github.com/o/r"); err == nil {
		t.Fatal("unauthenticated request should fail closed")
	}
}

func TestTenantResolveNotSignedIn(t *testing.T) {
	tr := &TenantResolver{
		CurrentEmail:    func(_ *http.Request) (string, error) { return "", errors.New("no session") },
		Accounts:        fakeAccounts{},
		NamespaceForOrg: func(string) string { return "ns" },
	}
	if _, err := tr.Resolve(httptest.NewRequest("POST", "/run", nil), "github.com/o/r"); err == nil {
		t.Fatal("expected not-signed-in error")
	}
}
