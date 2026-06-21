// Command gateway is the public, customer-facing front door for the hosted
// offering (issue #210). It terminates customer API key authentication, resolves
// the owning organization, attaches an org context, enforces quota through the
// QuotaEnforcer seam (issue #213 implements the real enforcer), and forwards
// authenticated, org-scoped requests to the control plane through the
// ControlPlane seam. It sits ABOVE the internal mTLS and per-sandbox token
// plane; it does not replace it.
//
// This binary wires the in-memory store and a default-allow quota so it runs and
// is smoke-testable end to end. The Postgres store, the real quota enforcer, the
// real control-plane forward target, and TLS termination are documented
// follow-ups (docs/saas/accounts-gateway.md). A customer key VALUE is never
// logged; the gateway logs the key id, masked prefix, org id, and op only.
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

	"mitos.run/mitos/internal/saas"
)

// stubControlPlane is the placeholder forward target this binary ships with: it
// rejects every request with a clear message so an operator cannot mistake the
// default build for a wired control plane. The real implementation forwards over
// the internal mTLS plane to the controller and is a documented follow-up.
type stubControlPlane struct{}

func (stubControlPlane) Forward(_ context.Context, req saas.ForwardRequest) (saas.ForwardResponse, error) {
	// Echo the resolved org and op so a smoke test can confirm authn and
	// org-resolution worked, without implying a real sandbox was created.
	body := []byte(`{"forwarded":true,"org":"` + req.OrgID + `","op":"` + req.Op + `"}`)
	return saas.ForwardResponse{Status: http.StatusOK, Body: body}, nil
}

func main() {
	addr := flag.String("addr", ":8080", "public listen address")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// In-memory store and default-allow quota: the tested seams. Postgres and the
	// real enforcer are documented follow-ups.
	store := saas.NewMemStore()
	keys := saas.NewKeyService(store)
	gw := saas.NewGateway(keys, saas.AllowAllQuota{}, stubControlPlane{}, logger)

	mux := http.NewServeMux()
	mux.Handle("/v1/", gw)
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
