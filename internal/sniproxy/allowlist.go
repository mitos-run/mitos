package sniproxy

import (
	"net"

	"mitos.run/mitos/internal/dnsproxy"
)

// Allowlist decides whether a guest may open a TLS connection whose ClientHello
// SNI is serverName to the given TCP port. It is the policy seam the Proxy
// consults after peeking the SNI.
type Allowlist interface {
	// Allowed reports whether the guest at srcIP may reach serverName on port
	// under its per-sandbox domain allowlist. An empty serverName must return
	// false (the missing-SNI deny policy is enforced here as well as in Serve).
	Allowed(srcIP net.IP, serverName string, port int) bool
}

// RegistryAllowlist adapts a *dnsproxy.Registry to the Allowlist interface so the
// SNI proxy enforces the EXACT SAME per-sandbox domain allowlist the controlled
// DNS resolver does (issue #47): the same exact-or-anchored-wildcard name matcher
// and the same per-name port set, attributed by the guest source IP. There is no
// second matcher: the SNI decision is dnsproxy.Registry.Lookup. A name on the
// allowlist for the connection's port is allowed; everything else (unlisted name,
// wrong port, unregistered guest, empty SNI) is denied, fail closed.
type RegistryAllowlist struct {
	Registry *dnsproxy.Registry
}

// Allowed delegates to dnsproxy.Registry.Lookup, which canonicalizes the name
// (lowercase, trailing-dot tolerant) and applies the exact-or-anchored-wildcard
// match. It then requires the connection's port to be in the matched name's
// allowed port set, so an HTTPS-only name (port 443) does not authorize a
// connection to a different port. A nil registry or an empty serverName denies.
func (a RegistryAllowlist) Allowed(srcIP net.IP, serverName string, port int) bool {
	if a.Registry == nil || serverName == "" {
		return false
	}
	ports, ok := a.Registry.Lookup(srcIP, serverName)
	if !ok {
		return false
	}
	for _, p := range ports {
		if p == port {
			return true
		}
	}
	return false
}
