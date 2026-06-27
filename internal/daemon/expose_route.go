// internal/daemon/expose_route.go
package daemon

import (
	"crypto/subtle"
	"net/http"
	"strconv"
	"strings"
)

// checkBearer validates the per-sandbox bearer token on a request that is
// mounted outside the body-peeking requireBearer wrapper (the expose proxy and
// other streaming routes). It mirrors requireBearer's contract: fail closed when
// a token is registered and the presented bearer does not match in constant
// time; allow when AllowTokenless was set and no token is registered (standalone
// loopback). Token values are never logged.
func (api *SandboxAPI) checkBearer(sandboxID string, r *http.Request) bool {
	id := api.resolveSandboxID(sandboxID)
	api.mu.RLock()
	token, hasToken := api.tokens[id]
	api.mu.RUnlock()

	if !hasToken {
		return api.allowTokenless
	}
	presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(token)) == 1
}

// HandleExpose is the exported entry point so the standalone sandbox-server
// (a separate package) can mount the guest HTTP proxy route. It is identical to
// the route forkd mounts internally via handleExpose.
func (api *SandboxAPI) HandleExpose(w http.ResponseWriter, r *http.Request) {
	api.handleExpose(w, r)
}

// handleExpose reverse-proxies traffic to a guest port over the vsock tunnel. It
// proxies arbitrary HTTP methods (GET for SSE and browser traffic, POST and the
// rest for agent calls); it is registered without a method restriction. The path
// is /v1/sandboxes/{id}/expose/{port}/...; everything after the port is forwarded
// to the guest daemon. Auth is the per-sandbox bearer (checkBearer); the
// body-peeking requireBearer wrapper cannot front this route because it proxies
// arbitrary and streaming HTTP.
func (api *SandboxAPI) handleExpose(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	portStr := r.PathValue("port")
	if id == "" || portStr == "" {
		http.Error(w, "expose path must be /v1/sandboxes/{id}/expose/{port}/", http.StatusBadRequest)
		return
	}
	// Auth runs before port validation so an unauthenticated caller with a
	// malformed port receives 401, not 400 (defense-in-depth: do not reveal
	// port validity to unauthenticated callers).
	if !api.checkBearer(id, r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		http.Error(w, "guest port must be an integer in 1-65535", http.StatusBadRequest)
		return
	}

	// Enforce the per-sandbox expose concurrency cap BEFORE opening the tunnel.
	// A NEW request over the cap is rejected 429; existing tunnels are never
	// killed. acquireExpose returns (nil, false) when the cap is hit.
	release, ok := api.acquireExpose(id)
	if !ok {
		http.Error(w, "too many concurrent expose connections", http.StatusTooManyRequests)
		return
	}
	defer release()

	prefix := "/v1/sandboxes/" + id + "/expose/" + portStr
	// Resolve the requested id to the id the API operates on before proxying. In a
	// husk pod (singleSandbox mode) the cluster addresses the sandbox by its
	// cluster id while the pod registered it under the husk's single id; exec and
	// the bearer check already resolve, so the expose port-forward must too or
	// ProxyHTTP's registration lookup misses and every request 502s with
	// "no route to guest port".
	rp, err := api.ProxyHTTP(api.resolveSandboxID(id), port, prefix)
	if err != nil {
		http.Error(w, "no route to guest port", http.StatusBadGateway)
		return
	}
	rp.ServeHTTP(w, r)
}
