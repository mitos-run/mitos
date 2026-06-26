package runservice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
)

// AccountLookup maps a verified email to the org the instance is isolated and
// billed under. The production implementation adapts internal/saas AccountService
// (FindOrCreateByEmail then the account's personal org).
type AccountLookup interface {
	OrgForEmail(ctx context.Context, email string) (orgID string, err error)
}

// TenantResolver is the production IdentityResolver. It maps a signed-in request
// to the tenant namespace and a deterministic, globally unique instance label.
type TenantResolver struct {
	// CurrentEmail returns the verified email of the signed-in user, or an error
	// when the request is not authenticated (the funnel then routes to signup).
	CurrentEmail func(r *http.Request) (string, error)
	// Accounts maps an email to its org.
	Accounts AccountLookup
	// NamespaceForOrg maps an org id to its namespace (orgprovision, #410).
	NamespaceForOrg func(orgID string) string
}

// Resolve implements IdentityResolver.
func (t *TenantResolver) Resolve(r *http.Request, src string) (Identity, error) {
	email, err := t.CurrentEmail(r)
	if err != nil {
		return Identity{}, err
	}
	if strings.TrimSpace(email) == "" {
		return Identity{}, fmt.Errorf("not signed in")
	}
	orgID, err := t.Accounts.OrgForEmail(r.Context(), email)
	if err != nil {
		return Identity{}, fmt.Errorf("resolve org: %w", err)
	}
	ns := t.NamespaceForOrg(orgID)
	if ns == "" {
		return Identity{}, fmt.Errorf("no namespace provisioned for org %q", orgID)
	}
	label, err := instanceLabel(src, orgID)
	if err != nil {
		return Identity{}, err
	}
	return Identity{Namespace: ns, InstanceLabel: label}, nil
}

// ContextResolver resolves identity from a verified org already attached to the
// request context by an upstream session middleware (the console/gateway pattern,
// where org and account come from a verified session, never a client header). It
// is the resolver the console mounts.
type ContextResolver struct {
	// OrgFromRequest returns the verified org id for the request, or ok=false when
	// the request is unauthenticated.
	OrgFromRequest func(r *http.Request) (orgID string, ok bool)
	// NamespaceForOrg maps an org id to its provisioned namespace (orgprovision).
	NamespaceForOrg func(orgID string) string
}

// Resolve implements IdentityResolver.
func (c *ContextResolver) Resolve(r *http.Request, src string) (Identity, error) {
	orgID, ok := c.OrgFromRequest(r)
	if !ok || strings.TrimSpace(orgID) == "" {
		return Identity{}, fmt.Errorf("not signed in")
	}
	ns := c.NamespaceForOrg(orgID)
	if ns == "" {
		return Identity{}, fmt.Errorf("no namespace provisioned for org %q", orgID)
	}
	label, err := instanceLabel(src, orgID)
	if err != nil {
		return Identity{}, err
	}
	return Identity{Namespace: ns, InstanceLabel: label}, nil
}

// instanceLabel builds a deterministic, globally unique DNS label: the repo name
// plus a short hash of the org id. Deterministic so a repeat run reconciles the
// same instance; the org-scoped hash means labels never collide across tenants
// (the label is a global subdomain).
func instanceLabel(src, orgID string) (string, error) {
	_, repo, err := splitRepo(src)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(orgID))
	short := hex.EncodeToString(sum[:])[:10]
	label := sanitizeLabel(repo) + "-" + short
	if len(label) > 63 {
		label = label[:63]
	}
	return label, nil
}

// sanitizeLabel lowercases and reduces a string to DNS-label-safe characters.
func sanitizeLabel(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "app"
	}
	return out
}
