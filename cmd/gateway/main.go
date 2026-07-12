// Command gateway is the public, customer-facing front door for the hosted
// offering (issue #210). It terminates customer API key authentication, resolves
// the owning organization, attaches an org context, enforces quota through the
// QuotaEnforcer seam (issue #213 implements the real enforcer), and forwards
// authenticated, org-scoped requests to the control plane. By default it forwards
// to the REAL control plane (internal/saas/controlplane), which turns an
// org-scoped request into Kubernetes actions on the mitos.run/v1 Sandbox kind and
// reverse-proxies runtime calls (exec, files, run_code over Connect) to the
// sandbox endpoint with the per-sandbox bearer token.
//
// A customer key VALUE is never logged; the gateway logs the key id, masked
// prefix, org id, and op only. The per-sandbox token is never logged and is
// returned to the caller only on create.
//
// Production gate: this front door is NOT cleared for production tenants until
// the external security review (issue #194) covers the public attack surface it
// adds. See docs/threat-model.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/controlplane"
	"mitos.run/mitos/internal/saas/pgstore"
	"mitos.run/mitos/internal/saas/quota"
	"mitos.run/mitos/internal/telemetry"
)

// stubControlPlane is a dev-only forward target, selected with --allow-stub. It
// rejects nothing and creates nothing: it echoes the resolved org and op so a
// smoke test can confirm authn and org-resolution worked WITHOUT a live cluster.
// It is never the default, so an operator cannot mistake a real deployment for a
// wired control plane.
type stubControlPlane struct{}

func (stubControlPlane) Forward(_ context.Context, req saas.ForwardRequest) (saas.ForwardResponse, error) {
	// Echo the resolved org and op so a smoke test can confirm authn and
	// org-resolution worked, without implying a real sandbox was created.
	body := []byte(`{"forwarded":true,"org":"` + req.OrgID + `","op":"` + req.Op + `"}`)
	return saas.ForwardResponse{Status: http.StatusOK, Body: body}, nil
}

// newScheme builds the scheme the control-plane client needs: corev1 (the
// per-sandbox token Secret) and mitos.run/v1 (the Sandbox kind).
func newScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1.AddToScheme(scheme))
	return scheme
}

// newControlPlane builds the real control plane over an in-cluster
// controller-runtime client. The client is WATCH capable so the control plane
// observes sandbox readiness event driven instead of on a poll-tick boundary;
// the poll interval governs only the fail-open fallback loop. The client is
// returned alongside so main can wire the SAME client into the quota
// enforcer's live sandbox counter.
func newControlPlane(readyTimeout, readyPollInterval time.Duration, defaultPool, singleTenantNS string, checkout controlplane.CheckoutConfig) (*controlplane.K8sControlPlane, client.Client, error) {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("load kubeconfig for the control-plane client: %w", err)
	}
	c, err := client.NewWithWatch(cfg, client.Options{Scheme: newScheme()})
	if err != nil {
		return nil, nil, fmt.Errorf("build controller-runtime watch client: %w", err)
	}
	opts := []controlplane.Option{
		controlplane.WithReadyTimeout(readyTimeout),
		controlplane.WithPollInterval(readyPollInterval),
	}
	if defaultPool != "" {
		opts = append(opts, controlplane.WithDefaultPool(defaultPool))
	}
	opts = append(opts, controlplane.WithSingleTenantNamespace(singleTenantNS))
	if len(checkout.Pools) > 0 {
		opts = append(opts, controlplane.WithCheckout(checkout))
	}
	return controlplane.New(c, opts...), c, nil
}

