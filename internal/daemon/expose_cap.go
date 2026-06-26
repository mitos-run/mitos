// internal/daemon/expose_cap.go
package daemon

import (
	"net"
	"sync"
)

// defaultMaxExpose is the per-sandbox ceiling on concurrent OPEN expose tunnels.
// It mirrors defaultMaxForwards (16) for the same resource-exhaustion reason:
// each expose request opens a vsock PortForward tunnel, and an unbounded count
// would exhaust host vsock connections and goroutines. Set via
// SetMaxExposePerSandbox; n<=0 disables the cap.
const defaultMaxExpose = 16

// SetMaxExposePerSandbox sets the per-sandbox ceiling on concurrent OPEN expose
// tunnels (authenticated guest HTTP proxy). A NEW tunnel opened while a sandbox
// is already at the cap is rejected with 429; existing tunnels are never killed.
// n<=0 disables the cap (unbounded). Must be called before the API serves
// requests; the field is not synchronized.
func (api *SandboxAPI) SetMaxExposePerSandbox(n int) {
	api.maxExpose = n
}

// acquireExpose reserves one concurrent-expose slot for sandboxID, enforcing the
// per-sandbox cap. It returns a release func and true when admitted; the caller
// MUST call release exactly once (defer) when the tunnel closes. It returns
// (nil, false) when the sandbox is already at the cap, in which case the caller
// must reject the request with 429 and never call release. The cap is checked
// here, at tunnel OPEN, before the vsock PortForward is dialed; it is a single
// map lookup under mu. maxExpose<=0 disables the cap.
func (api *SandboxAPI) acquireExpose(sandboxID string) (release func(), ok bool) {
	api.mu.Lock()
	if api.maxExpose > 0 && api.openExpose[sandboxID] >= api.maxExpose {
		api.mu.Unlock()
		return nil, false
	}
	api.openExpose[sandboxID]++
	api.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			api.mu.Lock()
			if n := api.openExpose[sandboxID] - 1; n > 0 {
				api.openExpose[sandboxID] = n
			} else {
				delete(api.openExpose, sandboxID)
			}
			api.mu.Unlock()
		})
	}, true
}

// trackExposeConn registers c as a live expose conn for sandboxID so that
// CloseExpose can close it when the sandbox terminates. Called from ProxyHTTP's
// dial closure after newPFConn is created. Safe to call concurrently.
func (api *SandboxAPI) trackExposeConn(sandboxID string, c net.Conn) {
	api.mu.Lock()
	if api.exposeConns[sandboxID] == nil {
		api.exposeConns[sandboxID] = make(map[net.Conn]struct{})
	}
	api.exposeConns[sandboxID][c] = struct{}{}
	api.mu.Unlock()
}

// untrackExposeConn removes c from the expose tracking set for sandboxID.
// Called when the tunnel conn is closed (via the wrapping trackingConn or by
// CloseExpose). Safe to call after CloseExpose has already cleared the entry
// (no-op in that case).
func (api *SandboxAPI) untrackExposeConn(sandboxID string, c net.Conn) {
	api.mu.Lock()
	if api.exposeConns[sandboxID] != nil {
		delete(api.exposeConns[sandboxID], c)
		if len(api.exposeConns[sandboxID]) == 0 {
			delete(api.exposeConns, sandboxID)
		}
	}
	api.mu.Unlock()
}

// CloseExpose closes every tracked expose conn for sandboxID and clears the
// tracking entry. Called by UnregisterSandbox so no vsock tunnel goroutine
// outlives a terminated sandbox. Safe to call for a sandbox with no tracked
// conns.
func (api *SandboxAPI) CloseExpose(sandboxID string) {
	api.mu.Lock()
	conns := api.exposeConns[sandboxID]
	delete(api.exposeConns, sandboxID)
	api.mu.Unlock()

	for c := range conns {
		_ = c.Close()
	}
}

// trackingConn wraps a net.Conn and calls untrack on Close (once) so the conn
// is removed from the exposeConns tracking map when the tunnel tears down
// naturally. This prevents the map from growing across request lifetimes.
type trackingConn struct {
	net.Conn
	once    sync.Once
	untrack func()
}

func (tc *trackingConn) Close() error {
	err := tc.Conn.Close()
	tc.once.Do(tc.untrack)
	return err
}
