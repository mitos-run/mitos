package saas

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"mitos.run/mitos/internal/apierr"
)

// execWSPath is the Connect Exec RPC path the interactive PTY upgrades on. The
// daemon serves the bidi sandbox.v1.Sandbox.Exec RPC over a WebSocket here
// (internal/daemon/exec_ws.go), reading the sandbox id from the ?sandbox= query
// and the per-sandbox token from the Authorization header.
const execWSPath = "/sandbox.v1.Sandbox/Exec"

// isWebSocketUpgrade reports whether r is a WebSocket upgrade handshake: a GET
// with Connection naming "upgrade" and Upgrade: websocket. The Connection header
// is a comma list, so it is matched token-wise and case-insensitively.
func isWebSocketUpgrade(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	for _, tok := range strings.Split(r.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
			return true
		}
	}
	return false
}

// runtimeSandboxID resolves the target sandbox id for a runtime ws upgrade. The
// native PTY client (sdk pty.ts) identifies the sandbox via the ?sandbox= query;
// the X-Sandbox-Id header is also honored and wins when both are set. The id is
// only used to look the sandbox up org-scoped, never to bypass that check.
func runtimeSandboxID(r *http.Request) string {
	if id := r.Header.Get("X-Sandbox-Id"); id != "" {
		return id
	}
	return r.URL.Query().Get("sandbox")
}

// proxyRuntimeWebSocket proxies an authenticated, org-scoped PTY WebSocket to the
// owning sandbox. Auth, scope, and quota are already enforced by ServeHTTP. The
// org-scoped endpoint and per-sandbox token are resolved through the
// RuntimeResolver seam (a cross-org id collapses to not_found), then the request
// is reverse-proxied with the Upgrade preserved so httputil.ReverseProxy hijacks
// the connection and pipes the WebSocket bidirectionally. The customer key is
// NEVER forwarded to the sandbox: the per-sandbox token replaces it.
func (g *Gateway) proxyRuntimeWebSocket(ctx context.Context, w http.ResponseWriter, r *http.Request, orgID string) {
	resolver, ok := g.cp.(RuntimeResolver)
	if !ok {
		g.fail(w, apierr.Get(apierr.CodeInternal).
			WithCause("the control plane does not support websocket runtime proxying"))
		return
	}

	id := runtimeSandboxID(r)
	if id == "" {
		g.fail(w, apierr.Get(apierr.CodeNotFound).
			WithCause("no sandbox id was supplied; set the X-Sandbox-Id header or the ?sandbox= query"))
		return
	}

	target, aerr := resolver.ResolveRuntime(ctx, orgID, id)
	if aerr != nil {
		g.fail(w, *aerr)
		return
	}

	g.log.Info("gateway ws proxy", "org", orgID, "sandbox", target.SandboxID)

	// failed is set by the ErrorHandler so a backend failure (already counted
	// through g.fail) is not double-counted; a proxy that returns without a
	// backend error completed the 101 upgrade and is counted as 1xx.
	var failed bool
	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = target.Endpoint
			req.URL.Path = execWSPath
			// The daemon ws Exec endpoint identifies the sandbox by the ?sandbox=
			// query; carry the control-plane-resolved name (never the client value).
			req.URL.RawQuery = url.Values{"sandbox": {target.SandboxID}}.Encode()
			req.Host = target.Endpoint
			// Replace the customer credential with the per-sandbox token. The client
			// Authorization (the customer key) is NEVER presented to the sandbox.
			req.Header.Set("Authorization", "Bearer "+target.Token)
			req.Header.Set("X-Sandbox-Id", target.SandboxID)
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			// A backend dial/handshake failure must not leak internals. Map it to a
			// generic bad_gateway-style envelope; the token is never in err here.
			g.log.Info("gateway ws proxy backend error", "org", orgID, "sandbox", target.SandboxID, "err", err.Error())
			failed = true
			g.fail(w, apierr.Get(apierr.CodeInternal).
				WithCause("the sandbox runtime endpoint could not be reached"))
		},
		ErrorLog: slog.NewLogLogger(g.log.Handler(), slog.LevelInfo),
	}
	rp.ServeHTTP(w, r)
	if !failed {
		g.metrics.observeStatus(http.StatusSwitchingProtocols)
	}
}
