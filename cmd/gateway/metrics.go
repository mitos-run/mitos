package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// serveMetrics serves the registry on its own listener, NEVER on the public
// mux: the gateway's public surface is API-key authenticated, while /metrics
// is unauthenticated by convention and must stay cluster-internal (the chart
// exposes it as a named container port scraped by a PodMonitor, not through
// the public Service). The server is shut down by ctx.
func serveMetrics(ctx context.Context, logger *slog.Logger, addr string, reg *prometheus.Registry) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Metrics are operationally important but must never take the
			// serving process down; log and continue without them.
			logger.Error("metrics listener failed", "addr", addr, "err", err.Error())
		}
	}()
	logger.Info("metrics listening", "addr", addr)
}
