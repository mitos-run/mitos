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

// TestCapabilitiesAuthConnectorsInResponse asserts that the authConnectors
// field is serialised into the capabilities response and reflects the value
// set in Deps.Capabilities. An empty slice serialises as [] (not null) so the
// SPA can safely iterate without a nil-guard.
func TestCapabilitiesAuthConnectorsInResponse(t *testing.T) {
	t.Run("github only", func(t *testing.T) {
		c := New(Deps{Capabilities: Capabilities{
			Edition:        "community",
			Teams:          true,
			IDP:            "oidc",
			Proof:          true,
			Ownership:      "self-hosted",
			Secrets:        SecretsCapability{Providers: []string{"kube"}},
			AuthConnectors: []string{"github"},
		}})
		_, caps := getCaps(t, c)
		if len(caps.AuthConnectors) != 1 || caps.AuthConnectors[0] != "github" {
			t.Errorf("authConnectors = %v, want [github]", caps.AuthConnectors)
		}
	})

	t.Run("github and google", func(t *testing.T) {
		c := New(Deps{Capabilities: Capabilities{
			Edition:        "hosted",
			Teams:          true,
			IDP:            "oidc",
			Proof:          true,
			Ownership:      "hosted",
			Secrets:        SecretsCapability{Providers: []string{"kube"}},
			AuthConnectors: []string{"github", "google"},
		}})
		_, caps := getCaps(t, c)
		if len(caps.AuthConnectors) != 2 {
			t.Fatalf("authConnectors = %v, want [github google]", caps.AuthConnectors)
		}
		if caps.AuthConnectors[0] != "github" || caps.AuthConnectors[1] != "google" {
			t.Errorf("authConnectors order wrong: %v", caps.AuthConnectors)
		}
	})

	t.Run("empty serialises as array not null", func(t *testing.T) {
		c := New(Deps{}) // defaultCapabilities() sets AuthConnectors: []string{}
		_, caps := getCaps(t, c)
		if caps.AuthConnectors == nil {
			t.Error("authConnectors must be [] not null when no connectors configured")
		}
		if len(caps.AuthConnectors) != 0 {
			t.Errorf("authConnectors = %v, want []", caps.AuthConnectors)
		}
	})
}

// TestCapabilitiesDefaultsIncludeFeedbackAndVersion asserts the community
// default carries a usable feedback channel (github, so a self-hoster with no
// support inbox still has a one-click path to file an issue) and a version
// string (defaulting "dev" until a build injects one), so the SPA's feedback
// dialog and sidebar footer never render empty.
func TestCapabilitiesDefaultsIncludeFeedbackAndVersion(t *testing.T) {
	c := New(Deps{})
	_, caps := getCaps(t, c)
	if caps.Feedback.Channel != "github" {
		t.Errorf("feedback.channel = %q, want github", caps.Feedback.Channel)
	}
	if caps.Feedback.Target != "mitos-run/mitos" {
		t.Errorf("feedback.target = %q, want mitos-run/mitos", caps.Feedback.Target)
	}
	if caps.Version != "dev" {
		t.Errorf("version = %q, want dev", caps.Version)
	}
}

// TestCapabilitiesFeedbackAndVersionEchoedVerbatim asserts an explicit
// Capabilities config (as cmd/console's capabilitiesFromEnv builds) is
// returned unchanged: Feedback and Version are deployment-level config, not
// resolved per-request like Plan/Entitlements/Admin.
func TestCapabilitiesFeedbackAndVersionEchoedVerbatim(t *testing.T) {
	want := Capabilities{
		Edition:   "hosted",
		Teams:     true,
		IDP:       "oidc",
		Proof:     true,
		Ownership: "hosted",
		Secrets:   SecretsCapability{Providers: []string{"kube"}},
		Feedback:  FeedbackCapability{Channel: "email", Target: "feedback@mitos.run"},
		Version:   "1.6.0",
	}
	c := New(Deps{Capabilities: want})
	_, caps := getCaps(t, c)
	if caps.Feedback.Channel != "email" || caps.Feedback.Target != "feedback@mitos.run" {
		t.Errorf("feedback = %+v, want email/feedback@mitos.run", caps.Feedback)
	}
	if caps.Version != "1.6.0" {
		t.Errorf("version = %q, want 1.6.0", caps.Version)
	}
}
