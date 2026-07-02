package charttest

import (
	"strings"
	"testing"
)

// Usage metering chart wiring (issue #602). The controller grew the collector,
// internal usage API, and durable-store flags long ago, but the chart never
// rendered them, so hosted metering stayed off and credits never drew down.
// These tests pin the wiring: OFF by default (a self-host install renders none
// of it), and when controller.usage.collector is true the controller gets the
// flags plus the DSN and bearer-token env (secretKeyRef ONLY), a ClusterIP
// Service exposes the usage API port, and the console gets the matching
// MITOS_USAGE_API_URL and MITOS_USAGE_API_TOKEN env.

// usageEnabledSets is the canonical enabled render the positive tests share.
func usageEnabledSets() []string {
	return []string{
		"controller.usage.collector=true",
		"controller.usage.tokenSecret.name=mitos-usage",
		"database.dsnSecretRef.name=mitos-db",
	}
}

// controllerDeployment extracts the mitos-controller Deployment YAML document so
// an assertion targets the controller container specifically.
func controllerDeployment(t *testing.T, out string) string {
	t.Helper()
	docs := strings.Split(out, "\n---\n")
	for _, d := range docs {
		if strings.Contains(d, "kind: Deployment") && strings.Contains(d, "name: mitos-controller") {
			return d
		}
	}
	t.Fatal("mitos-controller Deployment not found in rendered manifests")
	return ""
}

// consoleDeployment extracts the mitos-console Deployment YAML document.
func consoleDeployment(t *testing.T, out string) string {
	t.Helper()
	docs := strings.Split(out, "\n---\n")
	for _, d := range docs {
		if strings.Contains(d, "kind: Deployment") && strings.Contains(d, "name: mitos-console") {
			return d
		}
	}
	t.Fatal("mitos-console Deployment not found in rendered manifests")
	return ""
}

// TestUsageMeteringOffByDefault asserts the default render carries NO usage
// metering surface: no collector flags, no usage env on controller or console,
// and no usage API Service. A self-host install that does not want metering is
// unaffected.
func TestUsageMeteringOffByDefault(t *testing.T) {
	out := render(t)
	for _, needle := range []string{
		"--usage-collector",
		"--usage-api-address",
		"MITOS_USAGE_API_TOKEN",
		"MITOS_USAGE_API_URL",
		"name: mitos-controller-usage",
	} {
		if strings.Contains(out, needle) {
			t.Fatalf("%q rendered by default; usage metering must be opt-in", needle)
		}
	}
}

// TestUsageMeteringEnabledRendersControllerFlags asserts the enabled render
// carries the collector flags and interval plus the internal usage API address
// on the controller container.
func TestUsageMeteringEnabledRendersControllerFlags(t *testing.T) {
	dep := controllerDeployment(t, render(t, usageEnabledSets()...))
	for _, needle := range []string{
		"- --usage-collector",
		"- --usage-collector-interval=60s",
		"- --usage-api-address=:8092",
	} {
		if !strings.Contains(dep, needle) {
			t.Fatalf("controller Deployment missing %q when controller.usage.collector is true", needle)
		}
	}
}

// TestUsageMeteringEnabledWiresControllerSecrets asserts the DSN and the bearer
// token reach the controller via secretKeyRef ONLY (the existing
// database.dsnSecretRef pattern for MITOS_DATABASE_DSN; the dedicated
// tokenSecret for MITOS_USAGE_API_TOKEN). Values never appear inline.
func TestUsageMeteringEnabledWiresControllerSecrets(t *testing.T) {
	dep := controllerDeployment(t, render(t, usageEnabledSets()...))
	mustNamedSecretKeyRef(t, dep, "MITOS_DATABASE_DSN", "mitos-db", "dsn")
	mustNamedSecretKeyRef(t, dep, "MITOS_USAGE_API_TOKEN", "mitos-usage", "usage-api-token")
}

// TestUsageMeteringEnabledRendersService asserts a ClusterIP Service exposes
// the internal usage API port so the console can reach the controller.
func TestUsageMeteringEnabledRendersService(t *testing.T) {
	out := render(t, usageEnabledSets()...)
	docs := strings.Split(out, "\n---\n")
	for _, d := range docs {
		if strings.Contains(d, "kind: Service") && strings.Contains(d, "name: mitos-controller-usage") {
			if !strings.Contains(d, "port: 8092") {
				t.Fatalf("mitos-controller-usage Service does not expose port 8092:\n%s", d)
			}
			return
		}
	}
	t.Fatal("mitos-controller-usage Service not rendered when controller.usage.collector is true")
}

// TestUsageMeteringEnabledWiresConsole asserts the console Deployment gets the
// matching read-side env: the in-cluster usage API URL and the SAME bearer
// token via secretKeyRef.
func TestUsageMeteringEnabledWiresConsole(t *testing.T) {
	dep := consoleDeployment(t, render(t, usageEnabledSets()...))
	mustEnv(t, dep, "MITOS_USAGE_API_URL", "http://mitos-controller-usage.mitos.svc:8092")
	mustNamedSecretKeyRef(t, dep, "MITOS_USAGE_API_TOKEN", "mitos-usage", "usage-api-token")
}

// TestUsageMeteringConsoleURLOverride asserts console.usage.url overrides the
// derived in-cluster Service URL (an out-of-cluster or split deployment).
func TestUsageMeteringConsoleURLOverride(t *testing.T) {
	sets := append(usageEnabledSets(), "console.usage.url=http://usage.internal:9000")
	dep := consoleDeployment(t, render(t, sets...))
	mustEnv(t, dep, "MITOS_USAGE_API_URL", "http://usage.internal:9000")
}
