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

// TestGatewayEnforcementDefaultsOn asserts the hosted profile renders the gateway
// with quota/abuse enforcement enabled by default and the trusted-proxy-hops flag
// set, so the public front door enforces quotas out of the box.
func TestGatewayEnforcementDefaultsOn(t *testing.T) {
	out := render(t)
	deploy := section(t, out, "kind: Deployment", "mitos-gateway")
	mustContain(t, deploy, "--enforce-quota=true")
	mustContain(t, deploy, "--trusted-proxy-hops=0")
}

// TestGatewayEnforcementOverridable asserts an operator can disable enforcement
// and set the trusted-proxy hop count, so a trusted single-tenant deployment can
// opt out and a deployment behind an ingress can resolve the client IP correctly.
func TestGatewayEnforcementOverridable(t *testing.T) {
	out := render(t, "gateway.enforce.enabled=false", "gateway.enforce.trustedProxyHops=1")
	deploy := section(t, out, "kind: Deployment", "mitos-gateway")
	mustContain(t, deploy, "--enforce-quota=false")
	mustContain(t, deploy, "--trusted-proxy-hops=1")
}

// TestGatewaySingleTenantNamespaceAbsentByDefault asserts the default render
// does NOT inject MITOS_GATEWAY_SINGLE_TENANT_NAMESPACE: the default is
// per-org namespace mode and the QA override must be an explicit opt-in.
func TestGatewaySingleTenantNamespaceAbsentByDefault(t *testing.T) {
	out := render(t)
	if strings.Contains(out, "MITOS_GATEWAY_SINGLE_TENANT_NAMESPACE") {
		t.Fatal("default render contains MITOS_GATEWAY_SINGLE_TENANT_NAMESPACE; single-tenant mode must be an explicit opt-in")
	}
}

// TestGatewaySingleTenantNamespaceRendersWhenSet asserts that setting
// gateway.singleTenantNamespace injects MITOS_GATEWAY_SINGLE_TENANT_NAMESPACE
// as a plain env var into the gateway Deployment. The value is a namespace
// name, not a secret, so a direct value (not secretKeyRef) is correct.
func TestGatewaySingleTenantNamespaceRendersWhenSet(t *testing.T) {
	out := render(t, "gateway.singleTenantNamespace=mitos")
	deploy := section(t, out, "kind: Deployment", "mitos-gateway")
	mustContain(t, deploy, "MITOS_GATEWAY_SINGLE_TENANT_NAMESPACE")
	mustContain(t, deploy, `value: "mitos"`)
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
