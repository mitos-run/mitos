// Command preview-proxy is the per-sandbox preview URL reverse proxy (issue
// #126). One entrypoint fronts many ephemeral per-sandbox backends: it parses
// <label>.<domain>, verifies a signed expiring preview token plus
// the per-sandbox bearer gate, looks up the backend in a route table built from
// Ready claims, and proxies to it. Automatic TLS is wired behind the
// preview.CertProvider seam; this binary ships the self-signed provider so it
// serves HTTPS without ACME. Real on-demand TLS (CertMagic) is a documented
// follow-up that needs a public domain and DNS (see docs/preview-urls.md).
//
// This binary wires an in-memory route table that an operator (or the controller
// wiring follow-up) populates from Ready SandboxClaims. The preview signing
// secret is read from MITOS_PREVIEW_SECRET and is never logged.
//
// Auth ladder (slice 4): when -oidc-issuer is set the proxy wires a native OIDC
// relying party on auth.<domain>. The OIDC client secret and all HMAC session/
// grant/SSO/state secrets are env-sourced (never argv). When -oidc-issuer is
// unset the proxy operates in token-only/public mode; private/org/authenticated
// tiers return 401.
//
// Production gate: this proxy adds a public ingress surface and is NOT cleared
// for production tenants until the external security review (issue #194) covers
// it. See docs/threat-model.md.
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
	"syscall"
	"time"

	"mitos.run/mitos/internal/preview"
	"mitos.run/mitos/internal/saas/oidcauth"
)

