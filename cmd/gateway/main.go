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
	"flag"
	"log"
	"log/slog"
	"net/http"
	"os"
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
// controller-runtime client.
func newControlPlane(readyTimeout time.Duration, defaultPool string) (saas.ControlPlane, error) {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, err
	}
	c, err := client.New(cfg, client.Options{Scheme: newScheme()})
	if err != nil {
		return nil, err
	}
	opts := []controlplane.Option{controlplane.WithReadyTimeout(readyTimeout)}
	if defaultPool != "" {
		opts = append(opts, controlplane.WithDefaultPool(defaultPool))
	}
	return controlplane.New(c, opts...), nil
}

func main() {
	addr := flag.String("addr", ":8080", "public listen address")
	allowStub := flag.Bool("allow-stub", false, "DEV ONLY: forward to an in-memory stub control plane that creates nothing; the default is the real control plane")
	readyTimeout := flag.Duration("ready-timeout", 120*time.Second, "how long a create waits for the sandbox to become Ready before returning a timeout error")
	defaultPool := flag.String("default-pool", "", "fallback pool name used when a create request names neither a pool nor an image")
	databaseDSN := flag.String("database-dsn", "", "Postgres DSN for durable persistence (accounts, orgs, memberships, API keys). Falls back to the "+pgstore.EnvDSN+" env var. Empty means in-memory persistence (DEV ONLY). The value is a secret and is never logged.")
	enforceQuota := flag.Bool("enforce-quota", true, "enforce per-organization quotas, rate limits, and the abuse kill-switch before forwarding. Default on (the hosted profile). Set to false only for a trusted single-tenant deployment; the bypass is logged at startup.")
	trustedProxyHops := flag.Int("trusted-proxy-hops", 0, "number of trusted reverse-proxy hops in front of the gateway for client-IP resolution. 0 (the default) does NOT trust X-Forwarded-For and uses the connection RemoteAddr. Set to the count of trusted proxies (for example 1 behind a single ingress) so the per-IP rate limit keys on the real client; a too-short or spoofed X-Forwarded-For fails closed to RemoteAddr.")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Durable Postgres when a DSN is configured (flag or MITOS_DATABASE_DSN),
	// in-memory otherwise (dev only). The DSN value is never logged. A bad DSN
	// fails fast below.
	store, closeStore, err := pgstore.ResolveStore(context.Background(), *databaseDSN, logger)
	if err != nil {
		log.Fatalf("persistence: %v", err)
	}
	defer closeStore()
	keys := saas.NewKeyService(store)

	var cp saas.ControlPlane
	if *allowStub {
		logger.Warn("gateway running with the DEV stub control plane; no sandboxes are created (--allow-stub)")
		cp = stubControlPlane{}
	} else {
		real, err := newControlPlane(*readyTimeout, *defaultPool)
		if err != nil {
			log.Fatalf("build control plane: %v", err)
		}
		cp = real
		logger.Info("gateway using the real control plane", "ready_timeout", readyTimeout.String(), "default_pool", *defaultPool)
	}

	// Build the quota/abuse enforcement surface: the real quota.Enforcer wrapped in
	// the gateway adapter when enabled (the hosted default), or the permissive
	// AllowAllQuota when explicitly disabled. The same suspension store backs the
	// enforcer, the abuse kill-switch, and the billing suspender, so a suspended org
	// is blocked at the gateway. The mode is logged so the posture is never silent.
	encfg := enforcementConfig{enabled: *enforceQuota, trustedProxyHops: *trustedProxyHops}
	wiring := buildQuotaEnforcer(encfg)
	logEnforcementMode(logger, encfg, wiring)
	_ = wiring.killSwitch       // operator emergency-stop / abuse-signal driver (wired into the suspension store).
	_ = wiring.billingSuspender // billing past-due / spend-cap driver (wired into the suspension store).

	gw := saas.NewGateway(keys, wiring.enforcer, cp, logger).
		WithTrustedProxyHops(saas.TrustedProxyHops(*trustedProxyHops))

	mux := http.NewServeMux()
	mux.Handle("/v1/", gw)
	mux.Handle("/sandbox.v1.Sandbox/", gw)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	logger.Info("gateway listening", "addr", *addr)
	srv := &http.Server{Addr: *addr, Handler: mux}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
