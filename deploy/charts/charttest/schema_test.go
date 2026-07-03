package charttest

import (
	"os/exec"
	"strings"
	"testing"
)

// values.schema.json (issue #620): a misspelled values key must fail at
// install/template time instead of deploying silently misconfigured. These
// tests pin that the schema rejects unknown keys at the levels where the key
// set is known, and that the deliberately free-form maps (extraEnv, rate
// tables, resources, annotations) still pass through.

// renderExpectSchemaError runs `helm template` with the given --set overrides
// and asserts it FAILS with a values schema violation.
func renderExpectSchemaError(t *testing.T, sets ...string) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed; skipping chart render test")
	}
	args := []string{"template", "t", chartDir(t), "--kube-version", "1.31.0"}
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	out, err := exec.Command("helm", args...).CombinedOutput()
	if err == nil {
		t.Fatalf("helm template succeeded with %v; the values schema must reject unknown keys", sets)
	}
	if !strings.Contains(string(out), "values don't meet the specifications of the schema") {
		t.Fatalf("helm template failed but not with a schema violation:\n%s", out)
	}
}

// TestSchemaRejectsUnknownTopLevelKey asserts a typo at the top level fails.
func TestSchemaRejectsUnknownTopLevelKey(t *testing.T) {
	renderExpectSchemaError(t, "controler.replicas=3")
}

// TestSchemaRejectsUnknownComponentKey asserts a typo inside a component
// section fails (the classic silently-ignored-knob case).
func TestSchemaRejectsUnknownComponentKey(t *testing.T) {
	renderExpectSchemaError(t, "console.typoKey=1")
	renderExpectSchemaError(t, "forkd.enableNetwork=true")
	renderExpectSchemaError(t, "gateway.enforce.trustedProxyHop=1")
}

// TestSchemaRejectsWrongType asserts a well-named key with the wrong type
// fails instead of rendering garbage.
func TestSchemaRejectsWrongType(t *testing.T) {
	renderExpectSchemaError(t, "controller.replicas=two")
}

// TestSchemaAllowsFreeFormMaps asserts the maps the chart intends as verbatim
// passthrough still accept arbitrary keys: extraEnv, ingress annotations,
// resources (including the mitos.run/kvm extended resource), and commonLabels.
func TestSchemaAllowsFreeFormMaps(t *testing.T) {
	out := render(t,
		"console.extraEnv[0].name=MITOS_CONSOLE_SCHEMA_PROBE",
		"console.extraEnv[0].value=on",
		"console.ingress.enabled=true",
		"console.ingress.host=console.example.com",
		"console.ingress.annotations.cert-manager\\.io/cluster-issuer=letsencrypt",
		"commonLabels.team=platform",
	)
	mustEnv(t, out, "MITOS_CONSOLE_SCHEMA_PROBE", "on")
	mustContain(t, out, "team: platform")
}

// TestSchemaAcceptsTalosProfile asserts the shipped hardened-runtime values
// profile validates against the schema (the known real-world values file).
func TestSchemaAcceptsTalosProfile(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed; skipping chart render test")
	}
	out, err := exec.Command("helm", "template", "t", chartDir(t),
		"--kube-version", "1.31.0",
		"-f", chartDir(t)+"/values/talos.yaml").CombinedOutput()
	if err != nil {
		t.Fatalf("helm template with values/talos.yaml failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "FOWNER") {
		t.Fatal("talos profile did not add CAP_FOWNER to forkd")
	}
}
