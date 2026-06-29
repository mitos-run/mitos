package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"mitos.run/mitos/internal/saas/console"
)

// TestCapabilitiesFromEnvDefaultsToCommunity asserts that, with no env set, the
// binary advertises the self-hosted community edition with the #208 gate held
// (signup + billing off) and the kube provider.
func TestCapabilitiesFromEnvDefaultsToCommunity(t *testing.T) {
	for _, k := range []string{"MITOS_CONSOLE_EDITION", "MITOS_CONSOLE_BILLING", "MITOS_CONSOLE_SIGNUP", "MITOS_CONSOLE_SECRET_PROVIDERS"} {
		t.Setenv(k, "")
	}
	c := capabilitiesFromEnv()
	if c.Edition != "community" || c.Billing || c.Signup || c.Ownership != "self-hosted" {
		t.Fatalf("community defaults wrong: %+v", c)
	}
	if len(c.Secrets.Providers) != 1 || c.Secrets.Providers[0] != "kube" {
		t.Fatalf("providers = %v, want [kube]", c.Secrets.Providers)
	}
}

// TestCapabilitiesFromEnvHosted asserts the hosted env flips the
// server-controlled flags and parses a multi-provider list.
func TestCapabilitiesFromEnvHosted(t *testing.T) {
	t.Setenv("MITOS_CONSOLE_EDITION", "hosted")
	t.Setenv("MITOS_CONSOLE_BILLING", "true")
	t.Setenv("MITOS_CONSOLE_SIGNUP", "1")
	t.Setenv("MITOS_CONSOLE_SECRET_PROVIDERS", "kube, openbao")
	c := capabilitiesFromEnv()
	if c.Edition != "hosted" || !c.Billing || !c.Signup || !c.OrgSwitcher || c.Ownership != "hosted" {
		t.Fatalf("hosted flags wrong: %+v", c)
	}
	if len(c.Secrets.Providers) != 2 || c.Secrets.Providers[1] != "openbao" {
		t.Fatalf("providers = %v, want [kube openbao]", c.Secrets.Providers)
	}
}

// TestCapabilitiesFromEnvConnectors asserts MITOS_CONSOLE_AUTH_CONNECTORS
// controls which social-login buttons the SPA renders. Only "github" and
// "google" are known; unknown values are silently dropped; empty yields none.
func TestCapabilitiesFromEnvConnectors(t *testing.T) {
	clearBase := func(t *testing.T) {
		t.Helper()
		for _, k := range []string{"MITOS_CONSOLE_EDITION", "MITOS_CONSOLE_BILLING", "MITOS_CONSOLE_SIGNUP", "MITOS_CONSOLE_SECRET_PROVIDERS", "MITOS_CONSOLE_AUTH_CONNECTORS"} {
			t.Setenv(k, "")
		}
	}

	t.Run("github only", func(t *testing.T) {
		clearBase(t)
		t.Setenv("MITOS_CONSOLE_AUTH_CONNECTORS", "github")
		c := capabilitiesFromEnv()
		if len(c.AuthConnectors) != 1 || c.AuthConnectors[0] != "github" {
			t.Fatalf("authConnectors = %v, want [github]", c.AuthConnectors)
		}
	})

	t.Run("both providers sorted", func(t *testing.T) {
		clearBase(t)
		t.Setenv("MITOS_CONSOLE_AUTH_CONNECTORS", "google,github")
		c := capabilitiesFromEnv()
		if len(c.AuthConnectors) != 2 || c.AuthConnectors[0] != "github" || c.AuthConnectors[1] != "google" {
			t.Fatalf("authConnectors = %v, want [github google]", c.AuthConnectors)
		}
	})

	t.Run("unknown values dropped", func(t *testing.T) {
		clearBase(t)
		t.Setenv("MITOS_CONSOLE_AUTH_CONNECTORS", "github,gitlab,okta")
		c := capabilitiesFromEnv()
		if len(c.AuthConnectors) != 1 || c.AuthConnectors[0] != "github" {
			t.Fatalf("authConnectors = %v, want [github] (unknown dropped)", c.AuthConnectors)
		}
	})

	t.Run("empty yields no social buttons", func(t *testing.T) {
		clearBase(t)
		// MITOS_CONSOLE_AUTH_CONNECTORS is already empty from clearBase.
		c := capabilitiesFromEnv()
		if len(c.AuthConnectors) != 0 {
			t.Fatalf("authConnectors = %v, want []", c.AuthConnectors)
		}
	})

	t.Run("duplicates deduplicated", func(t *testing.T) {
		clearBase(t)
		t.Setenv("MITOS_CONSOLE_AUTH_CONNECTORS", "github,github,google")
		c := capabilitiesFromEnv()
		if len(c.AuthConnectors) != 2 || c.AuthConnectors[0] != "github" || c.AuthConnectors[1] != "google" {
			t.Fatalf("authConnectors = %v, want [github google]", c.AuthConnectors)
		}
	})
}

// TestAuthConnectorsEndpoint asserts the public GET /auth/connectors handler
// returns the correct JSON payload without requiring a session. This endpoint
// is the pre-auth data source the SPA Login/Signup pages consume to render
// only the social-login buttons for configured providers.
func TestAuthConnectorsEndpoint(t *testing.T) {
	type resp struct {
		Connectors []string `json:"connectors"`
	}

	hit := func(t *testing.T, caps console.Capabilities) resp {
		t.Helper()
		h := newAuthConnectorsHandler(caps)
		r := httptest.NewRequest(http.MethodGet, "/auth/connectors", nil)
		w := httptest.NewRecorder()
		h(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d", w.Code)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		var got resp
		if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return got
	}

	t.Run("github only", func(t *testing.T) {
		got := hit(t, console.Capabilities{AuthConnectors: []string{"github"}})
		if len(got.Connectors) != 1 || got.Connectors[0] != "github" {
			t.Errorf("connectors = %v, want [github]", got.Connectors)
		}
	})

	t.Run("github and google", func(t *testing.T) {
		got := hit(t, console.Capabilities{AuthConnectors: []string{"github", "google"}})
		if len(got.Connectors) != 2 {
			t.Fatalf("connectors = %v, want [github google]", got.Connectors)
		}
	})

	t.Run("empty returns array not null", func(t *testing.T) {
		got := hit(t, console.Capabilities{AuthConnectors: []string{}})
		if got.Connectors == nil {
			t.Error("connectors must be [] not null")
		}
		if len(got.Connectors) != 0 {
			t.Errorf("connectors = %v, want []", got.Connectors)
		}
	})
}
