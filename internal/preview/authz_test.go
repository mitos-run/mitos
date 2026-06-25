package preview

import (
	"net"
	"testing"
)

// helpers for building test identities and routes.
func id(sub, email string, verified bool, orgIDs ...string) *Identity {
	return &Identity{Sub: sub, Email: email, EmailVerified: verified, OrgIDs: orgIDs}
}

func route(sharing string, opts ...func(*Route)) Route {
	r := Route{Label: "test", SandboxID: "sb-1", Sharing: sharing}
	for _, o := range opts {
		o(&r)
	}
	return r
}

func withOrg(orgID string) func(*Route) {
	return func(r *Route) { r.OrgID = orgID }
}

func withNetwork(cidrs ...string) func(*Route) {
	return func(r *Route) { r.Network = cidrs }
}

func withPrincipals(emails ...string) func(*Route) {
	return func(r *Route) { r.AllowedPrincipals = emails }
}

func withDomains(domains ...string) func(*Route) {
	return func(r *Route) { r.AllowedEmailDomains = domains }
}

func ip(s string) net.IP { return net.ParseIP(s) }

// TestAuthorize covers the full decision matrix.
func TestAuthorize(t *testing.T) {
	t.Run("public_allow", func(t *testing.T) {
		got := Authorize(route("public"), nil, nil)
		if got != Allow {
			t.Fatalf("want Allow, got %s", got)
		}
	})

	t.Run("public_allow_with_id", func(t *testing.T) {
		got := Authorize(route("public"), id("u1", "a@b.com", true), nil)
		if got != Allow {
			t.Fatalf("want Allow, got %s", got)
		}
	})

	t.Run("public_with_principals_forbidden", func(t *testing.T) {
		// Audience on a public route is a misconfiguration: DenyForbidden.
		got := Authorize(route("public", withPrincipals("a@b.com")), nil, nil)
		if got != DenyForbidden {
			t.Fatalf("want DenyForbidden, got %s", got)
		}
	})

	t.Run("public_with_domains_forbidden", func(t *testing.T) {
		got := Authorize(route("public", withDomains("b.com")), nil, nil)
		if got != DenyForbidden {
			t.Fatalf("want DenyForbidden, got %s", got)
		}
	})

	// authenticated tier.

	t.Run("authenticated_allow_with_id", func(t *testing.T) {
		got := Authorize(route("authenticated"), id("u1", "a@b.com", true), nil)
		if got != Allow {
			t.Fatalf("want Allow, got %s", got)
		}
	})

	t.Run("authenticated_unauthenticated_without_id", func(t *testing.T) {
		got := Authorize(route("authenticated"), nil, nil)
		if got != DenyUnauthenticated {
			t.Fatalf("want DenyUnauthenticated, got %s", got)
		}
	})

	// private / org tier.

	t.Run("private_org_allow_when_org_matches", func(t *testing.T) {
		got := Authorize(
			route("private", withOrg("org-123")),
			id("u1", "a@b.com", true, "org-123", "org-456"),
			nil,
		)
		if got != Allow {
			t.Fatalf("want Allow, got %s", got)
		}
	})

	t.Run("private_org_forbidden_when_org_not_in_id", func(t *testing.T) {
		got := Authorize(
			route("private", withOrg("org-123")),
			id("u1", "a@b.com", true, "org-999"),
			nil,
		)
		if got != DenyForbidden {
			t.Fatalf("want DenyForbidden, got %s", got)
		}
	})

	t.Run("private_unauthenticated_when_no_id", func(t *testing.T) {
		got := Authorize(route("private", withOrg("org-123")), nil, nil)
		if got != DenyUnauthenticated {
			t.Fatalf("want DenyUnauthenticated, got %s", got)
		}
	})

	t.Run("private_no_owner_org_forbidden", func(t *testing.T) {
		// route.OrgID is empty; even with a valid identity, cannot satisfy org check.
		got := Authorize(route("private"), id("u1", "a@b.com", true, "org-1"), nil)
		if got != DenyForbidden {
			t.Fatalf("want DenyForbidden, got %s", got)
		}
	})

	t.Run("org_tier_alias_same_as_private", func(t *testing.T) {
		// "org" is treated identically to "private".
		got := Authorize(
			route("org", withOrg("org-abc")),
			id("u1", "a@b.com", true, "org-abc"),
			nil,
		)
		if got != Allow {
			t.Fatalf("want Allow, got %s", got)
		}
	})

	// link tier.

	t.Run("link_allow_when_id_set", func(t *testing.T) {
		// Link verification is upstream; id already decoded from cookie.
		got := Authorize(route("link"), id("u1", "a@b.com", true), nil)
		if got != Allow {
			t.Fatalf("want Allow, got %s", got)
		}
	})

	t.Run("link_unauthenticated_when_no_id", func(t *testing.T) {
		got := Authorize(route("link"), nil, nil)
		if got != DenyUnauthenticated {
			t.Fatalf("want DenyUnauthenticated, got %s", got)
		}
	})

	// unknown / empty Sharing.

	t.Run("unknown_sharing_forbidden", func(t *testing.T) {
		got := Authorize(route("superspecial"), id("u1", "a@b.com", true), nil)
		if got != DenyForbidden {
			t.Fatalf("want DenyForbidden, got %s", got)
		}
	})

	t.Run("empty_sharing_forbidden", func(t *testing.T) {
		got := Authorize(route(""), id("u1", "a@b.com", true), nil)
		if got != DenyForbidden {
			t.Fatalf("want DenyForbidden, got %s", got)
		}
	})

	// network checks.

	t.Run("network_allow_ip_in_cidr", func(t *testing.T) {
		got := Authorize(
			route("authenticated", withNetwork("10.0.0.0/8")),
			id("u1", "a@b.com", true),
			ip("10.1.2.3"),
		)
		if got != Allow {
			t.Fatalf("want Allow, got %s", got)
		}
	})

	t.Run("network_forbidden_ip_out_of_cidr", func(t *testing.T) {
		got := Authorize(
			route("authenticated", withNetwork("10.0.0.0/8")),
			id("u1", "a@b.com", true),
			ip("192.168.1.1"),
		)
		if got != DenyForbidden {
			t.Fatalf("want DenyForbidden, got %s", got)
		}
	})

	t.Run("network_forbidden_nil_ip", func(t *testing.T) {
		got := Authorize(
			route("authenticated", withNetwork("10.0.0.0/8")),
			id("u1", "a@b.com", true),
			nil,
		)
		if got != DenyForbidden {
			t.Fatalf("want DenyForbidden, got %s", got)
		}
	})

	t.Run("malformed_cidr_fails_closed", func(t *testing.T) {
		// A malformed CIDR is treated as non-matching; since the only entry fails
		// to parse, no CIDR matches, so the result is DenyForbidden.
		got := Authorize(
			route("authenticated", withNetwork("not-a-cidr")),
			id("u1", "a@b.com", true),
			ip("10.0.0.1"),
		)
		if got != DenyForbidden {
			t.Fatalf("want DenyForbidden (fail closed), got %s", got)
		}
	})

	t.Run("network_multiple_cidrs_allow_second", func(t *testing.T) {
		got := Authorize(
			route("authenticated", withNetwork("10.0.0.0/8", "192.168.0.0/16")),
			id("u1", "a@b.com", true),
			ip("192.168.5.5"),
		)
		if got != Allow {
			t.Fatalf("want Allow, got %s", got)
		}
	})

	// AllowedPrincipals audience checks.

	t.Run("principals_allow_email_listed", func(t *testing.T) {
		got := Authorize(
			route("authenticated", withPrincipals("alice@example.com", "bob@example.com")),
			id("u1", "alice@example.com", true),
			nil,
		)
		if got != Allow {
			t.Fatalf("want Allow, got %s", got)
		}
	})

	t.Run("principals_forbidden_email_not_listed", func(t *testing.T) {
		got := Authorize(
			route("authenticated", withPrincipals("alice@example.com")),
			id("u1", "eve@example.com", true),
			nil,
		)
		if got != DenyForbidden {
			t.Fatalf("want DenyForbidden, got %s", got)
		}
	})

	// AllowedEmailDomains audience checks.

	t.Run("email_domains_allow_verified_exact_domain", func(t *testing.T) {
		got := Authorize(
			route("authenticated", withDomains("acme.com")),
			id("u1", "alice@acme.com", true),
			nil,
		)
		if got != Allow {
			t.Fatalf("want Allow, got %s", got)
		}
	})

	t.Run("email_domains_forbidden_unverified", func(t *testing.T) {
		got := Authorize(
			route("authenticated", withDomains("acme.com")),
			id("u1", "alice@acme.com", false),
			nil,
		)
		if got != DenyForbidden {
			t.Fatalf("want DenyForbidden (unverified), got %s", got)
		}
	})

	t.Run("email_domains_forbidden_suffix_trick", func(t *testing.T) {
		// "evilacme.com" must not satisfy an allowlist entry of "acme.com".
		got := Authorize(
			route("authenticated", withDomains("acme.com")),
			id("u1", "user@evilacme.com", true),
			nil,
		)
		if got != DenyForbidden {
			t.Fatalf("want DenyForbidden (suffix trick), got %s", got)
		}
	})

	t.Run("email_domains_forbidden_wrong_domain", func(t *testing.T) {
		got := Authorize(
			route("authenticated", withDomains("acme.com")),
			id("u1", "user@other.com", true),
			nil,
		)
		if got != DenyForbidden {
			t.Fatalf("want DenyForbidden, got %s", got)
		}
	})

	t.Run("email_domains_case_folded", func(t *testing.T) {
		// Domain comparison is case-insensitive.
		got := Authorize(
			route("authenticated", withDomains("ACME.COM")),
			id("u1", "user@acme.com", true),
			nil,
		)
		if got != Allow {
			t.Fatalf("want Allow (case-folded), got %s", got)
		}
	})

	// Combined: principals + domains.

	t.Run("principals_and_domains_both_must_pass", func(t *testing.T) {
		// Email in principals but domain fails verification (unverified).
		got := Authorize(
			route("authenticated",
				withPrincipals("alice@acme.com"),
				withDomains("acme.com"),
			),
			id("u1", "alice@acme.com", false), // not verified
			nil,
		)
		if got != DenyForbidden {
			t.Fatalf("want DenyForbidden, got %s", got)
		}
	})

	// emailDomain helper.

	t.Run("emailDomain_normal", func(t *testing.T) {
		if got := emailDomain("alice@Example.COM"); got != "example.com" {
			t.Fatalf("want example.com, got %q", got)
		}
	})

	t.Run("emailDomain_no_at", func(t *testing.T) {
		if got := emailDomain("noatsign"); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})

	t.Run("emailDomain_last_at", func(t *testing.T) {
		// Multiple '@' signs: use the LAST one.
		if got := emailDomain("a@b@acme.com"); got != "acme.com" {
			t.Fatalf("want acme.com, got %q", got)
		}
	})
}
