package console

import "net/http"

// Capabilities is the deployment-level configuration the console advertises at
// GET /console/capabilities. It is the keystone of the single-artifact design:
// the hosted SaaS and any self-hosted install run the identical binary and SPA
// bundle, and differ ONLY by this server-advertised document. There is no
// build-time edition fork.
//
// It is also the enforcement mechanism for the #208 hard gate: Signup and
// Billing are server-controlled, so no client can flip on self-serve onboarding
// or billing. Until the production gates pass, Signup stays false (waitlist
// mode).
//
// Capabilities carries NO org data, so the handler is unauthenticated: the SPA
// reads it at boot, before login, to decide which routes to mount.
type Capabilities struct {
	// Edition is "community" (self-hosted default) or "hosted" (our SaaS).
	Edition string `json:"edition"`
	// Billing gates the Stripe billing/subscription surface (hosted only).
	Billing bool `json:"billing"`
	// Signup gates self-serve org creation. Default false (gated, waitlist).
	Signup bool `json:"signup"`
	// Teams gates members + roles. On in both editions.
	Teams bool `json:"teams"`
	// IDP names the session identity source ("oidc").
	IDP string `json:"idp"`
	// OrgSwitcher is true when the install exposes more than one org.
	OrgSwitcher bool `json:"orgSwitcher"`
	// Secrets advertises the configured secret providers.
	Secrets SecretsCapability `json:"secrets"`
	// Proof gates the Pareto proof surface (instrument panel, fork tree).
	Proof bool `json:"proof"`
	// Ownership is "self-hosted" or "hosted"; drives the chrome badge.
	Ownership string `json:"ownership"`
}

// SecretsCapability advertises which secret-store providers are enabled. The
// registry is the seam; editions differ only in which providers are configured.
type SecretsCapability struct {
	Providers []string `json:"providers"`
}

// defaultCapabilities is the self-hosted community edition: one org, OIDC, the
// kube secret provider, the proof surface on, and billing/signup/orgSwitcher
// off. It is applied when a Console is built without an explicit Capabilities.
func defaultCapabilities() Capabilities {
	return Capabilities{
		Edition:     "community",
		Billing:     false,
		Signup:      false,
		Teams:       true,
		IDP:         "oidc",
		OrgSwitcher: false,
		Secrets:     SecretsCapability{Providers: []string{"kube"}},
		Proof:       true,
		Ownership:   "self-hosted",
	}
}

// handleCapabilities serves the deployment capabilities document. It is
// intentionally unauthenticated: it carries no org data and the SPA needs it
// before a session exists.
func (c *Console) handleCapabilities(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, c.deps.Capabilities)
}
