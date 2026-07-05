package main

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"time"

	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/onboarding"
)

// internalDeps are the machine-to-machine handlers that must NOT be reachable
// from the public internet. They are bearer-gated, but the bearer is the only
// gate, so serving them on the public console listener made them internet
// reachable (identity/resolve even provisions accounts). They belong on a
// dedicated cluster-internal listener, exactly like /metrics (GHSA-rcf5-cfv3-jxvv).
type internalDeps struct {
	accounts     *saas.AccountService
	sessions     *saas.SessionService
	allowlist    onboarding.Allowlist
	emailSender  onboarding.EmailSender
	resolveToken string
	approveToken string
	logger       *slog.Logger
}

// newInternalMux builds the mux for the bearer-gated M2M endpoints. Each route
// is mounted only when its token is configured, mirroring the previous public
// mounting logic but on a mux that is served ONLY on the internal listener.
func newInternalMux(d internalDeps) *http.ServeMux {
	mux := http.NewServeMux()
	if d.resolveToken != "" {
		mux.Handle("POST /internal/identity/resolve", saas.NewIdentityResolveHandler(d.accounts, d.resolveToken, d.logger))
		mux.Handle("POST /internal/session/resolve", saas.NewSessionResolveHandler(d.sessions, d.resolveToken, d.logger))
		d.logger.Info("identity and session resolve endpoints mounted on the internal listener")
	} else {
		d.logger.Warn("MITOS_IDENTITY_RESOLVE_TOKEN unset; identity/session resolve endpoints not mounted")
	}
	if d.approveToken != "" {
		mux.Handle("POST /internal/approve-signup", onboarding.NewApproveSignupHandler(d.allowlist, d.emailSender, d.approveToken, d.logger))
		d.logger.Info("approve-signup endpoint mounted on the internal listener")
	} else {
		d.logger.Warn("MITOS_CONSOLE_APPROVE_SIGNUP_TOKEN unset; approve-signup endpoint not mounted")
	}
	return mux
}

// serveInternal starts the cluster-internal listener for the M2M endpoints. It
// mirrors serveMetrics: a separate http.Server with bounded read timeouts, shut
// down when ctx is cancelled. A bind failure is fatal, unlike metrics: these
// endpoints are load-bearing for the front door, so a silent bind failure must
// not leave the console up but the resolve seam dark.
func serveInternal(ctx context.Context, logger *slog.Logger, addr string, mux *http.ServeMux) {
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	go func() {
		logger.Info("console internal listener listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("console: internal listener: %v", err)
		}
	}()
}
