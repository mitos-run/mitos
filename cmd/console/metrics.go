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

// drawdownMetrics is the Prometheus view of the usage drawdown driver
// (issue #617), the loop that settles metered usage against prepaid credit.
// Three series, each backing one alert:
//
//   - cycleErrors -> DrawdownFailing: per-cycle failed operations (org list,
//     record list, settled-window read, per-record drawdown, marker prune).
//   - lastSuccess -> DrawdownStalled: unix time of the last cycle that
//     completed with zero failed operations. It is initialized to the driver
//     start time, so staleness is measured from boot for a driver that never
//     succeeds, and the series exists ONLY when the driver is enabled (a
//     deployment without metering cannot fire the stall alert).
//   - creditExhausted -> OrgCreditExhausted: newly settled records whose cost
//     exceeded the org's remaining prepaid credit (DrawdownResult.Remaining >
//     0). Replays never count.
//
// SECRET HYGIENE: all three are label-free; no org id, balance, or cost ever
// enters a label or value (matching the driver's counts-only logging rule).
type drawdownMetrics struct {
	cycleErrors     prometheus.Counter
	lastSuccess     prometheus.Gauge
	creditExhausted prometheus.Counter
}

// newDrawdownMetrics builds the drawdown driver metrics. They are
// unregistered; main registers them onto the console metrics registry ONLY
// when the driver is enabled, mirroring the internal/usage Metrics shape.
func newDrawdownMetrics() *drawdownMetrics {
	return &drawdownMetrics{
		cycleErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "mitos_drawdown_cycle_errors_total",
			Help: "Failed operations across drawdown cycles (org list, usage record list, settled-window read, per-record drawdown, marker prune). Each failed operation is counted and skipped, never aborting the cycle; a sustained rate means some usage is not settling against credit.",
		}),
		lastSuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "mitos_drawdown_last_success_timestamp_seconds",
			Help: "Unix time of the last drawdown cycle that completed with zero failed operations, initialized to the driver start time. Present only when the drawdown driver is enabled. time() minus this value is the staleness the DrawdownStalled alert watches.",
		}),
		creditExhausted: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "mitos_drawdown_credit_exhausted_total",
			Help: "Newly settled usage records whose cost exceeded the org's remaining prepaid credit (the unbacked remainder is nonzero). Replayed records never count. Sustained volume means at least one org keeps consuming beyond its prepaid balance.",
		}),
	}
}

// mustRegister registers the drawdown metrics on reg. Panics on duplicate
// registration, the standard fail-fast for a misconfigured wiring.
func (m *drawdownMetrics) mustRegister(reg prometheus.Registerer) {
	reg.MustRegister(m.cycleErrors, m.lastSuccess, m.creditExhausted)
}

// markStarted stamps the driver start time into the last-success gauge so the
// DrawdownStalled staleness is measured from boot until the first clean cycle.
// Nil-safe.
func (m *drawdownMetrics) markStarted(now time.Time) {
	if m == nil {
		return
	}
	m.lastSuccess.Set(float64(now.Unix()))
}

// observeCycle records one completed cycle: every failed operation is added to
// the error counter, and a cycle with zero failures stamps the last-success
// gauge. Nil-safe.
func (m *drawdownMetrics) observeCycle(stats drawdownStats, now time.Time) {
	if m == nil {
		return
	}
	m.cycleErrors.Add(float64(stats.failed))
	if stats.failed == 0 {
		m.lastSuccess.Set(float64(now.Unix()))
	}
}

// observeCreditExhausted counts one newly settled record whose cost exceeded
// the org's remaining prepaid credit. Nil-safe.
func (m *drawdownMetrics) observeCreditExhausted() {
	if m == nil {
		return
	}
	m.creditExhausted.Inc()
}

// newDBPingFailuresCounter builds the readiness-probe Postgres failure counter
// (issue #617). The kubelet drives /readyz on a fixed cadence (the chart sets
// periodSeconds 10), so this counter's rate is a continuous database
// reachability signal without a dedicated prober.
func newDBPingFailuresCounter() prometheus.Counter {
	return prometheus.NewCounter(prometheus.CounterOpts{
		Name: "mitos_console_db_ping_failures_total",
		Help: "Readiness-probe pings of the configured Postgres pool that failed. Driven by the kubelet /readyz cadence, so the rate approximates continuous database reachability. Absent when the console runs on in-memory persistence (dev only). The error detail is never exported.",
	})
}

// serveMetrics serves the registry on its own listener, NEVER on the public
// mux: the console's public surface is session/signature authenticated, while
// /metrics is unauthenticated by convention and must stay cluster-internal
// (the chart exposes it as a named container port scraped by a PodMonitor,
// not through the Service or Ingress). The server is shut down by ctx.
func serveMetrics(ctx context.Context, logger *slog.Logger, addr string, reg *prometheus.Registry) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	// Timeouts bound a slow or stuck scraper (gosec G112): the listener is
	// cluster-internal, but a Server without ReadHeaderTimeout still holds a
	// goroutine per dangling connection.
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
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Metrics are operationally important but must never take the
			// serving process down; log and continue without them.
			logger.Error("metrics listener failed", "addr", addr, "err", err.Error())
		}
	}()
	logger.Info("metrics listening", "addr", addr)
}
