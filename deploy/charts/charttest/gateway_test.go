package charttest

import (
	"strings"
	"testing"
)

// TestGatewayServiceAccountRenders asserts the gateway ServiceAccount renders and
// the gateway pod uses it: the real control plane needs an identity to bind its
// ClusterRole to.
func TestGatewayServiceAccountRenders(t *testing.T) {
	out := render(t)
	mustContain(t, out, "kind: ServiceAccount")
	mustContain(t, out, "name: mitos-gateway")
	mustContain(t, out, "serviceAccountName: mitos-gateway")
}

// TestGatewayRBACRenders asserts the gateway ClusterRole grants exactly the
// least-privilege control-plane surface: Sandboxes lifecycle, SandboxPools read,
// Secrets get (the per-sandbox token), and Namespaces get, bound to the gateway
// ServiceAccount.
func TestGatewayRBACRenders(t *testing.T) {
	out := render(t)
	clusterRole := section(t, out, "kind: ClusterRole\n", "mitos-gateway")
	for _, want := range []string{"sandboxes", "sandboxpools", "secrets", "namespaces", "create", "delete"} {
		if !strings.Contains(clusterRole, want) {
			t.Errorf("gateway ClusterRole missing %q\n%s", want, clusterRole)
		}
	}
	// The gateway must NOT grant write on Secrets: it only reads the token.
	if strings.Contains(secretsRule(t, clusterRole), "update") || strings.Contains(secretsRule(t, clusterRole), "create") {
		t.Error("gateway ClusterRole grants write on Secrets; it must only read the per-sandbox token")
	}
	mustContain(t, out, "kind: ClusterRoleBinding")
}

// TestGatewayRBACAbsentWhenDisabled asserts gateway.enabled=false removes the
// gateway SA and RBAC along with the Deployment.
func TestGatewayRBACAbsentWhenDisabled(t *testing.T) {
	out := render(t, "gateway.enabled=false")
	if strings.Contains(out, "name: mitos-gateway") {
		t.Fatal("gateway.enabled=false still rendered a mitos-gateway resource")
	}
}

// section returns the YAML document containing both marker and name, to scope an
// assertion to one rendered resource.
func section(t *testing.T, out, marker, name string) string {
	t.Helper()
	for _, doc := range strings.Split(out, "\n---") {
		if strings.Contains(doc, marker) && strings.Contains(doc, "name: "+name) {
			return doc
		}
	}
	t.Fatalf("no rendered document with %q and name %q", marker, name)
	return ""
}

// secretsRule returns the verbs block for the secrets resource within a rendered
// ClusterRole, so a test can assert it is read-only.
func secretsRule(t *testing.T, clusterRole string) string {
	t.Helper()
	idx := strings.Index(clusterRole, "- secrets")
	if idx < 0 {
		return ""
	}
	rest := clusterRole[idx:]
	// Stop at the next resources block so we only see the secrets rule's verbs.
	if next := strings.Index(rest[len("- secrets"):], "resources:"); next >= 0 {
		return rest[:next]
	}
	return rest
}
