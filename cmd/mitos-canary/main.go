// Command mitos-canary continuously exercises the full user-facing sandbox path
// (auth -> gateway -> controller -> forkd -> fork -> exec -> terminate) against a
// live mitos install and exports the result as Prometheus metrics.
//
// It is the synthetic probe behind the production alerts: when a real user
// would get fork_unavailable, the canary sees it first and mitos_canary_up goes
// to 0. The canary reuses the exact HostedBackend the CLI and SDKs use, so it
// fails the same way a customer would, not through a privileged side channel.
//
// Liveness vs platform health are deliberately separate. /healthz reports only
// whether the probe LOOP is alive (so Kubernetes restarts a wedged canary and
// self-heals it), never whether the platform is healthy: a platform outage must
// NOT crash-loop the canary, or we would lose the very metrics the alerts read.
// Platform health lives in mitos_canary_up and the alerting rules on top of it.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"mitos.run/mitos/internal/agentcli"
)

func main() {
	cfg, err := parseConfig(os.Args[1:], os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mitos-canary:", err)
		os.Exit(2)
	}

	reg := prometheus.NewRegistry()
	m := newMetrics(reg)

	// A dedicated http client with a sane timeout so a hung gateway cannot wedge
	// a probe forever; the per-cycle context is the primary bound.
	backend := agentcli.NewHostedBackend(cfg.baseURL, cfg.apiKey, &http.Client{Timeout: cfg.cycleTimeout})

	h := &health{stalenessWindow: cfg.stalenessWindow}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := newServer(cfg.listenAddr, reg, h)
	go func() {
		log.Printf("mitos-canary: serving /metrics /healthz /readyz on %s", cfg.listenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("mitos-canary: http server error: %v", err)
			stop()
		}
	}()

	log.Printf("mitos-canary: probing template %q every %s (exec timeout %ds, cycle timeout %s)",
		cfg.pool, cfg.interval, cfg.execTimeout, cfg.cycleTimeout)
	runLoop(ctx, backend, m, h, cfg)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	log.Print("mitos-canary: stopped")
}

// config is the resolved canary configuration.
type config struct {
	baseURL         string        // gateway/API base; empty lets HostedBackend resolve in-cluster
	apiKey          string        // bearer token; never logged
	pool            string        // template name to fork each cycle
	interval        time.Duration // time between cycle starts
	execTimeout     int           // per-exec timeout in seconds
	cycleTimeout    time.Duration // whole-cycle deadline
	listenAddr      string        // metrics/health listen address
	stalenessWindow time.Duration // /healthz fails if the loop has not ticked within this window
}

// parseConfig resolves flags with environment-variable defaults. getenv is
// injected so the resolution is unit-testable. The api key is read from
// MITOS_API_KEY or, preferred for secret mounts, the file at MITOS_API_KEY_FILE.
func parseConfig(args []string, getenv func(string) string) (config, error) {
	fs := flag.NewFlagSet("mitos-canary", flag.ContinueOnError)
	baseURL := fs.String("base-url", getenv("MITOS_BASE_URL"), "gateway/API base URL; empty resolves the in-cluster gateway")
	pool := fs.String("pool", envOr(getenv, "MITOS_CANARY_POOL", "python"), "template name to fork each cycle")
	interval := fs.Duration("interval", envDurationOr(getenv, "MITOS_CANARY_INTERVAL", 60*time.Second), "time between probe cycles")
	execTimeout := fs.Int("exec-timeout", envIntOr(getenv, "MITOS_CANARY_EXEC_TIMEOUT", 30), "per-exec timeout in seconds")
	cycleTimeout := fs.Duration("cycle-timeout", envDurationOr(getenv, "MITOS_CANARY_CYCLE_TIMEOUT", 120*time.Second), "whole-cycle deadline")
	listenAddr := fs.String("listen", envOr(getenv, "MITOS_CANARY_LISTEN", ":9102"), "metrics/health listen address")
	staleness := fs.Duration("staleness-window", envDurationOr(getenv, "MITOS_CANARY_STALENESS", 0), "liveness fails if the loop has not ticked within this window (default 3x interval)")
	apiKeyFile := fs.String("api-key-file", getenv("MITOS_API_KEY_FILE"), "path to a file containing the api key (preferred over the env var)")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	apiKey := getenv("MITOS_API_KEY")
	if *apiKeyFile != "" {
		raw, err := os.ReadFile(*apiKeyFile)
		if err != nil {
			return config{}, fmt.Errorf("read api key file: %w", err)
		}
		apiKey = strings.TrimSpace(string(raw))
	}
	if apiKey == "" {
		return config{}, errors.New("no api key: set MITOS_API_KEY or MITOS_API_KEY_FILE (a mitos_live_ token scoped to the canary org)")
	}
	if *interval <= 0 {
		return config{}, fmt.Errorf("interval must be positive, got %s", *interval)
	}
	window := *staleness
	if window <= 0 {
		// Default: three missed ticks means the loop is wedged.
		window = 3 * *interval
	}
	return config{
		baseURL:         *baseURL,
		apiKey:          apiKey,
		pool:            *pool,
		interval:        *interval,
		execTimeout:     *execTimeout,
		cycleTimeout:    *cycleTimeout,
		listenAddr:      *listenAddr,
		stalenessWindow: window,
	}, nil
}