// splitNonEmpty splits a comma-separated flag value into trimmed, non-empty
// items.
func splitNonEmpty(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// envInt and envDuration read an env default for a numeric flag; any parse
// problem keeps the compiled-in default.
func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func main() {
	addr := flag.String("addr", ":8080", "public listen address")
	metricsAddr := flag.String("metrics-addr", ":9100", "Prometheus metrics listen address (a SEPARATE cluster-internal listener; /metrics is never mounted on the public mux). Empty disables the metrics listener.")
	allowStub := flag.Bool("allow-stub", false, "DEV ONLY: forward to an in-memory stub control plane that creates nothing; the default is the real control plane")
	readyTimeout := flag.Duration("ready-timeout", 120*time.Second, "how long a create waits for the sandbox to become Ready before returning a timeout error")
	readyPollInterval := flag.Duration("ready-poll-interval", 25*time.Millisecond, "status poll interval for the create readiness FALLBACK loop. Readiness is normally observed by a watch on the sandbox; this interval is used only when the watch cannot be established.")
	var orgTierFlags multiFlag
	flag.Var(&orgTierFlags, "org-tier", "grant one org a tier above the fail-closed free default, as <orgID>=<tier> (tiers: free, starter, pro). Repeatable. OPERATOR configuration only; an org not named here stays on the tightest tier. Note that the pro tier also opens the sandbox egress posture.")
	defaultPool := flag.String("default-pool", "", "fallback pool name used when a create request names neither a pool nor an image")
	singleTenantNS := flag.String("single-tenant-namespace", os.Getenv("MITOS_GATEWAY_SINGLE_TENANT_NAMESPACE"), "pin all sandbox operations to this fixed namespace instead of the per-org mitos-org-<id> namespace; use for QA deployments where per-org namespaces are not provisioned and a shared SandboxPool exists in this namespace; empty (the default) keeps per-org namespacing; org-label authz is preserved regardless")
	databaseDSN := flag.String("database-dsn", "", "Postgres DSN for durable persistence (accounts, orgs, memberships, API keys). Falls back to the "+pgstore.EnvDSN+" env var. Empty means in-memory persistence (DEV ONLY). The value is a secret and is never logged.")
	enforceQuota := flag.Bool("enforce-quota", true, "enforce per-organization quotas, rate limits, and the abuse kill-switch before forwarding. Default on (the hosted profile). Set to false only for a trusted single-tenant deployment; the bypass is logged at startup.")
	trustedProxyHops := flag.Int("trusted-proxy-hops", 0, "number of trusted reverse-proxy hops in front of the gateway for client-IP resolution. 0 (the default) does NOT trust X-Forwarded-For and uses the connection RemoteAddr. Set to the count of trusted proxies (for example 1 behind a single ingress) so the per-IP rate limit keys on the real client; a too-short or spoofed X-Forwarded-For fails closed to RemoteAddr.")
	checkoutPools := flag.String("checkout-pools", os.Getenv("MITOS_GATEWAY_CHECKOUT_POOLS"), "comma-separated pool names served by the pre-claimed checkout buffer (empty disables the feature); requires --single-tenant-namespace")
	checkoutFloor := flag.Int("checkout-floor", envInt("MITOS_GATEWAY_CHECKOUT_FLOOR", 2), "buffered sandboxes to keep ready per checkout pool")
	checkoutCap := flag.Int("checkout-cap", envInt("MITOS_GATEWAY_CHECKOUT_CAP", 4), "hard ceiling of buffered sandboxes per checkout pool")
	checkoutMaxAge := flag.Duration("checkout-max-age", envDuration("MITOS_GATEWAY_CHECKOUT_MAX_AGE", 10*time.Minute), "buffered sandboxes older than this are recycled")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Durable Postgres when a DSN is configured (flag or MITOS_DATABASE_DSN),
	// in-memory otherwise (dev only). The DSN value is never logged. A bad DSN
	// fails fast below. The pool is kept so the kill-switch suspension store can
	// share the same connection (mirroring cmd/console's durable-store wiring).
	store, pool, closeStore, err := pgstore.ResolveStoreWithPool(context.Background(), *databaseDSN, logger)
	if err != nil {
		log.Fatalf("persistence: %v", err)
	}
	defer closeStore()
	// API key hash pepper (issue #733, item 3). Opt-in via MITOS_API_KEY_PEPPER;
	// when set, the SAME value must be configured on the console (and CLI) or
	// keys will not verify. The value is never logged; only its presence.
	var keyOpts []saas.KeyServiceOption
	if pepper, ok := saas.KeyPepperFromEnv(); ok {
		keyOpts = append(keyOpts, saas.WithSalt(pepper))
		logger.Info("api key pepper configured", "env", saas.EnvKeyPepper)
	} else {
		logger.Info("api key pepper not set; keys are hashed without a pepper", "env", saas.EnvKeyPepper)
	}
	keys := saas.NewKeyService(store, keyOpts...)

	// liveUsage is the enforcer's live-usage input: the cluster-backed sandbox
	// counter when the real control plane is in use (issue #615 seam 2), so the
	// concurrency cap is enforced against what the org is ACTUALLY running.
	// With the dev stub there is no cluster client and it stays nil; the
	// enforcer then reports the live caps as not enforced.
	var liveUsage quota.LiveUsageSource
	var cp saas.ControlPlane
	if *allowStub {
		logger.Warn("gateway running with the DEV stub control plane; no sandboxes are created (--allow-stub)")
		cp = stubControlPlane{}
	} else {
		checkoutCfg := controlplane.CheckoutConfig{
			Pools:  splitNonEmpty(*checkoutPools),
			Floor:  *checkoutFloor,
			Cap:    *checkoutCap,
			MaxAge: *checkoutMaxAge,
		}
		real, k8sClient, err := newControlPlane(*readyTimeout, *readyPollInterval, *defaultPool, *singleTenantNS, checkoutCfg)
		if err != nil {
			log.Fatalf("build control plane: %v", err)
		}
		cp = real
		// The checkout buffer's refill/janitor loop runs for the server's
		// lifetime; the hot path only ever reads its cache. Not being silent
		// about a half-configured feature: --checkout-pools without
		// --single-tenant-namespace leaves the buffer off by design (see the
		// spec's per-org-namespace migration note), and that must be loud.
		switch {
		case len(checkoutCfg.Pools) > 0 && *singleTenantNS == "":
			logger.Warn("checkout pools configured but --single-tenant-namespace is unset; the pre-claimed checkout stays OFF", "pools", *checkoutPools)
		case len(checkoutCfg.Pools) > 0:
			real.StartCheckout(context.Background())
			logger.Info("pre-claimed checkout enabled", "pools", *checkoutPools, "floor", *checkoutFloor, "cap", *checkoutCap, "max_age", checkoutMaxAge.String())
		}
		// The counter shares the control plane's client and namespace model
		// (per-org, or the pinned single-tenant namespace) so it counts exactly
		// where the control plane creates.
		counter := controlplane.NewLiveCounter(k8sClient, *singleTenantNS)
		liveUsage = quota.NewLiveCounterSource(counter)
		// One startup self-check so a persistent RBAC/scheme misconfiguration
		// (which would fail-closed-deny EVERY create while reads keep working)
		// is loud at boot; a failure logs remediation and keeps serving.
		probeCtx, cancelProbe := context.WithTimeout(context.Background(), 10*time.Second)
		probeLiveCounter(probeCtx, counter, logger)
		cancelProbe()
		if *singleTenantNS != "" {
			logger.Info("gateway using the real control plane in single-tenant mode", "ready_timeout", readyTimeout.String(), "default_pool", *defaultPool, "single_tenant_namespace", *singleTenantNS)
		} else {
			logger.Info("gateway using the real control plane", "ready_timeout", readyTimeout.String(), "default_pool", *defaultPool)
		}
	}

	// Kill-switch suspension store: durable and replica-shared over the SAME
	// Postgres pool the key store uses when a database is configured, so a
	// suspension survives restarts and binds every replica (issue #615). The
	// short-TTL read cache keeps the per-request suspension check off Postgres;
	// a suspension written on another replica takes effect here within the TTL.
	// Without a database the store stays in-process (dev only) and
	// buildQuotaEnforcer names that mode in the startup log.
	var suspensions quota.SuspensionStore
	if pool != nil {
		suspensions = quota.NewCachedSuspensionStore(
			pgstore.NewPgSuspensionStore(pool), quota.DefaultSuspensionCacheTTL, nil)
	}

	// Build the quota/abuse enforcement surface: the real quota.Enforcer wrapped in
	// the gateway adapter when enabled (the hosted default), or the permissive
	// AllowAllQuota when explicitly disabled. The same suspension store backs the
	// enforcer, the abuse kill-switch, and the billing suspender, so a suspended org
	// is blocked at the gateway. The mode is logged so the posture is never silent.
	orgTiers, err := parseOrgTiers(orgTierFlags)
	if err != nil {
		// Fail closed and loudly: a malformed or unknown tier must never fall back to a
		// silently different set of limits.
		logger.Error("invalid --org-tier", "err", err)
		os.Exit(1)
	}
	encfg := enforcementConfig{enabled: *enforceQuota, trustedProxyHops: *trustedProxyHops, suspensions: suspensions, live: liveUsage, orgTiers: orgTiers}
	wiring := buildQuotaEnforcer(encfg)
	logEnforcementMode(logger, encfg, wiring)
	_ = wiring.killSwitch       // operator emergency-stop / abuse-signal driver (wired into the suspension store).
	_ = wiring.billingSuspender // billing past-due / spend-cap driver (wired into the suspension store).

	// Product telemetry is OPT-IN and OFF by default. FromEnv returns a no-op
	// emitter unless MITOS_TELEMETRY_ENABLED is truthy AND an endpoint is set, and
	// is force-disabled by DO_NOT_TRACK. The salt and any token are secrets and are
	// never logged. A startup line states enabled/disabled and the sink name.
	tel := telemetry.FromEnv(logger)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tel.Shutdown(ctx)
	}()

	// Request-outcome and auth-denial counters for the #617 SaaS alerts, served
	// on their own cluster-internal listener below, never on the public mux.
	// Labels are bounded classes only; no key, org, or path detail is exported.
	metricsRegistry := prometheus.NewRegistry()
	gatewayMetrics := saas.NewGatewayMetrics()
	gatewayMetrics.MustRegister(metricsRegistry)

	gw := saas.NewGateway(keys, wiring.enforcer, cp, logger, saas.WithTelemetry(tel), saas.WithGatewayMetrics(gatewayMetrics)).
		WithTrustedProxyHops(saas.TrustedProxyHops(*trustedProxyHops))

	mux := http.NewServeMux()
	mux.Handle("/v1/", gw)
	mux.Handle("/sandbox.v1.Sandbox/", gw)
	// Liveness stays a static 200 (the process is up); readiness is split out to
	// /readyz so a draining replica is removed from the Service before its
	// in-flight requests are cut. draining flips on SIGTERM/SIGINT below.
	var draining atomic.Bool
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", newReadyzHandler(&draining))

	// rootCtx stops the metrics listener on shutdown, alongside the main server.
	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()
	if *metricsAddr != "" {
		serveMetrics(rootCtx, logger, *metricsAddr, metricsRegistry)
	}

	logger.Info("gateway listening", "addr", *addr)
	srv := &http.Server{Addr: *addr, Handler: mux}

	// Graceful shutdown (same pattern and timeout as cmd/frontdoor): serve in a
	// goroutine, then on SIGTERM/SIGINT flip readiness to 503 and drain in-flight
	// requests (API calls, billing-relevant creates) inside a bounded timeout
	// instead of dropping them on every rollout.
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("gateway: listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	<-stop
	draining.Store(true)
	rootCancel()
	logger.Info("gateway shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
