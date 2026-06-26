package main

import (
	"log/slog"
	"net/http"
	"os"
	"strings"

	"mitos.run/mitos/internal/runservice"
	"mitos.run/mitos/internal/saas/console"
	"mitos.run/mitos/internal/tenant"
)

// mountRunWithMitos mounts the Run with Mitos endpoints (GET /run/describe, POST
// /run) behind the session middleware so each call carries the verified org from
// the OIDC session (never a client header). It is OPT-IN via
// MITOS_CONSOLE_RUN_WITH_MITOS and currently UNVERIFIED AT RUNTIME: the
// unit-tested runservice core is wired to the console's ambient kube client, the
// verified-org session context, and the per-org namespace map (#410), but it has
// not yet been exercised against a live cluster end to end.
//
// This mounts the FIRST-PARTY flagship path for signed-in tenants. The public
// arbitrary-repo path stays gated on #341 and is not enabled here.
func mountRunWithMitos(mux *http.ServeMux, authMiddleware func(http.Handler) http.Handler, logger *slog.Logger) {
	if os.Getenv("MITOS_CONSOLE_RUN_WITH_MITOS") == "" {
		return
	}
	domain := strings.TrimSpace(os.Getenv("MITOS_EXPOSE_DOMAIN"))
	if domain == "" {
		logger.Warn("run-with-mitos requested but MITOS_EXPOSE_DOMAIN is empty; not mounting")
		return
	}
	kc, err := kubeClient()
	if err != nil {
		logger.Error("run-with-mitos: kube client unavailable; not mounting", "err", err)
		return
	}
	svc := runservice.New(&runservice.GitHubFetcher{}, &runservice.K8sApplier{Client: kc}, domain)
	resolver := &runservice.ContextResolver{
		OrgFromRequest:  func(r *http.Request) (string, bool) { return console.OrgFromContext(r.Context()) },
		NamespaceForOrg: tenant.NamespaceForOrg,
	}
	runMux := http.NewServeMux()
	runservice.NewHandler(svc, resolver).Routes(runMux)
	// Both "/run" (POST) and "/run/describe" (GET) route through the sub-mux behind
	// the session middleware.
	mux.Handle("/run", authMiddleware(runMux))
	mux.Handle("/run/", authMiddleware(runMux))
	logger.Info("run-with-mitos endpoints mounted behind session auth", "exposeDomain", domain)
}
