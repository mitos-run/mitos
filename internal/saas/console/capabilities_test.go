package console

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// getCaps issues an UNAUTHENTICATED GET /console/capabilities. Capabilities are
// deployment-level config (edition + feature flags), not org data, so the SPA
// must be able to read them at boot before login (e.g. to decide whether to
// render a signup affordance). The handler therefore must not require a caller
// context.
func getCaps(t *testing.T, c *Console) (*httptest.ResponseRecorder, Capabilities) {
	t.Helper()
	r := httptest.NewRequest("GET", "/console/capabilities", nil)
	w := httptest.NewRecorder()
	c.ServeHTTP(w, r)
	var caps Capabilities
	if w.Code == http.StatusOK {
		decode(t, w, &caps)
	}
	return w, caps
}

// TestCapabilitiesDefaultsToCommunity asserts that a Console built without an
// explicit Capabilities config advertises the self-hosted community edition:
// billing/signup/orgSwitcher off, teams on, oidc, the kube secret provider, and
// the proof surface available. This is the "100% same code, server decides"
// keystone: self-host is the default edition.
func TestCapabilitiesDefaultsToCommunity(t *testing.T) {
	c := New(Deps{})
	w, caps := getCaps(t, c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if caps.Edition != "community" {
		t.Errorf("edition = %q, want community", caps.Edition)
	}
	if caps.Billing {
		t.Error("billing should be off in community edition")
	}
	if caps.Signup {
		t.Error("signup should be off by default (gated, waitlist mode)")
	}
	if !caps.Teams {
		t.Error("teams should be on in both editions")
	}
	if caps.OrgSwitcher {
		t.Error("orgSwitcher should be off in single-org self-host default")
	}
	if caps.IDP != "oidc" {
		t.Errorf("idp = %q, want oidc", caps.IDP)
	}
	if caps.Ownership != "self-hosted" {
		t.Errorf("ownership = %q, want self-hosted", caps.Ownership)
	}
	if !caps.Proof {
		t.Error("proof surface should be available by default")
	}
	if len(caps.Secrets.Providers) != 1 || caps.Secrets.Providers[0] != "kube" {
		t.Errorf("secrets.providers = %v, want [kube]", caps.Secrets.Providers)
	}
}

// TestCapabilitiesHostedEditionVerbatim asserts that an explicit hosted
// Capabilities config is returned verbatim: the SaaS turns billing and signup
// on, switches ownership to hosted, and adds the openbao provider. No edition
// is inferred from a build flag; the server config is the single source.
func TestCapabilitiesHostedEditionVerbatim(t *testing.T) {
	want := Capabilities{
		Edition:     "hosted",
		Billing:     true,
		Signup:      true,
		Teams:       true,
		IDP:         "oidc",
		OrgSwitcher: true,
		Secrets:     SecretsCapability{Providers: []string{"kube", "openbao"}},
		Proof:       true,
		Ownership:   "hosted",
	}
	c := New(Deps{Capabilities: want})
	w, caps := getCaps(t, c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if caps.Edition != "hosted" || !caps.Billing || !caps.Signup || !caps.OrgSwitcher {
		t.Errorf("hosted flags not echoed: %+v", caps)
	}
	if caps.Ownership != "hosted" {
		t.Errorf("ownership = %q, want hosted", caps.Ownership)
	}
	if len(caps.Secrets.Providers) != 2 {
		t.Errorf("secrets.providers = %v, want [kube openbao]", caps.Secrets.Providers)
	}
}
