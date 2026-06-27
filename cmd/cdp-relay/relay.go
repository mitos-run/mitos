// cmd/cdp-relay/relay.go
package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
)

// rewriteDiscovery rewrites the upstream-loopback ws host in a CDP /json body
// to the external origin. upstreamHostPort is e.g. "127.0.0.1:9223".
// It replaces both the IP form (ws://127.0.0.1:PORT) and the localhost form
// (ws://localhost:PORT) that Chromium may emit. The SCHEME+HOST+PORT prefix is
// replaced; the /devtools/... path suffix is preserved.
func rewriteDiscovery(body []byte, upstreamHostPort, scheme, externalHost string) []byte {
	// Extract port for the localhost variant (ws://localhost:PORT).
	port := ""
	if idx := strings.LastIndex(upstreamHostPort, ":"); idx >= 0 {
		port = upstreamHostPort[idx+1:]
	}
	replacement := scheme + "://" + externalHost
	s := string(body)
	// Replace the IP form.
	s = strings.ReplaceAll(s, "ws://"+upstreamHostPort, replacement)
	s = strings.ReplaceAll(s, "wss://"+upstreamHostPort, replacement)
	// Replace the localhost form.
	if port != "" {
		s = strings.ReplaceAll(s, "ws://localhost:"+port, replacement)
		s = strings.ReplaceAll(s, "wss://localhost:"+port, replacement)
	}
	return []byte(s)
}

// newRelayHandler returns an http.Handler that reverse-proxies all traffic to
// the Chromium CDP endpoint at upstream (e.g. "127.0.0.1:9223"), setting
// Host: localhost on every upstream request so Chromium's DevTools host-header
// check passes. For /json* paths it rewrites webSocketDebuggerUrl and
// devtoolsFrontendUrl in the response body from the loopback address to the
// external origin (read from X-Forwarded-Host and X-Forwarded-Proto). WebSocket
// upgrade requests on /devtools/... are proxied transparently by
// httputil.ReverseProxy; the rewritten Host applies to the upgrade too.
func newRelayHandler(upstream string) http.Handler {
	target := &url.URL{
		Scheme: "http",
		Host:   upstream,
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// Chromium rejects any Host that is not an IP or "localhost".
			pr.Out.Host = "localhost"
			// The ReverseProxy strips incoming X-Forwarded-* headers before
			// calling Rewrite when the Rewrite hook is used. Re-propagate them
			// from pr.In (the original, unmodified request) so ModifyResponse
			// can read them from resp.Request.Header.
			if xfh := pr.In.Header.Get("X-Forwarded-Host"); xfh != "" {
				pr.Out.Header.Set("X-Forwarded-Host", xfh)
			}
			if xfp := pr.In.Header.Get("X-Forwarded-Proto"); xfp != "" {
				pr.Out.Header.Set("X-Forwarded-Proto", xfp)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			// Only rewrite /json* discovery endpoints; leave all other paths alone.
			if !strings.HasPrefix(resp.Request.URL.Path, "/json") {
				return nil
			}
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				return fmt.Errorf("read CDP discovery body: %w", err)
			}
			// Determine external ws scheme from forwarding headers preserved above.
			scheme := "ws"
			if resp.Request.Header.Get("X-Forwarded-Proto") == "https" {
				scheme = "wss"
			}
			externalHost := resp.Request.Header.Get("X-Forwarded-Host")
			if externalHost == "" {
				// Fallback: no forwarding header means the relay is accessed
				// directly. Use the upstream address so the body is at least
				// syntactically valid (local loopback relay mode).
				externalHost = upstream
			}
			rewritten := rewriteDiscovery(body, upstream, scheme, externalHost)
			resp.Body = io.NopCloser(bytes.NewReader(rewritten))
			resp.ContentLength = int64(len(rewritten))
			resp.Header.Set("Content-Length", strconv.Itoa(len(rewritten)))
			return nil
		},
		// FlushInterval is left at the default (0) so that httputil.ReverseProxy
		// can handle WebSocket upgrades transparently. A positive value would
		// buffer non-streaming responses; -1 would flush immediately after each
		// write. CDP WebSocket sessions use the upgrade path regardless, so the
		// default is correct.
	}
	return rp
}