// runLoop probes immediately, then on every interval tick, until ctx is done.
// It owns the consecutive-failure gauge and the liveness heartbeat.
func runLoop(ctx context.Context, api sandboxAPI, m *metrics, h *health, cfg config) {
	ticker := time.NewTicker(cfg.interval)
	defer ticker.Stop()

	var consecFail int
	once := func() {
		h.tick() // heartbeat first: a probe that hangs still proves the loop was alive here
		nonce := newNonce()
		cctx, cancel := context.WithTimeout(ctx, cfg.cycleTimeout)
		defer cancel()

		res := runProbe(cctx, api, m, cfg.pool, nonce, cfg.execTimeout)
		if res.OK {
			consecFail = 0
			log.Printf("mitos-canary: cycle ok (template=%s)", cfg.pool)
		} else {
			consecFail++
			// res.Err comes from HostedBackend, which already redacts the api key.
			log.Printf("mitos-canary: cycle FAILED at %s (consecutive=%d): %v", res.Phase, consecFail, res.Err)
		}
		m.consecFail.Set(float64(consecFail))
		h.ready()
	}

	once()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			once()
		}
	}
}

// newNonce returns a per-cycle unique token embedded in the exec so a stale or
// proxied response cannot be mistaken for a fresh guest round-trip.
func newNonce() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is effectively impossible; fall back to a fixed
		// marker rather than panic, verification still requires an exact match.
		return "canary-fixed"
	}
	return "canary-" + hex.EncodeToString(b[:])
}

// health tracks the probe loop's liveness and first-cycle readiness. It is safe
// for concurrent use by the loop goroutine and the HTTP handlers.
type health struct {
	mu              sync.Mutex
	lastTick        time.Time
	started         bool
	stalenessWindow time.Duration
}

// tick records that the loop is alive at the top of a cycle.
func (h *health) tick() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastTick = time.Now()
}

// ready marks that at least one cycle has completed, so metrics are populated.
func (h *health) ready() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.started = true
}

// live reports whether the loop has ticked within the staleness window. It is
// the liveness signal: false means the loop wedged and the pod should restart.
func (h *health) live() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastTick.IsZero() {
		return true // not started probing yet; give it until the first tick
	}
	return time.Since(h.lastTick) < h.stalenessWindow
}

// isReady reports whether the first cycle has completed.
func (h *health) isReady() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.started
}

// newServer builds the metrics/health HTTP server. /healthz is liveness (loop
// alive), /readyz is readiness (first cycle done), /metrics is the scrape target.
func newServer(addr string, reg *prometheus.Registry, h *health) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if h.live() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("probe loop stalled"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if h.isReady() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ready"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("first cycle not complete"))
	})
	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

// envOr returns the environment value for key or def when unset/empty.
func envOr(getenv func(string) string, key, def string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return def
}

// envIntOr parses the environment value for key or returns def.
func envIntOr(getenv func(string) string, key string, def int) int {
	if v := getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// envDurationOr parses the environment value for key or returns def.
func envDurationOr(getenv func(string) string, key string, def time.Duration) time.Duration {
	if v := getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
