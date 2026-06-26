package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"mitos.run/mitos/internal/usage"
)

// UsageAPIRunnable is the manager Runnable that serves the controller's INTERNAL
// usage API: a machine-to-machine HTTP endpoint the hosted console reads so it
// can show the SAME per-org usage the collector recorded, without a shared
// database. The collector (UsageCollectorRunnable) writes into Store; this
// runnable serves reads of the same Store, org-scoped and bearer-gated.
//
// It is mounted on its OWN listener (not the controller's metrics or webhook
// mux) so the bearer-gated usage surface is separate from the public
// operational endpoints. It is OFF unless a Token is set: an empty token fails
// closed (the handler refuses every request), and an empty Addr disables the
// listener entirely.
//
// SECURITY: the org is taken from the InternalOrgHeader the console sets (the
// console derived it from the gateway-verified session); the store still scopes
// every read to that org, so a bad header can only ever return that org's data,
// never a cross-org bleed. The bearer token is never logged.
type UsageAPIRunnable struct {
	// Store is the SAME usage store the collector writes into; reads here reflect
	// the collected usage. Required.
	Store usage.UsageStore
	// Prices is the price list used to fill the cost estimate in the response.
	Prices usage.PriceList
	// Addr is the listen address for the internal usage API (for example :8092).
	Addr string
	// Token is the shared bearer secret the console presents. Empty fails closed.
	Token string
}

// Start serves the internal usage API until ctx is canceled. It builds the
// org-scoped, bearer-gated handler over the shared store and listens on Addr.
func (u *UsageAPIRunnable) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("usage-api")
	if u.Addr == "" {
		logger.Info("internal usage API disabled (no address)")
		<-ctx.Done()
		return nil
	}
	prices := u.Prices
	if (prices == usage.PriceList{}) {
		prices = usage.DefaultPriceList()
	}
	mux := http.NewServeMux()
	mux.Handle("GET /internal/usage", usage.NewInternalUsageHandler(u.Store, prices, u.Token))

	srv := &http.Server{
		Addr:              u.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("internal usage API listening", "addr", u.Addr, "tokenConfigured", u.Token != "")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("serve internal usage api: %w", err)
		}
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}
