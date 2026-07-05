package console

import (
	"net/http"

	"mitos.run/mitos/internal/saas/billing"
)

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
	// AuthConnectors is the sorted, deduplicated list of social-login providers
	// that are actually configured (e.g. ["github"] or ["github","google"]).
	// The SPA renders a social-login button ONLY for each entry. An empty list
	// means no social buttons; the email magic-link form is always available.
	// The value is set server-side from MITOS_CONSOLE_AUTH_CONNECTORS; the SPA
	// cannot override it. Only "github" and "google" are recognised; unknown
	// values are silently dropped.
	AuthConnectors []string `json:"authConnectors"`
	// Plan is the caller's org's current billing plan ("free" or "team" on a
	// hosted deployment). On the self-hosted community edition this value is
	// informational only: the engine is never gated by it (see Entitlements).
	// handleCapabilities resolves this per request from Deps.Plans when the
	// request carries an org context; otherwise it is the deployment default.
	Plan billing.Plan `json:"plan"`
	// Entitlements is the resolved set of plan-gated hosted conveniences for
	// Plan on this deployment's Edition (billing.EntitlementsFor). The
	// self-hosted community edition always resolves to every entitlement
	// enabled with unlimited audit retention.
	Entitlements billing.Entitlements `json:"entitlements"`
	// Admin is true when the CALLER holds the instance-operator capability
	// (see admin.go's isInstanceAdmin): it gates the SPA's "Operate" nav
	// group and /admin/* routes. Like Plan/Entitlements it is resolved
	// per-request from the caller's context, never a deployment-wide
	// default; an unauthenticated request always sees false.
	Admin bool `json:"admin"`
	// Feedback advertises the one-click feedback channel the SPA's
	// FeedbackButton composes into: email (the hosted default, opened as a
	// mailto:) or a GitHub new-issue link (the community default). There is
	// no server-persisted feedback store in v1; this is deployment-level
	// config, not resolved per-request. See FeedbackCapability.
	Feedback FeedbackCapability `json:"feedback"`
	// Version is the console binary's build version (wired from -ldflags at
	// release time where available), or "dev" for a local/unreleased build.
	// The SPA renders it in the sidebar footer and includes it in feedback
	// diagnostics so a report always carries what build produced it.
	Version string `json:"version"`
}

// FeedbackCapability tells the SPA where composed feedback goes. There is NO
// server write path for feedback in v1: the SPA hands the message + attached
// diagnostics straight to the OS mail client (channel "email") or opens a
// browser tab at a prefilled GitHub new-issue URL (channel "github"). Nothing
// here is ever a secret, so it is safe to serve from the unauthenticated
// capabilities endpoint.
type FeedbackCapability struct {
	// Channel is "email" or "github".
	Channel string `json:"channel"`
	// Target is a mailto address when Channel is "email", or an "owner/repo"
	// GitHub slug when Channel is "github".
	Target string `json:"target"`
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
		Edition:        "community",
		Billing:        false,
		Signup:         false,
		Teams:          true,
		IDP:            "oidc",
		OrgSwitcher:    false,
		Secrets:        SecretsCapability{Providers: []string{"kube"}},
		Proof:          true,
		Ownership:      "self-hosted",
		AuthConnectors: []string{},
		Plan:           billing.PlanFree,
		Entitlements:   billing.EntitlementsFor(billing.PlanFree, "community"),
		Feedback:       FeedbackCapability{Channel: "github", Target: "mitos-run/mitos"},
		Version:        "dev",
	}
}

// handleCapabilities serves the deployment capabilities document. It is
// intentionally reachable unauthenticated: it carries no org data and the SPA
// needs it before a session exists, so an unauthenticated caller gets the
// deployment-default Plan/Entitlements baked in at boot (New).
//
// When the request DOES carry an org context (the normal authenticated path;
// the console mux is wrapped in session middleware in production), Plan and
// Entitlements are re-resolved for that org via Deps.Plans so a caller always
// sees ITS org's real plan, never the boot-time default. A plan-lookup
// failure leaves the boot-time default in place rather than failing the
// request: capabilities must always be servable.
func (c *Console) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	caps := c.deps.Capabilities
	// Admin is explicitly reset here (rather than left at whatever
	// deps.Capabilities.Admin happened to be) so the "an unauthenticated
	// request always sees false" guarantee documented on the Admin field is
	// enforced by this function itself, not by the convention that no code
	// path currently sets that field on the boot-time default.
	caps.Admin = false
	if orgID, ok := OrgFromContext(r.Context()); ok && c.deps.Plans != nil {
		if plan, err := c.deps.Plans.GetPlan(r.Context(), orgID); err == nil {
			caps.Plan = plan
			caps.Entitlements = billing.EntitlementsFor(plan, caps.Edition)
		} else if c.deps.Log != nil {
			c.deps.Log.Warn("capabilities: plan lookup failed, serving boot-time default", "org", orgID, "err", err.Error())
		}
	}
	if accountID, ok := CallerFromContext(r.Context()); ok {
		caps.Admin = c.isInstanceAdmin(r.Context(), accountID)
	}
	writeJSON(w, http.StatusOK, caps)
}
