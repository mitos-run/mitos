package console

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"mitos.run/mitos/internal/apierr"
	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/billingprovider"
)

// maxTopUpCents is the per-request ceiling for a prepaid credit top-up.
// Amounts above this value are rejected with 400 before reaching the provider.
const maxTopUpCents = 1_000_000

// TopUpLinker starts a prepaid credit checkout and returns the provider-hosted
// URL. The handler builds the TopUp from the injected config (product id and
// currency) and the caller context; the linker resolves the billing customer
// ref for the org and delegates to the underlying provider. A missing customer
// mapping is ErrNotFound so the console returns 404 and hides the affordance.
type TopUpLinker interface {
	CheckoutURL(ctx context.Context, in billingprovider.TopUp) (string, error)
}

// noTopUp is the default TopUpLinker when billing is not configured.
type noTopUp struct{}

func (noTopUp) CheckoutURL(context.Context, billingprovider.TopUp) (string, error) {
	return "", ErrNotFound
}

// handleBillingTopUp returns a Paddle hosted-checkout URL for a prepaid
// credit top-up. The amount is passed as an integer cent value in the query
// string so no floating-point parsing is involved. The product id and currency
// are injected from server-controlled config; a missing product id means
// top-up is not enabled for this installation (400). Provider keys, secrets,
// and the checkout URL are never logged.
func (c *Console) handleBillingTopUp(w http.ResponseWriter, r *http.Request) {
	_, orgID, e, ok := c.authorize(r, saas.PermManageBilling)
	if !ok {
		apierr.Encode(w, e)
		return
	}

	amountStr := r.URL.Query().Get("amount")
	amount, err := strconv.ParseInt(amountStr, 10, 64)
	if err != nil || amount <= 0 || amount > maxTopUpCents {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidInput).
			WithCause("amount must be a positive integer in cents, at most 1000000"))
		return
	}

	if c.deps.TopUpProductID == "" {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidInput).
			WithCause("top-up is not configured for this installation"))
		return
	}

	checkoutURL, err := c.deps.TopUp.CheckoutURL(r.Context(), billingprovider.TopUp{
		OrgID:       orgID,
		AmountCents: amount,
		ProductID:   c.deps.TopUpProductID,
		Currency:    c.deps.TopUpCurrency,
	})
	if err != nil || checkoutURL == "" {
		if errors.Is(err, ErrNotFound) {
			apierr.Encode(w, apierr.Get(apierr.CodeNotFound).
				WithCause("no billing customer is linked to this organization"))
			return
		}
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).
			WithCause("the top-up checkout could not be created"))
		return
	}

	writeJSON(w, http.StatusOK, struct {
		URL string `json:"url"`
	}{URL: checkoutURL})
}
