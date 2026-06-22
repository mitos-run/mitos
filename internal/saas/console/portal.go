package console

import (
	"context"
	"errors"
	"net/http"

	"mitos.run/mitos/internal/apierr"
)

// PortalLinker returns the provider-hosted "manage subscription" URL for an org
// (the Stripe Customer Portal, or a Merchant-of-Record equivalent). It is the
// provider-NEUTRAL seam the console deep-links to instead of rebuilding payment
// UI; the real implementation resolves the org's customer ref and calls the
// configured billingprovider. A community/self-host install has none, so the
// default returns ErrNotFound and the console hides the affordance.
type PortalLinker interface {
	PortalURL(ctx context.Context, orgID string) (string, error)
}

// noPortal is the default PortalLinker: no billing portal is configured.
type noPortal struct{}

func (noPortal) PortalURL(context.Context, string) (string, error) { return "", ErrNotFound }

// handleBillingPortal returns the caller org's manage-subscription URL. When no
// portal is configured (community edition, or no customer yet) it is a 404, so
// the UI never shows a fabricated link.
func (c *Console) handleBillingPortal(w http.ResponseWriter, r *http.Request) {
	_, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	url, err := c.deps.Portal.PortalURL(r.Context(), orgID)
	if err != nil || url == "" {
		if err != nil && !errors.Is(err, ErrNotFound) {
			apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the billing portal link could not be created"))
			return
		}
		apierr.Encode(w, apierr.Get(apierr.CodeNotFound).WithCause("no billing portal is available for this organization"))
		return
	}
	writeJSON(w, http.StatusOK, struct {
		URL string `json:"url"`
	}{URL: url})
}
