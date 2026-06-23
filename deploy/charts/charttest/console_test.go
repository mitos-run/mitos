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
// the SAME chart turns on edition/signup/billing and adds the openbao provider —
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
