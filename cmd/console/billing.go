package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"mitos.run/mitos/internal/saas/billing"
	"mitos.run/mitos/internal/saas/billingprovider"
	"mitos.run/mitos/internal/saas/billingprovider/stripe"
	"mitos.run/mitos/internal/saas/console"
)

// portalLinker adapts a billingprovider.Provider + an org→customer map into the
// console.PortalLinker seam: it resolves the org's customer ref, then asks the
// provider for that customer's manage-subscription URL. An org with no linked
// customer is console.ErrNotFound (the BFF returns 404).
type portalLinker struct {
	provider  billingprovider.Provider
	customers billingprovider.OrgCustomers
}

func (p portalLinker) PortalURL(ctx context.Context, orgID string) (string, error) {
	cust, ok := p.customers.CustomerForOrg(ctx, orgID)
	if !ok {
		return "", console.ErrNotFound
	}
	return p.provider.PortalURL(ctx, cust)
}

// billingWiring is the result of setupBilling: the portal seam for the BFF and
// the (unauthenticated, signature-verified) webhook handler to mount publicly.
type billingWiring struct {
	portal  console.PortalLinker
	webhook http.Handler
}

// setupBilling builds the billing provider wiring when billing is enabled. The
// provider is currently Stripe (selected like the secret providers); a Merchant
// of Record plugs in here as a sibling. The webhook is fully functional
// (signature-verified status sync); the live portal-session API call is injected
// via the provider's Portal func and is the remaining external adapter, so until
// it is set the portal endpoint simply 404s.
func setupBilling(logger *slog.Logger, status billing.StatusStore) billingWiring {
	if !envBool("MITOS_CONSOLE_BILLING") {
		return billingWiring{portal: nil} // console fills the no-portal default
	}
	provider := stripe.New(stripe.Config{
		SigningSecret: os.Getenv("MITOS_CONSOLE_STRIPE_WEBHOOK_SECRET"),
		Tolerance:     5 * time.Minute,
	})
	customers := billingprovider.NewMemCustomers() // durable mapping is a follow-up
	logger.Info("billing enabled", "provider", provider.Name())
	return billingWiring{
		portal:  portalLinker{provider: provider, customers: customers},
		webhook: billingprovider.NewWebhookHandler(provider, customers, status),
	}
}
