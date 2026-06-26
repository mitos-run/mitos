// Package charttest renders the Mitos Helm chart with `helm template` and
// asserts the console/gateway components behave per the spec: ONE chart, two
// editions driven by values, the #208 gate enforced server-side (signup/billing
// off by default), and the console gated by console.enabled. It skips when the
// helm binary is not installed so it never blocks a helm-less environment.
package charttest

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// chartDir is the chart relative to this test package.
func chartDir(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("../mitos")
	if err != nil {
		t.Fatalf("abs chart dir: %v", err)
	}
	return abs
}

// render runs `helm template` with the given --set overrides and returns the
// rendered manifests. A kube-version is pinned so the chart's kubeVersion
// constraint is satisfied independent of the host's client default.
func render(t *testing.T, sets ...string) string {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed; skipping chart render test")
	}
	args := []string{"template", "t", chartDir(t), "--kube-version", "1.31.0"}
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	out, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	return string(out)
}

// TestCommunityEditionDefaults asserts the self-host default render: the console
// is present, edition=community, and the #208 gate holds (signup and billing
// off), with only the kube secret provider advertised.
func TestCommunityEditionDefaults(t *testing.T) {
	out := render(t)
	mustContain(t, out, `name: mitos-console`)
	mustContain(t, out, `value: "community"`)
	mustEnv(t, out, "MITOS_CONSOLE_EDITION", "community")
	mustEnv(t, out, "MITOS_CONSOLE_SIGNUP", "false")
	mustEnv(t, out, "MITOS_CONSOLE_BILLING", "false")
	mustEnv(t, out, "MITOS_CONSOLE_SECRET_PROVIDERS", "kube")
}

// TestHostedEditionFlipsServerControlledFlags asserts the hosted SaaS render of
// the SAME chart turns on edition/signup/billing and adds the openbao provider:
// all from values, no separate chart or image.
func TestHostedEditionFlipsServerControlledFlags(t *testing.T) {
	out := render(t,
		"console.edition=hosted",
		"console.signup=true",
		"console.billing.enabled=true",
		"console.secrets.openbao.enabled=true",
		"console.secrets.openbao.address=https://bao.example.com",
	)
	mustEnv(t, out, "MITOS_CONSOLE_EDITION", "hosted")
	mustEnv(t, out, "MITOS_CONSOLE_SIGNUP", "true")
	mustEnv(t, out, "MITOS_CONSOLE_BILLING", "true")
	mustEnv(t, out, "MITOS_CONSOLE_SECRET_PROVIDERS", "kube,openbao")
	mustEnv(t, out, "MITOS_CONSOLE_OPENBAO_ADDR", "https://bao.example.com")
}

// TestConsoleDisabledRendersNothing asserts console.enabled=false removes every
// console resource (Deployment, Service, RBAC, SA).
func TestConsoleDisabledRendersNothing(t *testing.T) {
	out := render(t, "console.enabled=false")
	if strings.Contains(out, "name: mitos-console") {
		t.Fatal("console.enabled=false still rendered a mitos-console resource")
	}
}

// TestConsoleIngressRendersWhenEnabled asserts the optional Ingress renders with
// the configured host.
func TestConsoleIngressRendersWhenEnabled(t *testing.T) {
	out := render(t, "console.ingress.enabled=true", "console.ingress.host=console.example.com")
	mustContain(t, out, "kind: Ingress")
	mustContain(t, out, "host: \"console.example.com\"")
}

// TestGatewayGatedByEnabled asserts the gateway renders by default and is removed
// when disabled.
func TestGatewayGatedByEnabled(t *testing.T) {
	mustContain(t, render(t), "name: mitos-gateway")
	if strings.Contains(render(t, "gateway.enabled=false"), "name: mitos-gateway") {
		t.Fatal("gateway.enabled=false still rendered a mitos-gateway resource")
	}
}

// TestConsoleOIDCRedirectURL asserts the chart emits MITOS_CONSOLE_OIDC_REDIRECT_URL
// when console.oidc.redirectURL is set. The console sends this value as the OAuth
// redirect_uri; without it the IdP rejects the login, so OIDC is unusable via the
// chart.
func TestConsoleOIDCRedirectURL(t *testing.T) {
	out := render(t, "console.oidc.redirectURL=https://app.mitos.run/auth/callback")
	mustEnv(t, out, "MITOS_CONSOLE_OIDC_REDIRECT_URL", "https://app.mitos.run/auth/callback")
}

