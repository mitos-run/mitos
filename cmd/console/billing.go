package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"mitos.run/mitos/internal/saas/billing"
	"mitos.run/mitos/internal/saas/billingprovider"
	"mitos.run/mitos/internal/saas/billingprovider/paddle"
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
// provider is selected by configuration the same way the secret providers are:
// Paddle (our Merchant of Record: the legal seller, which handles global
// sales-tax/VAT) is selected when its API key and webhook secret are present;
// otherwise the Stripe provider remains the default. Both are siblings behind the
// provider-neutral seam, so the dunning/status core never names a provider. The
// webhook is fully functional (signature-verified status sync); the Paddle portal
// link is served by a live Paddle Billing API call, while the Stripe portal call
// stays an injected adapter, so for Stripe the portal endpoint 404s until set.
//
// Secrets: the Paddle API key and webhook secret are read from env and never
// logged; only the selected provider's Name() is logged.
func setupBilling(logger *slog.Logger, status billing.StatusStore) billingWiring {
	if !envBool("MITOS_CONSOLE_BILLING") {
		return billingWiring{portal: nil} // console fills the no-portal default
	}
	var provider billingprovider.Provider
	paddleKey := os.Getenv("MITOS_CONSOLE_PADDLE_API_KEY")
	paddleSecret := os.Getenv("MITOS_CONSOLE_PADDLE_WEBHOOK_SECRET")
	if paddleKey != "" && paddleSecret != "" {
		provider = paddle.New(paddle.Config{
			APIKey:        paddleKey,
			WebhookSecret: paddleSecret,
			BaseURL:       envOr("MITOS_CONSOLE_PADDLE_BASE_URL", paddle.LiveBaseURL),
			Tolerance:     5 * time.Minute,
		})
	} else {
		provider = stripe.New(stripe.Config{
			SigningSecret: os.Getenv("MITOS_CONSOLE_STRIPE_WEBHOOK_SECRET"),
			Tolerance:     5 * time.Minute,
		})
	}
	customers := billingprovider.NewMemCustomers() // durable mapping is a follow-up
	logger.Info("billing enabled", "provider", provider.Name())
	return billingWiring{
		portal:  portalLinker{provider: provider, customers: customers},
		webhook: billingprovider.NewWebhookHandler(provider, customers, status),
	}
}
