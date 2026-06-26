// internal/daemon/expose.go
package daemon

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
)

// ProxyHTTP returns a reverse proxy that forwards an HTTP request to the guest's
// 127.0.0.1:guestPort over a fresh PortForward tunnel, stripping prefix from the
// request path so the guest daemon sees the sub-path. FlushInterval is -1 so
// responses (including Server-Sent-Events) stream immediately with no buffering;
// keep-alives are disabled so each request uses its own tunnel and guest TCP
// connection, matching the per-connection tunnel model of ForwardPort. It fails
// fast for an unregistered sandbox or an out-of-range port. Bytes are never
// logged (secret hygiene).
func (api *SandboxAPI) ProxyHTTP(sandboxID string, guestPort int, prefix string) (*httputil.ReverseProxy, error) {
	if guestPort < 1 || guestPort > 65535 {
		return nil, fmt.Errorf("guest port %d out of range 1-65535", guestPort)
	}
	if err := api.checkSandboxRegistered(sandboxID); err != nil {
		return nil, err
	}
	api.mu.RLock()
	_, hasPath := api.streamPaths[sandboxID]
	api.mu.RUnlock()
	if !hasPath {
		return nil, fmt.Errorf("sandbox %s has no stream path; cannot proxy HTTP", sandboxID)
	}

	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		g := newVsockGuestConn(api, sandboxID)
		stream, err := g.PortForward(ctx, uint32(guestPort))
		if err != nil {
			return nil, fmt.Errorf("open guest port forward: %w", err)
		}
		raw := newPFConn(stream)
		// Track the conn so CloseExpose (called by UnregisterSandbox on terminate)
		// can close it and no tunnel goroutine outlives the sandbox.
		api.trackExposeConn(sandboxID, raw)
		tc := &trackingConn{
			Conn:    raw,
			untrack: func() { api.untrackExposeConn(sandboxID, raw) },
		}
		return tc, nil
	}

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = "guest" // ignored: DialContext returns the tunnel
			pr.Out.Host = "guest"
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, prefix)
			if pr.Out.URL.Path == "" {
				pr.Out.URL.Path = "/"
			}
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
		},
		Transport: &http.Transport{
			DialContext:       dial,
			DisableKeepAlives: true,
		},
		FlushInterval: -1, // immediate flush: SSE and long-lived streams
	}
	return rp, nil
}
