package egressproxy

import (
	"net"
	"sync"
)

// Registry maps a sandbox guest IP (the source address of its proxied
// connections) to the sandbox ID that owns it. It is the SandboxResolver the
// Proxy attributes each connection with. It is safe for concurrent use: the
// proxy reads it on every accepted connection while the daemon registers and
// deregisters sandboxes. It mirrors dnsproxy.Registry's guest-IP-keyed,
// RWMutex-guarded shape.
type Registry struct {
	mu      sync.RWMutex
	byGuest map[string]string
}

// NewRegistry returns an empty Registry ready for use.
func NewRegistry() *Registry {
	return &Registry{byGuest: make(map[string]string)}
}

// Register records that a guest IP belongs to sandboxID. Registering the same
// guest IP again replaces the prior owner, so a reused guest IP never inherits a
// stale attribution.
func (r *Registry) Register(guestIP net.IP, sandboxID string) {
	r.mu.Lock()
	r.byGuest[guestIP.String()] = sandboxID
	r.mu.Unlock()
}

// Deregister removes a guest IP's attribution. Subsequent Lookups for that guest
// return ok=false, so the proxy refuses connections sourced from it.
func (r *Registry) Deregister(guestIP net.IP) {
	r.mu.Lock()
	delete(r.byGuest, guestIP.String())
	r.mu.Unlock()
}

// Lookup returns the sandbox ID that owns a guest source IP. ok is false when
// the guest is not registered.
func (r *Registry) Lookup(srcIP net.IP) (sandboxID string, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, found := r.byGuest[srcIP.String()]
	return id, found
}
