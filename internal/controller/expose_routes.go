package controller

import (
	v1 "mitos.run/mitos/api/v1"
)

// ExposeRoute is the route DTO produced by BuildExposeRoutes for each Ready,
// exposed sandbox. The struct has no json tags so its JSON keys are the bare
// field names, matching the shape the proxy admin endpoint expects. Token
// values are secrets and must never be logged.
type ExposeRoute struct {
	Label        string
	SandboxID    string
	NodeEndpoint string
	Port         int
	Token        string
	Sharing      string
	Ready        bool
}

// BuildExposeRoutes maps a slice of Sandboxes to the route DTO set for the
// Expose proxy admin endpoint. A sandbox is included only when all four
// conditions hold: Status.Phase is SandboxReady, Spec.Expose is non-nil,
// Status.Endpoint is non-empty, and tokenFor returns ok=true. Routes are
// always emitted with Ready=true. An empty Spec.Expose.Sharing defaults to
// "private".
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
		out = append(out, ExposeRoute{
			Label:        sb.Spec.Expose.Label,
			SandboxID:    sb.Status.SandboxID,
			NodeEndpoint: sb.Status.Endpoint,
			Port:         int(sb.Spec.Expose.Port),
			Token:        tok,
			Sharing:      sharing,
			Ready:        true,
		})
	}
	return out
}