// TestConsoleOIDCRedirectURLOmittedByDefault asserts the redirect env is absent
// when redirectURL is unset, so installs that do not use OIDC are unaffected.
func TestConsoleOIDCRedirectURLOmittedByDefault(t *testing.T) {
	if strings.Contains(render(t), "MITOS_CONSOLE_OIDC_REDIRECT_URL") {
		t.Fatal("MITOS_CONSOLE_OIDC_REDIRECT_URL rendered when console.oidc.redirectURL is unset")
	}
}

// TestPaddleAbsentByDefault asserts the Paddle env is not rendered in the default
// community install (billing off), so no billing provider config leaks in.
func TestPaddleAbsentByDefault(t *testing.T) {
	out := render(t)
	if strings.Contains(out, "MITOS_CONSOLE_PADDLE_API_KEY") {
		t.Fatal("Paddle API key env rendered by default; billing must be opt-in")
	}
	if strings.Contains(out, "MITOS_CONSOLE_PADDLE_WEBHOOK_SECRET") {
		t.Fatal("Paddle webhook secret env rendered by default")
	}
}

// TestPaddleSecretRefWiring asserts that when billing is enabled and the Paddle
// secret refs are set, the API key and webhook secret are injected via
// secretKeyRef ONLY (never as plaintext values), and the base URL passes through.
func TestPaddleSecretRefWiring(t *testing.T) {
	out := render(t,
		"console.billing.enabled=true",
		"console.billing.paddle.apiKeySecretRef.name=mitos-paddle",
		"console.billing.paddle.webhookSecretRef.name=mitos-paddle",
		"console.billing.paddle.baseURL=https://sandbox-api.paddle.com",
	)
	mustNamedSecretKeyRef(t, out, "MITOS_CONSOLE_PADDLE_API_KEY", "mitos-paddle", "api-key")
	mustNamedSecretKeyRef(t, out, "MITOS_CONSOLE_PADDLE_WEBHOOK_SECRET", "mitos-paddle", "webhook-secret")
	mustEnv(t, out, "MITOS_CONSOLE_PADDLE_BASE_URL", "https://sandbox-api.paddle.com")
}

// TestPaddleSecretValuesNeverPlaintext asserts the chart NEVER renders a Paddle
// secret as a plaintext env value: only the secretKeyRef indirection is allowed.
func TestPaddleSecretValuesNeverPlaintext(t *testing.T) {
	out := render(t,
		"console.billing.enabled=true",
		"console.billing.paddle.apiKeySecretRef.name=mitos-paddle",
		"console.billing.paddle.webhookSecretRef.name=mitos-paddle",
	)
	lines := strings.Split(out, "\n")
	for i, ln := range lines {
		for _, name := range []string{"MITOS_CONSOLE_PADDLE_API_KEY", "MITOS_CONSOLE_PADDLE_WEBHOOK_SECRET"} {
			if strings.TrimSpace(ln) == "- name: "+name && i+1 < len(lines) {
				next := strings.TrimSpace(lines[i+1])
				if strings.HasPrefix(next, "value:") {
					t.Fatalf("%s rendered as plaintext value; must use secretKeyRef", name)
				}
			}
		}
	}
}

// TestConsoleExtraEnv asserts arbitrary console.extraEnv entries pass through, so
// operators can inject env the chart does not model.
func TestConsoleExtraEnv(t *testing.T) {
	out := render(t, "console.extraEnv[0].name=MITOS_CONSOLE_CUSTOM", "console.extraEnv[0].value=on")
	mustEnv(t, out, "MITOS_CONSOLE_CUSTOM", "on")
}

// TestOrgTenancyDisabledByDefault asserts the per-org tenancy surface is OFF in
// the default render: no --enable-org-tenancy flag and no namespace-management
// RBAC rules, so a self-host single-tenant install is unaffected.
func TestOrgTenancyDisabledByDefault(t *testing.T) {
	out := render(t)
	if strings.Contains(out, "--enable-org-tenancy") {
		t.Fatal("--enable-org-tenancy rendered by default; per-org tenancy must be opt-in")
	}
	// The controller ClusterRole must not grant namespace management by default.
	// Match the resource LIST entry (a YAML "- namespaces" line), not a comment
	// that happens to mention "pool namespaces".
	if hasResourceRule(controllerClusterRole(t, out), "namespaces") {
		t.Fatal("controller ClusterRole grants namespaces by default; should be gated behind controller.orgTenancy.enabled")
	}
}