func main() {
	addr := flag.String("addr", ":8443", "HTTPS listen address")
	httpAddr := flag.String("http-addr", "", "optional plaintext HTTP listen address (testing / behind a TLS terminator)")
	domain := flag.String("domain", "", "base preview domain; routes <label>.<domain>")
	tlsCert := flag.String("tls-cert", "", "path to the wildcard TLS certificate (PEM); empty uses a self-signed cert")
	tlsKey := flag.String("tls-key", "", "path to the wildcard TLS private key (PEM)")

	// OIDC relying-party flags. When -oidc-issuer is empty the proxy runs in
	// token-only/public mode; OIDC-backed tiers (private/org/authenticated)
	// return 401. Secrets are sourced from env, never flags, so they do not
	// appear in process listings.
	oidcIssuer := flag.String("oidc-issuer", "", "OIDC issuer URL (e.g. https://accounts.google.com); empty disables OIDC")
	oidcClientID := flag.String("oidc-client-id", "", "OIDC client ID")

	// Identity resolve endpoint: optional SaaS in-cluster service that maps a
	// verified email to org IDs. The bearer token is env-sourced.
	resolveURL := flag.String("resolve-url", "", "SaaS identity resolve endpoint URL (e.g. http://console:8080); empty disables")

	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if *domain == "" {
		log.Fatal("preview-proxy: -domain is required (for example example.com)")
	}

	// The signing secret is a deployment secret. It is never logged; only its
	// presence is asserted here.
	secret := os.Getenv("MITOS_PREVIEW_SECRET")
	if secret == "" {
		log.Fatal("preview-proxy: MITOS_PREVIEW_SECRET is required and must be at least 16 bytes")
	}
	signer, err := preview.NewSigner([]byte(secret))
	if err != nil {
		log.Fatalf("preview-proxy: %v", err)
	}

	// Grant and session HMAC secrets: distinct keys limit blast radius if one
	// leaks. Never logged; only presence is checked.
	var grantSigner *preview.GrantSigner
	var sessions *preview.SessionCodec

	grantSecret := os.Getenv("MITOS_EXPOSE_GRANT_SECRET")
	sessionSecret := os.Getenv("MITOS_EXPOSE_SESSION_SECRET")

	if grantSecret != "" {
		gs, gsErr := preview.NewGrantSigner([]byte(grantSecret))
		if gsErr != nil {
			log.Fatalf("preview-proxy: grant signer: %v", gsErr)
		}
		grantSigner = gs
	}
	if sessionSecret != "" {
		sc, scErr := preview.NewSessionCodec([]byte(sessionSecret))
		if scErr != nil {
			log.Fatalf("preview-proxy: session codec: %v", scErr)
		}
		sessions = sc
	}

	routes := preview.NewRouteTable()

	// Wire the OIDC auth origin when an issuer is configured.
	var authOrigin *preview.AuthOrigin

	if *oidcIssuer != "" {
		if *oidcClientID == "" {
			log.Fatal("preview-proxy: -oidc-client-id is required when -oidc-issuer is set")
		}
		if grantSigner == nil {
			log.Fatal("preview-proxy: MITOS_EXPOSE_GRANT_SECRET is required when -oidc-issuer is set")
		}
		if sessions == nil {
			log.Fatal("preview-proxy: MITOS_EXPOSE_SESSION_SECRET is required when -oidc-issuer is set")
		}

		// ssoSecret and stateSecret gate the SSO and CSRF state cookies respectively.
		// Both are bearer credentials and are never logged.
		ssoSecret := os.Getenv("MITOS_EXPOSE_SSO_SECRET")
		stateSecret := os.Getenv("MITOS_EXPOSE_STATE_SECRET")
		if ssoSecret == "" {
			log.Fatal("preview-proxy: MITOS_EXPOSE_SSO_SECRET is required when -oidc-issuer is set")
		}
		if stateSecret == "" {
			log.Fatal("preview-proxy: MITOS_EXPOSE_STATE_SECRET is required when -oidc-issuer is set")
		}

		ssoCodec, ssoErr := preview.NewSessionCodec([]byte(ssoSecret))
		if ssoErr != nil {
			log.Fatalf("preview-proxy: SSO session codec: %v", ssoErr)
		}
		stateCodec, stateErr := preview.NewSessionCodec([]byte(stateSecret))
		if stateErr != nil {
			log.Fatalf("preview-proxy: state session codec: %v", stateErr)
		}

		// oidcClientSecret is a deployment secret and must not be logged.
		oidcClientSecret := os.Getenv("MITOS_EXPOSE_OIDC_CLIENT_SECRET")
		if oidcClientSecret == "" {
			log.Fatal("preview-proxy: MITOS_EXPOSE_OIDC_CLIENT_SECRET is required when -oidc-issuer is set")
		}

		// redirect URL: default to https://auth.<domain>/auth/callback.
		redirectURL := "https://auth." + *domain + "/auth/callback"

		verifier, exchanger, provErr := oidcauth.NewProvider(context.Background(), oidcauth.ProviderConfig{
			IssuerURL:    *oidcIssuer,
			ClientID:     *oidcClientID,
			ClientSecret: oidcClientSecret,
			RedirectURL:  redirectURL,
		})
		if provErr != nil {
			log.Fatalf("preview-proxy: OIDC provider setup: %v", provErr)
		}

		var resolver *preview.Resolver
		if *resolveURL != "" {
			resolveToken := os.Getenv("MITOS_EXPOSE_RESOLVE_TOKEN")
			// Bearer token is a secret and is never logged.
			resolver = preview.NewResolver(*resolveURL, resolveToken)
		}

		authOrigin = &preview.AuthOrigin{
			Verifier:     verifier,
			Exchanger:    exchanger,
			Grants:       grantSigner,
			SSO:          ssoCodec,
			StateCodec:   stateCodec,
			Resolver:     resolver,
			Routes:       routes,
			ExposeDomain: *domain,
		}

		logger.Info("preview-proxy: OIDC auth origin wired", "issuer", *oidcIssuer, "auth-host", "auth."+*domain)
	} else {
		logger.Info("preview-proxy: OIDC issuer not set; proxy operates in token-only/public mode; private/org/authenticated tiers return 401")
	}

	proxy := preview.NewProxy(preview.Config{
		Domain:      *domain,
		Signer:      signer,
		Routes:      routes,
		Logger:      logger,
		AuthOrigin:  authOrigin,
		Sessions:    sessions,
		GrantSigner: grantSigner,
	})

	// The admin token gates the route-sync endpoint. It is a bearer credential
	// and is never logged; only its presence is checked here.
	adminToken := os.Getenv("MITOS_EXPOSE_ADMIN_TOKEN")

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// Admin route-sync endpoint: mount before the catch-all proxy handler.
	if adminToken != "" {
		mux.Handle("/internal/routes", preview.NewAdminHandler(routes, adminToken, logger))
	} else {
		logger.Info("preview-proxy: MITOS_EXPOSE_ADMIN_TOKEN is unset; route-sync endpoint disabled (routes must be seeded another way)")
	}
	// Everything else is preview traffic resolved by vhost.
	mux.Handle("/", proxy)

	var certProvider preview.CertProvider
	if *tlsCert != "" || *tlsKey != "" {
		if *tlsCert == "" || *tlsKey == "" {
			log.Fatal("preview-proxy: --tls-cert and --tls-key must be set together")
		}
		wp, err := preview.NewWildcardProvider(*tlsCert, *tlsKey)
		if err != nil {
			log.Fatalf("preview-proxy: %v", err)
		}
		certProvider = wp
		logger.Info("preview-proxy: serving the operator wildcard certificate", "cert", *tlsCert)
	} else {
		ss, err := preview.NewSelfSignedProvider()
		if err != nil {
			log.Fatalf("preview-proxy: cert provider: %v", err)
		}
		certProvider = ss
		logger.Info("preview-proxy: serving self-signed certificates (no --tls-cert set; not browser trusted)")
	}

	servers := startServers(*addr, *httpAddr, mux, certProvider, logger)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	<-stop
	logger.Info("preview-proxy shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(ctx)
	}
}

// startServers launches the HTTPS server (always) and an optional plaintext
// server, returning them for graceful shutdown.
func startServers(httpsAddr, httpAddr string, h http.Handler, cp preview.CertProvider, logger *slog.Logger) []*http.Server {
	var servers []*http.Server

	httpsSrv := &http.Server{
		Addr:      httpsAddr,
		Handler:   h,
		TLSConfig: preview.ServerTLSConfig(cp),
	}
	servers = append(servers, httpsSrv)
	go func() {
		logger.Info("preview-proxy listening (https)", "addr", httpsAddr)
		// Certs come from GetCertificate, so empty file args are correct.
		if err := httpsSrv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("preview-proxy https: %v", err)
		}
	}()

	if httpAddr != "" {
		httpSrv := &http.Server{Addr: httpAddr, Handler: h}
		servers = append(servers, httpSrv)
		go func() {
			logger.Info("preview-proxy listening (http)", "addr", httpAddr)
			if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("preview-proxy http: %v", err)
			}
		}()
	}

	return servers
}
