package runmanifest

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// TestExpandPublicURL covers the single, well-defined MITOS_PUBLIC_URL
// substitution: only ${MITOS_PUBLIC_URL} is expanded, every other token is left
// exactly as written, an empty url leaves the reference literal, and there is no
// shell, path, or arithmetic expansion (no injection surface).
func TestExpandPublicURL(t *testing.T) {
	const url = "https://app.mitos.run"
	cases := []struct {
		name string
		in   string
		url  string
		want string
	}{
		{"braced", "origin=${MITOS_PUBLIC_URL}", url, "origin=https://app.mitos.run"},
		{"embedded", "${MITOS_PUBLIC_URL}/callback", url, "https://app.mitos.run/callback"},
		{"twice", "${MITOS_PUBLIC_URL},${MITOS_PUBLIC_URL}", url, "https://app.mitos.run,https://app.mitos.run"},
		{"no token", "plain value", url, "plain value"},
		{"unknown var left literal", "${OTHER} and ${MITOS_PUBLIC_URL}", url, "${OTHER} and https://app.mitos.run"},
		{"bare dollar not expanded", "$MITOS_PUBLIC_URL", url, "$MITOS_PUBLIC_URL"},
		{"empty url leaves literal", "${MITOS_PUBLIC_URL}", "", "${MITOS_PUBLIC_URL}"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := expandPublicURL(c.in, c.url); got != c.want {
				t.Errorf("expandPublicURL(%q, %q) = %q, want %q", c.in, c.url, got, c.want)
			}
		})
	}
}

// TestGoldenPoolInjectsPublicURL asserts the resolved public URL is injected as
// MITOS_PUBLIC_URL into the golden template env (and the captured-running workload
// env), and that ${MITOS_PUBLIC_URL} references in run.command and run.env are
// substituted with the resolved URL so a snapshot-after-ready app self-configures
// at build time.
func TestGoldenPoolInjectsPublicURL(t *testing.T) {
	m, err := Parse([]byte(`
name: app
source:
  image: ghcr.io/x/y:latest
run:
  command: ["node", "app.js", "--origin", "${MITOS_PUBLIC_URL}"]
  env:
    ALLOWED_ORIGINS: ${MITOS_PUBLIC_URL}
    HOME: /home/node
  ready:
    http: { port: 8080, path: /healthz, expect: 200 }
    timeout: 60s
preview:
  port: 8080
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	const url = "https://app.mitos.run"
	pool, err := m.GoldenPool("ns", url)
	if err != nil {
		t.Fatalf("GoldenPool: %v", err)
	}
	tpl := pool.Spec.Template

	// MITOS_PUBLIC_URL is injected into the template env.
	if got := envValue(tpl.Env, PublicURLEnvVar); got != url {
		t.Errorf("template env %s = %q, want %q", PublicURLEnvVar, got, url)
	}
	// run.env reference is substituted.
	if got := envValue(tpl.Env, "ALLOWED_ORIGINS"); got != url {
		t.Errorf("ALLOWED_ORIGINS = %q, want %q", got, url)
	}
	// A non-referencing run.env value is untouched.
	if got := envValue(tpl.Env, "HOME"); got != "/home/node" {
		t.Errorf("HOME = %q, want /home/node", got)
	}
	// run.command reference is substituted (the resolved URL appears literally in
	// the command, not a token).
	joined := strings.Join(tpl.Command, " ")
	if !strings.Contains(joined, url) {
		t.Errorf("command %v does not carry the resolved URL %q", tpl.Command, url)
	}
	if strings.Contains(joined, "${MITOS_PUBLIC_URL}") {
		t.Errorf("command %v still carries an unresolved token", tpl.Command)
	}
	// The captured-running workload env carries it too (build-time config).
	if tpl.Workload == nil {
		t.Fatal("expected a workload from the ready gate")
	}
	if got := envValue(tpl.Workload.Env, PublicURLEnvVar); got != url {
		t.Errorf("workload env %s = %q, want %q", PublicURLEnvVar, got, url)
	}
	if got := envValue(tpl.Workload.Env, "ALLOWED_ORIGINS"); got != url {
		t.Errorf("workload ALLOWED_ORIGINS = %q, want %q", got, url)
	}
}

// TestGoldenPoolNoURLLeavesLiteral asserts an empty public URL leaves references
// literal and injects no MITOS_PUBLIC_URL (back-compat for callers with no URL).
func TestGoldenPoolNoURLLeavesLiteral(t *testing.T) {
	m, err := Parse([]byte(`
name: app
source:
  image: ghcr.io/x/y:latest
run:
  env:
    ALLOWED_ORIGINS: ${MITOS_PUBLIC_URL}
preview:
  port: 8080
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	pool, err := m.GoldenPool("ns", "")
	if err != nil {
		t.Fatalf("GoldenPool: %v", err)
	}
	if got := envValue(pool.Spec.Template.Env, PublicURLEnvVar); got != "" {
		t.Errorf("no MITOS_PUBLIC_URL should be injected with an empty url, got %q", got)
	}
	if got := envValue(pool.Spec.Template.Env, "ALLOWED_ORIGINS"); got != "${MITOS_PUBLIC_URL}" {
		t.Errorf("ALLOWED_ORIGINS = %q, want the literal token left intact", got)
	}
}

// envValue is a test helper returning the value of name in a []corev1.EnvVar.
func envValue(env []corev1.EnvVar, name string) string {
	for _, e := range env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}
