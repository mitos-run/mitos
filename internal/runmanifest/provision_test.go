package runmanifest

import (
	"strings"
	"testing"

	v1 "mitos.run/mitos/api/v1"
)

// TestGoldenPoolEgress asserts the manifest egress allowlist becomes a default-deny
// pool network policy.
func TestGoldenPoolEgress(t *testing.T) {
	m, err := Parse(mustRead(t, "openclaw.yaml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	pool, err := m.GoldenPool("sandboxes", "")
	if err != nil {
		t.Fatalf("GoldenPool: %v", err)
	}
	np := pool.Spec.Template.Network
	if np == nil {
		t.Fatal("expected a default-deny network policy from egress")
	}
	if np.Egress != v1.EgressDeny {
		t.Errorf("egress = %q, want deny", np.Egress)
	}
	if len(np.Allow) != 2 || np.Allow[0] != "api.anthropic.com" {
		t.Errorf("allow = %v", np.Allow)
	}
}

// TestProvisionOpenClaw is the core of slice 2: clicker supplies one key, the
// mintable token is generated, and the result is a fork of the golden with the
// secret mounted, expose configured, workspace bound, and non-inheritance set.
func TestProvisionOpenClaw(t *testing.T) {
	m, err := Parse(mustRead(t, "openclaw.yaml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	res, err := Provision(m, map[string]string{"ANTHROPIC_API_KEY": "sk-real"}, "tenant-jannes", "jannes-openclaw", "")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// Secret holds the supplied value and a minted token (32 bytes -> 64 hex chars).
	if got := string(res.Secret.Data["ANTHROPIC_API_KEY"]); got != "sk-real" {
		t.Errorf("supplied secret = %q", got)
	}
	tok := string(res.Secret.Data["OPENCLAW_GATEWAY_TOKEN"])
	if len(tok) != 64 {
		t.Errorf("minted token len = %d, want 64 hex chars", len(tok))
	}

	sb := res.Sandbox
	if sb.Spec.Source.PoolRef == nil || sb.Spec.Source.PoolRef.Name != "openclaw" {
		t.Errorf("poolRef = %+v, want openclaw", sb.Spec.Source.PoolRef)
	}
	if sb.Spec.SecretInheritance != v1.SecretReissue {
		t.Errorf("secretInheritance = %q, want reissue (non-inheritance)", sb.Spec.SecretInheritance)
	}
	if len(sb.Spec.Secrets) != 2 {
		t.Fatalf("secret mounts = %d, want 2", len(sb.Spec.Secrets))
	}
	for _, mt := range sb.Spec.Secrets {
		if mt.SecretRef.Name != "jannes-openclaw-secrets" {
			t.Errorf("mount %s references %q, want jannes-openclaw-secrets", mt.Name, mt.SecretRef.Name)
		}
	}
	if sb.Spec.Expose == nil || sb.Spec.Expose.Port != 18789 || sb.Spec.Expose.Label != "jannes-openclaw" {
		t.Errorf("expose = %+v", sb.Spec.Expose)
	}
	if sb.Spec.Expose.Sharing != "authenticated" {
		t.Errorf("sharing = %q, want authenticated (auth ladder)", sb.Spec.Expose.Sharing)
	}
	if sb.Spec.WorkspaceRef == nil || sb.Spec.WorkspaceRef.Name != "jannes-openclaw-workspace" {
		t.Errorf("workspaceRef = %+v", sb.Spec.WorkspaceRef)
	}
}

// TestProvisionSecretValuesOnlyInSecret asserts secret VALUES live only in the
// Secret, never inlined into the Sandbox spec.
func TestProvisionSecretValuesOnlyInSecret(t *testing.T) {
	m, _ := Parse(mustRead(t, "openclaw.yaml"))
	res, err := Provision(m, map[string]string{"ANTHROPIC_API_KEY": "sk-leak-canary"}, "ns", "inst", "")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	for _, mt := range res.Sandbox.Spec.Secrets {
		// A SecretMount carries only a reference, never the value.
		if strings.Contains(mt.SecretRef.Name, "sk-leak-canary") || mt.EnvVar == "sk-leak-canary" {
			t.Error("secret value leaked into the Sandbox spec")
		}
	}
}

// TestProvisionInjectsPublicURL asserts the resolved per-instance public URL is
// delivered to the fork as the MITOS_PUBLIC_URL env var, and that run.env values
// referencing ${MITOS_PUBLIC_URL} are resolved per-fork into the Sandbox env so a
// fork's exec sessions and runtime reads see this instance's own URL.
func TestProvisionInjectsPublicURL(t *testing.T) {
	m, err := Parse([]byte(`
name: app
source:
  image: ghcr.io/x/y:latest
run:
  env:
    ALLOWED_ORIGINS: ${MITOS_PUBLIC_URL}
    HOME: /home/node
preview:
  port: 8080
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	const url = "https://jannes-app.mitos.run"
	res, err := Provision(m, nil, "ns", "jannes-app", url)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if got := envValue(res.Sandbox.Spec.Env, PublicURLEnvVar); got != url {
		t.Errorf("sandbox env %s = %q, want %q", PublicURLEnvVar, got, url)
	}
	if got := envValue(res.Sandbox.Spec.Env, "ALLOWED_ORIGINS"); got != url {
		t.Errorf("per-fork ALLOWED_ORIGINS = %q, want %q (resolved)", got, url)
	}
	// A non-referencing run.env value is not duplicated into the per-fork env (it
	// stays in the shared golden only).
	if got := envValue(res.Sandbox.Spec.Env, "HOME"); got != "" {
		t.Errorf("HOME should not be overlaid per-fork, got %q", got)
	}
}

// TestProvisionNoURLNoInjection asserts an empty public URL injects nothing, so a
// caller without a resolved URL provisions exactly as before.
func TestProvisionNoURLNoInjection(t *testing.T) {
	m, _ := Parse(mustRead(t, "openclaw.yaml"))
	res, err := Provision(m, map[string]string{"ANTHROPIC_API_KEY": "x"}, "ns", "inst", "")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if got := envValue(res.Sandbox.Spec.Env, PublicURLEnvVar); got != "" {
		t.Errorf("no MITOS_PUBLIC_URL should be injected with an empty url, got %q", got)
	}
}

// TestProvisionRequiredMissing fails closed when a required secret is absent.
func TestProvisionRequiredMissing(t *testing.T) {
	m, _ := Parse(mustRead(t, "openclaw.yaml"))
	_, err := Provision(m, map[string]string{}, "ns", "inst", "")
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("expected required-secret error, got: %v", err)
	}
}

// TestProvisionDeterministicMint stubs entropy to assert minting is wired and the
// minted value lands in the Secret.
func TestProvisionDeterministicMint(t *testing.T) {
	orig := randSource
	t.Cleanup(func() { randSource = orig })
	randSource = func(b []byte) (int, error) {
		for i := range b {
			b[i] = 0xAB
		}
		return len(b), nil
	}
	m, _ := Parse(mustRead(t, "openclaw.yaml"))
	res, err := Provision(m, map[string]string{"ANTHROPIC_API_KEY": "x"}, "ns", "inst", "")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if got := string(res.Secret.Data["OPENCLAW_GATEWAY_TOKEN"]); got != strings.Repeat("ab", 32) {
		t.Errorf("minted token = %q", got)
	}
}
