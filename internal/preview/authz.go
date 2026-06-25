package preview

import (
	"net"
	"strings"
)

// Decision is the result of the authorization pipeline.
type Decision int

const (
	// Allow grants the request.
	Allow Decision = iota
	// DenyUnauthenticated signals that the caller has no identity and must
	// be redirected to the login flow (HTTP 302 / 401).
	DenyUnauthenticated
	// DenyForbidden signals that the caller has an identity (or there is a
	// misconfiguration) but access is not permitted (HTTP 403).
	DenyForbidden
)

// String returns a human-readable label for the decision; useful in logs.
func (d Decision) String() string {
	switch d {
	case Allow:
		return "Allow"
	case DenyUnauthenticated:
		return "DenyUnauthenticated"
	case DenyForbidden:
		return "DenyForbidden"
	default:
		return "Unknown"
	}
}

// Authorize decides whether the request should be allowed, redirected for
// login, or denied outright. It evaluates three layers in order:
//
//  1. NETWORK: if route.Network is non-empty the client IP must fall within at
//     least one of the listed CIDRs. A nil or out-of-range IP is denied.
//     Malformed CIDRs are skipped (fail closed: no match => DenyForbidden).
//
//  2. TIER: the route.Sharing value selects the access tier.
//     "public": always Allow (id is ignored); but if audience fields are set on
//     a public route that is a misconfiguration because audience evaluation
//     requires an identity, so DenyForbidden.
//     "authenticated": id==nil -> DenyUnauthenticated; else continue.
//     "private", "org": id==nil -> DenyUnauthenticated; else route.OrgID must
//     be non-empty and present in id.OrgIDs, else DenyForbidden.
//     "link": link token verification is performed upstream by the proxy before
//     Authorize is called; if the proxy decoded a valid link cookie it sets id.
//     id != nil => continue to audience; id == nil => DenyUnauthenticated.
//     Any other value => DenyForbidden (fail closed).
//
//  3. AUDIENCE (only when id != nil and tier passed):
//     AllowedPrincipals: id.Email must be in the list (case-insensitive email
//     match). IdPs emit one canonical email per identity, so case-folding avoids
//     locking out a legitimate user when the operator typed a different case; it
//     never lets a different mailbox match.
//     AllowedEmailDomains: id.EmailVerified must be true, and emailDomain(id.Email)
//     must exactly match one of the listed domains (case-folded). A suffix such as
//     "evilacme.com" does NOT match an entry "acme.com". Empty allowlist entries
//     are skipped so a stray "" can never match.
func Authorize(route Route, id *Identity, clientIP net.IP) Decision {
	// 1. NETWORK check. The proxy also runs this gate early (before any
	// forwardAuth subrequest or cookie mint); the check here is idempotent so
	// Authorize stays self-contained and correct when called in isolation.
	if !NetworkAllows(route, clientIP) {
		return DenyForbidden
	}

	// 2. TIER check.
	switch route.Sharing {
	case "public":
		// Audience on a public route is a misconfiguration: audience evaluation
		// requires an identity that a public route does not enforce.
		if len(route.AllowedPrincipals) > 0 || len(route.AllowedEmailDomains) > 0 {
			return DenyForbidden
		}
		return Allow

	case "authenticated":
		if id == nil {
			return DenyUnauthenticated
		}

	case "private", "org":
		if id == nil {
			return DenyUnauthenticated
		}
		if route.OrgID == "" || !containsString(id.OrgIDs, route.OrgID) {
			return DenyForbidden
		}

	case "link":
		// Link token validation is the proxy's responsibility, performed before
		// this function is called. The proxy decodes a valid link cookie into id.
		// If id is nil the caller never presented a valid link token.
		if id == nil {
			return DenyUnauthenticated
		}

	default:
		return DenyForbidden
	}

	// 3. AUDIENCE check (id is non-nil here).
	if len(route.AllowedPrincipals) > 0 {
		if !containsEmailFolded(route.AllowedPrincipals, id.Email) {
			return DenyForbidden
		}
	}
	if len(route.AllowedEmailDomains) > 0 {
		if !id.EmailVerified {
			return DenyForbidden
		}
		domain := emailDomain(id.Email)
		if !containsDomainFolded(route.AllowedEmailDomains, domain) {
			return DenyForbidden
		}
	}

	return Allow
}

// emailDomain returns the part of email after the last '@', lowercased.
// Returns an empty string if email contains no '@'.
func emailDomain(email string) string {
	idx := strings.LastIndexByte(email, '@')
	if idx < 0 {
		return ""
	}
	return strings.ToLower(email[idx+1:])
}

// NetworkAllows reports whether clientIP passes route's network layer: it
// returns true when route.Network is empty (no restriction) or when clientIP
// falls within at least one of the listed CIDRs. It fails closed: a nil or
// out-of-range IP against a non-empty allowlist returns false. The proxy calls
// this as an EARLY gate (before the forwardAuth subrequest and before any
// cookie mint) so an out-of-network client is rejected before any outbound
// side effect; Authorize calls it again (idempotent).
func NetworkAllows(route Route, clientIP net.IP) bool {
	if len(route.Network) == 0 {
		return true
	}
	return ipInCIDRs(clientIP, route.Network)
}

// ipInCIDRs reports whether ip is contained in any of the given CIDR strings.
// Malformed CIDRs are skipped. Returns false if ip is nil.
func ipInCIDRs(ip net.IP, cidrs []string) bool {
	if ip == nil {
		return false
	}
	for _, cidr := range cidrs {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			// Malformed CIDR: treat as non-matching (fail closed).
			continue
		}
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// containsString reports whether slice contains s (case-sensitive). Used for
// org-ID membership, which is an opaque identifier compared byte-exact.
func containsString(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

// containsEmailFolded reports whether any entry in emails equals target under
// case folding. Empty configured entries are skipped so a stray "" never
// matches a degenerate identity email.
func containsEmailFolded(emails []string, target string) bool {
	for _, e := range emails {
		if e == "" {
			continue
		}
		if strings.EqualFold(e, target) {
			return true
		}
	}
	return false
}

// containsDomainFolded reports whether any entry in domains equals target under
// case folding. It uses exact equality only; a suffix like "evilacme.com" does
// not match an entry "acme.com". Empty configured entries are skipped so a stray
// "" never matches an email with no '@' (whose emailDomain is "").
func containsDomainFolded(domains []string, target string) bool {
	for _, d := range domains {
		if d == "" {
			continue
		}
		if strings.EqualFold(d, target) {
			return true
		}
	}
	return false
}
