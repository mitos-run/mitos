// Command preview-proxy is the per-sandbox preview URL reverse proxy (issue
// #126). One entrypoint fronts many ephemeral per-sandbox backends: it parses
// <sandbox-id>.preview.<domain>, verifies a signed expiring preview token plus
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
// Production gate: this proxy adds a public ingress surface and is NOT cleared
// for production tenants until the external security review (issue #194) covers
// it. See docs/threat-model.md.
package main

import (
	"context"
	"crypto/tls"
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
)

func main() {
	addr := flag.String("addr", ":8443", "HTTPS listen address")
	httpAddr := flag.String("http-addr", "", "optional plaintext HTTP listen address (testing / behind a TLS terminator)")
	domain := flag.String("domain", "", "base preview domain; routes <sandbox-id>.preview.<domain>")
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

	routes := preview.NewRouteTable()
	proxy := preview.NewProxy(preview.Config{
		Domain: *domain,
		Signer: signer,
		Routes: routes,
		Logger: logger,
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

	certProvider, err := preview.NewSelfSignedProvider()
	if err != nil {
		log.Fatalf("preview-proxy: cert provider: %v", err)
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
		Addr:    httpsAddr,
		Handler: h,
		TLSConfig: &tls.Config{
			GetCertificate: cp.GetCertificate,
			MinVersion:     tls.VersionTLS12,
		},
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
