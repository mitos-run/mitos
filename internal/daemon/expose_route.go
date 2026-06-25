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

// handleExpose reverse-proxies GET/POST traffic to a guest port over the vsock
// tunnel. The path is /v1/sandboxes/{id}/expose/{port}/...; everything after the
// port is forwarded to the guest daemon. Auth is the per-sandbox bearer
// (checkBearer); the body-peeking requireBearer wrapper cannot front this route
// because it proxies arbitrary and streaming HTTP.
func (api *SandboxAPI) handleExpose(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	portStr := r.PathValue("port")
	if id == "" || portStr == "" {
		http.Error(w, "expose path must be /v1/sandboxes/{id}/expose/{port}/", http.StatusBadRequest)
		return
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		http.Error(w, "guest port must be an integer in 1-65535", http.StatusBadRequest)
		return
	}
	if !api.checkBearer(id, r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	prefix := "/v1/sandboxes/" + id + "/expose/" + portStr
	rp, err := api.ProxyHTTP(id, port, prefix)
	if err != nil {
		http.Error(w, "no route to guest port", http.StatusBadGateway)
		return
	}
	rp.ServeHTTP(w, r)
}
