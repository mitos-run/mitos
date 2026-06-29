// Command frontdoor is the Mitos hosted-launch front-door reverse proxy. It
// routes requests to the marketing or console upstream depending on the path,
// resolves mitos_session cookies into account/org identity, injects
// X-Mitos-Account and X-Mitos-Org headers for authenticated requests, and
// redirects unauthenticated users to /login when required.
//
// Configuration is entirely via environment variables; no CLI flags are used so
// that secrets never appear in process listings.
//
//	MITOS_FRONTDOOR_ADDR                      listen address (default ":8080")
//	MITOS_FRONTDOOR_MARKETING_URL             base URL of the marketing upstream (required)
//	MITOS_FRONTDOOR_MARKETING_PAGES_ADDRS     comma-separated GitHub Pages IP:port list;
//	                                          when set, DNS for the marketing host is bypassed
//	                                          and one of these addresses is dialed instead.
//	                                          Example: "185.199.108.153:443,185.199.109.153:443"
//	MITOS_FRONTDOOR_CONSOLE_URL               base URL of the console upstream (required)
//	MITOS_FRONTDOOR_SESSION_RESOLVE_URL        URL of POST /internal/session/resolve (required)
//	MITOS_IDENTITY_RESOLVE_TOKEN              bearer token for the session-resolve endpoint (required)
package main

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"mitos.run/mitos/internal/frontdoor"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	addr := os.Getenv("MITOS_FRONTDOOR_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	marketingRaw := os.Getenv("MITOS_FRONTDOOR_MARKETING_URL")
	if marketingRaw == "" {
		log.Fatal("frontdoor: MITOS_FRONTDOOR_MARKETING_URL is required")
	}
	consoleRaw := os.Getenv("MITOS_FRONTDOOR_CONSOLE_URL")
	if consoleRaw == "" {
		log.Fatal("frontdoor: MITOS_FRONTDOOR_CONSOLE_URL is required")
	}
	resolveURL := os.Getenv("MITOS_FRONTDOOR_SESSION_RESOLVE_URL")
	if resolveURL == "" {
		log.Fatal("frontdoor: MITOS_FRONTDOOR_SESSION_RESOLVE_URL is required")
	}
	// The bearer token is a secret and must not be logged.
	resolveToken := os.Getenv("MITOS_IDENTITY_RESOLVE_TOKEN")
	if resolveToken == "" {
		log.Fatal("frontdoor: MITOS_IDENTITY_RESOLVE_TOKEN is required")
	}

	if _, err := url.Parse(marketingRaw); err != nil {
		log.Fatalf("frontdoor: invalid MITOS_FRONTDOOR_MARKETING_URL: %v", err)
	}
	if _, err := url.Parse(consoleRaw); err != nil {
		log.Fatalf("frontdoor: invalid MITOS_FRONTDOOR_CONSOLE_URL: %v", err)
	}
	if _, err := url.Parse(resolveURL); err != nil {
		log.Fatalf("frontdoor: invalid MITOS_FRONTDOOR_SESSION_RESOLVE_URL: %v", err)
	}

	// Parse MITOS_FRONTDOOR_MARKETING_PAGES_ADDRS (optional). When present the
	// marketing proxy bypasses DNS and dials one of these IPs directly, keeping
	// TLS SNI and Host as the marketing URL host. The values are not secrets
	// (they are public GitHub Pages IPs) so logging the count is acceptable.
	var marketingPagesAddrs []string
	if raw := os.Getenv("MITOS_FRONTDOOR_MARKETING_PAGES_ADDRS"); raw != "" {
		for _, a := range strings.Split(raw, ",") {
			if a = strings.TrimSpace(a); a != "" {
				marketingPagesAddrs = append(marketingPagesAddrs, a)
			}
		}
	}

	resolver := frontdoor.NewHTTPSessionResolver(resolveURL, resolveToken, nil)

	proxy, err := frontdoor.NewProxy(frontdoor.ProxyConfig{
		MarketingURL:        marketingRaw,
		MarketingPagesAddrs: marketingPagesAddrs,
		ConsoleURL:          consoleRaw,
		Resolver:            resolver,
		Logger:              logger,
	})
	if err != nil {
		log.Fatalf("frontdoor: build proxy: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("/", proxy)

	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Log startup with URLs but never the token.
	logger.Info("frontdoor starting",
		"addr", addr,
		"marketing_url", marketingRaw,
		"marketing_pages_addrs_count", len(marketingPagesAddrs),
		"console_url", consoleRaw,
		"resolve_url", resolveURL,
	)

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("frontdoor: listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	<-stop
	logger.Info("frontdoor shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