// TestOrgTenancyEnabledAddsFlagAndRBAC asserts controller.orgTenancy.enabled
// renders the --enable-org-tenancy flag with the default-quota flags AND the
// controller RBAC the OrgReconciler needs (namespaces, resourcequotas,
// limitranges, orgs).
func TestOrgTenancyEnabledAddsFlagAndRBAC(t *testing.T) {
	out := render(t,
		"controller.orgTenancy.enabled=true",
		"controller.orgTenancy.defaultMaxSandboxes=75",
		"controller.orgTenancy.defaultCPU=48",
		"controller.orgTenancy.defaultMemory=96Gi",
	)
	mustContain(t, out, "--enable-org-tenancy")
	mustContain(t, out, "--org-default-max-sandboxes=75")
	mustContain(t, out, "--org-default-cpu=48")
	mustContain(t, out, "--org-default-memory=96Gi")

	role := controllerClusterRole(t, out)
	for _, res := range []string{"namespaces", "resourcequotas", "limitranges", "orgs"} {
		if !hasResourceRule(role, res) {
			t.Fatalf("controller ClusterRole missing %q rule when org tenancy is enabled", res)
		}
	}
}

// hasResourceRule reports whether the rendered ClusterRole lists the given
// resource as a rule entry (a YAML "- <resource>" list line), so a comment that
// mentions the word does not falsely satisfy the check.
func hasResourceRule(role, resource string) bool {
	for _, ln := range strings.Split(role, "\n") {
		if strings.TrimSpace(ln) == "- "+resource {
			return true
		}
	}
	return false
}

// controllerClusterRole extracts the mitos-controller ClusterRole YAML document
// from the rendered manifests so an assertion targets that role specifically and
// is not satisfied by some other resource mentioning the same word.
func controllerClusterRole(t *testing.T, out string) string {
	t.Helper()
	docs := strings.Split(out, "\n---\n")
	for _, d := range docs {
		if strings.Contains(d, "kind: ClusterRole") && strings.Contains(d, "name: mitos-controller") {
			return d
		}
	}
	t.Fatal("mitos-controller ClusterRole not found in rendered manifests")
	return ""
}

// mustNamedSecretKeyRef asserts an env var with the given name is sourced from a
// secretKeyRef pointing at secretName/key, and never carries a plaintext value.
func mustNamedSecretKeyRef(t *testing.T, out, name, secretName, key string) {
	t.Helper()
	needle := "- name: " + name
	lines := strings.Split(out, "\n")
	for i, ln := range lines {
		if strings.TrimSpace(ln) != needle {
			continue
		}
		end := i + 6
		if end > len(lines) {
			end = len(lines)
		}
		block := strings.Join(lines[i:end], "\n")
		if !strings.Contains(block, "secretKeyRef:") {
			t.Fatalf("env %s is not sourced from a secretKeyRef", name)
		}
		if !strings.Contains(block, `name: "`+secretName+`"`) {
			t.Fatalf("env %s secretKeyRef does not reference secret %q", name, secretName)
		}
		if !strings.Contains(block, `key: "`+key+`"`) {
			t.Fatalf("env %s secretKeyRef does not use key %q", name, key)
		}
		return
	}
	t.Fatalf("env %s not found in rendered manifests", name)
}

func mustContain(t *testing.T, out, want string) {
	t.Helper()
	if !strings.Contains(out, want) {
		t.Fatalf("rendered manifests missing %q", want)
	}
}

// mustEnv asserts an env var with the given name is set to value somewhere in the
// rendered manifests (name on one line, value on the next).
func mustEnv(t *testing.T, out, name, value string) {
	t.Helper()
	needle := "- name: " + name
	lines := strings.Split(out, "\n")
	for i, ln := range lines {
		if strings.TrimSpace(ln) == needle && i+1 < len(lines) {
			if strings.Contains(lines[i+1], `value: "`+value+`"`) {
				return
			}
		}
	}
	t.Fatalf("env %s=%q not found in rendered manifests", name, value)
}
