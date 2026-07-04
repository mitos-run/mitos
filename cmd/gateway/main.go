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
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

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
// controller-runtime client. The client is returned alongside so main can wire
// the SAME client into the quota enforcer's live sandbox counter.
func newControlPlane(readyTimeout time.Duration, defaultPool, singleTenantNS string) (saas.ControlPlane, client.Client, error) {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, nil, err
	}
	c, err := client.New(cfg, client.Options{Scheme: newScheme()})
	if err != nil {
		return nil, nil, err
	}
	opts := []controlplane.Option{controlplane.WithReadyTimeout(readyTimeout)}
	if defaultPool != "" {
		opts = append(opts, controlplane.WithDefaultPool(defaultPool))
	}
	opts = append(opts, controlplane.WithSingleTenantNamespace(singleTenantNS))
	return controlplane.New(c, opts...), c, nil
}

func main() {
	addr := flag.String("addr", ":8080", "public listen address")
	allowStub := flag.Bool("allow-stub", false, "DEV ONLY: forward to an in-memory stub control plane that creates nothing; the default is the real control plane")
	readyTimeout := flag.Duration("ready-timeout", 120*time.Second, "how long a create waits for the sandbox to become Ready before returning a timeout error")
	defaultPool := flag.String("default-pool", "", "fallback pool name used when a create request names neither a pool nor an image")
	singleTenantNS := flag.String("single-tenant-namespace", os.Getenv("MITOS_GATEWAY_SINGLE_TENANT_NAMESPACE"), "pin all sandbox operations to this fixed namespace instead of the per-org mitos-org-<id> namespace; use for QA deployments where per-org namespaces are not provisioned and a shared SandboxPool exists in this namespace; empty (the default) keeps per-org namespacing; org-label authz is preserved regardless")
	databaseDSN := flag.String("database-dsn", "", "Postgres DSN for durable persistence (accounts, orgs, memberships, API keys). Falls back to the "+pgstore.EnvDSN+" env var. Empty means in-memory persistence (DEV ONLY). The value is a secret and is never logged.")
	enforceQuota := flag.Bool("enforce-quota", true, "enforce per-organization quotas, rate limits, and the abuse kill-switch before forwarding. Default on (the hosted profile). Set to false only for a trusted single-tenant deployment; the bypass is logged at startup.")
	trustedProxyHops := flag.Int("trusted-proxy-hops", 0, "number of trusted reverse-proxy hops in front of the gateway for client-IP resolution. 0 (the default) does NOT trust X-Forwarded-For and uses the connection RemoteAddr. Set to the count of trusted proxies (for example 1 behind a single ingress) so the per-IP rate limit keys on the real client; a too-short or spoofed X-Forwarded-For fails closed to RemoteAddr.")
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
	keys := saas.NewKeyService(store)

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
		real, k8sClient, err := newControlPlane(*readyTimeout, *defaultPool, *singleTenantNS)
		if err != nil {
			log.Fatalf("build control plane: %v", err)
		}
		cp = real
		// The counter shares the control plane's client and namespace model
		// (per-org, or the pinned single-tenant namespace) so it counts exactly
		// where the control plane creates.
		liveUsage = quota.NewLiveCounterSource(controlplane.NewLiveCounter(k8sClient, *singleTenantNS))
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
	encfg := enforcementConfig{enabled: *enforceQuota, trustedProxyHops: *trustedProxyHops, suspensions: suspensions, live: liveUsage}
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

	gw := saas.NewGateway(keys, wiring.enforcer, cp, logger, saas.WithTelemetry(tel)).
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
	logger.Info("gateway shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
