package controller

import (
	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/tenant"
)

// ExposeRoute is the route DTO produced by BuildExposeRoutes for each Ready,
// exposed sandbox. The struct has no json tags so its JSON keys are the bare
// field names, matching the shape the proxy admin endpoint expects. Token
// values are secrets and must never be logged.
// IMPORTANT: this struct and preview.ClaimState must stay byte-identical (same
// field names, same order, no json tags). Add fields to both in lockstep.
type ExposeRoute struct {
	Label               string
	SandboxID           string
	NodeEndpoint        string
	Port                int
	Token               string
	Sharing             string
	OrgID               string
	Network             []string
	ForwardAuthURL      string
	AllowedPrincipals   []string
	AllowedEmailDomains []string
	Ready               bool
}

// BuildExposeRoutes maps a slice of Sandboxes to the route DTO set for the
// Expose proxy admin endpoint. A sandbox is included only when all four
// conditions hold: Status.Phase is SandboxReady, Spec.Expose is non-nil,
// Status.Endpoint is non-empty, and tokenFor returns ok=true. Routes are
// always emitted with Ready=true. An empty Spec.Expose.Sharing defaults to
// "private". OrgID is derived from tenant.OrgFromNamespace; it is empty for
// non-org namespaces (self-host). The four policy fields (Network,
// ForwardAuthURL, AllowedPrincipals, AllowedEmailDomains) are copied verbatim
// from Spec.Expose.
func BuildExposeRoutes(sandboxes []v1.Sandbox, tokenFor func(sb v1.Sandbox) (string, bool)) []ExposeRoute {
	var out []ExposeRoute
	for _, sb := range sandboxes {
		if sb.Status.Phase != v1.SandboxReady {
			continue
		}
		if sb.Spec.Expose == nil {
			continue
		}
		if sb.Status.Endpoint == "" {
			continue
		}
		tok, ok := tokenFor(sb)
		if !ok {
			continue
		}
		sharing := sb.Spec.Expose.Sharing
		if sharing == "" {
			sharing = "private"
		}
		orgID, _ := tenant.OrgFromNamespace(sb.Namespace)
		out = append(out, ExposeRoute{
			Label:               sb.Spec.Expose.Label,
			SandboxID:           sb.Status.SandboxID,
			NodeEndpoint:        sb.Status.Endpoint,
			Port:                int(sb.Spec.Expose.Port),
			Token:               tok,
			Sharing:             sharing,
			OrgID:               orgID,
			Network:             sb.Spec.Expose.Network,
			ForwardAuthURL:      sb.Spec.Expose.ForwardAuthURL,
			AllowedPrincipals:   sb.Spec.Expose.AllowedPrincipals,
			AllowedEmailDomains: sb.Spec.Expose.AllowedEmailDomains,
			Ready:               true,
		})
	}
	return out
}
