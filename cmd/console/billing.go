package main

import (
	"context"
	"fmt"
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

// portalLinker adapts a billingprovider.Provider + an org-customer map into the
// console.PortalLinker seam: it resolves the org's customer ref, then asks the
// provider for that customer's manage-subscription URL. An org with no linked
// customer is console.ErrNotFound (the BFF returns 404).
type portalLinker struct {
	provider  billingprovider.Provider
	customers billingprovider.OrgCustomers
}

func (p portalLinker) PortalURL(ctx context.Context, orgID string) (string, error) {
	cust, ok, err := p.customers.CustomerForOrg(ctx, orgID)
	if err != nil {
		return "", fmt.Errorf("resolve billing customer: %w", err)
	}
	if !ok {
		return "", console.ErrNotFound
	}
	return p.provider.PortalURL(ctx, cust)
}

// checkoutURLer is the narrow seam for providers that support hosted checkout
// for prepaid credit top-ups. Only the Paddle provider implements this; the
// Stripe adapter does not, so top-up is unavailable when Stripe is selected.
type checkoutURLer interface {
	CheckoutURL(ctx context.Context, in billingprovider.TopUp) (string, error)
}

// topUpLinker adapts a checkoutURLer + an org-customer map into the
// console.TopUpLinker seam. It resolves the org's billing customer ref before
// forwarding to the provider. An org with no linked customer is
// console.ErrNotFound (the BFF returns 404 and hides the affordance).
// Provider keys and the returned URL are never logged; only org ids and counts.
type topUpLinker struct {
	provider  checkoutURLer
	customers billingprovider.OrgCustomers
}

func (l topUpLinker) CheckoutURL(ctx context.Context, in billingprovider.TopUp) (string, error) {
	cust, ok, err := l.customers.CustomerForOrg(ctx, in.OrgID)
	if err != nil {
		return "", fmt.Errorf("resolve billing customer: %w", err)
	}
	if !ok {
		return "", console.ErrNotFound
	}
	in.CustomerRef = cust
	return l.provider.CheckoutURL(ctx, in)
}

// billingWiring is the result of setupBilling: the portal seam and the top-up
// seam for the BFF, and the (unauthenticated, signature-verified) webhook
// handler to mount publicly.
type billingWiring struct {
	portal         console.PortalLinker
	webhook        http.Handler
	topUp          console.TopUpLinker
	topUpProductID string
	topUpCurrency  string
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
// Top-up checkout is Paddle-only: the top-up product id and currency are read
// from MITOS_CONSOLE_PADDLE_TOPUP_PRODUCT and MITOS_CONSOLE_PADDLE_CURRENCY
// (default EUR) only when the Paddle provider is active. An empty product id
// disables the affordance (the endpoint returns 400).
//
// Secrets: the Paddle API key and webhook secret are read from env and never
// logged; only the selected provider's Name() is logged.
//
// customers is the org to billing-customer map shared by the webhook (customer
// to org), the portal link, and top-up checkout (org to customer). The caller
// selects it the same way it selects the credit ledger: durable
// pgstore.PgCustomers when Postgres is configured, in-memory otherwise (DEV
// ONLY: an in-memory map cannot survive a restart, so a webhook arriving after
// a redeploy would drop its status sync).
func setupBilling(logger *slog.Logger, status billing.StatusStore, creditLedger billing.CreditLedger, customers billingprovider.Customers) billingWiring {
	if !envBool("MITOS_CONSOLE_BILLING") {
		return billingWiring{portal: nil} // console fills the no-portal default
	}
	var provider billingprovider.Provider
	paddleKey := os.Getenv("MITOS_CONSOLE_PADDLE_API_KEY")
	paddleSecret := os.Getenv("MITOS_CONSOLE_PADDLE_WEBHOOK_SECRET")

	var tu console.TopUpLinker
	var topUpProductID, topUpCurrency string

	if paddleKey != "" && paddleSecret != "" {
		pp := paddle.New(paddle.Config{
			APIKey:        paddleKey,
			WebhookSecret: paddleSecret,
			BaseURL:       envOr("MITOS_CONSOLE_PADDLE_BASE_URL", paddle.LiveBaseURL),
			Tolerance:     5 * time.Minute,
		})
		provider = pp
		topUpProductID = os.Getenv("MITOS_CONSOLE_PADDLE_TOPUP_PRODUCT")
		topUpCurrency = envOr("MITOS_CONSOLE_PADDLE_CURRENCY", "EUR")
		tu = topUpLinker{provider: pp, customers: customers}
	} else {
		provider = stripe.New(stripe.Config{
			SigningSecret: os.Getenv("MITOS_CONSOLE_STRIPE_WEBHOOK_SECRET"),
			Tolerance:     5 * time.Minute,
		})
	}

	logger.Info("billing enabled", "provider", provider.Name())
	return billingWiring{
		portal:         portalLinker{provider: provider, customers: customers},
		webhook:        billingprovider.NewWebhookHandler(provider, customers, status, creditLedger, time.Now),
		topUp:          tu,
		topUpProductID: topUpProductID,
		topUpCurrency:  topUpCurrency,
	}
}
