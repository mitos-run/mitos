package preview

import (
	"strings"
	"sync"
)

// ParseHost extracts the single leftmost label from an expose hostname of the
// form <label>.<domain>. It strips any ":port", lowercases (DNS is case
// insensitive), and requires exactly one label to the left of the base domain
// (no embedded dots), so a multi-label host is rejected. The label is an opaque
// routing key; the caller resolves it against the route table. ok is false for
// any host that does not match, so the proxy can reject unknown vhosts.
func ParseHost(host, domain string) (label string, ok bool) {
	host = strings.ToLower(strings.TrimSpace(host))
	domain = strings.ToLower(strings.TrimSpace(domain))
	if host == "" || domain == "" {
		return "", false
	}
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	suffix := "." + domain
	if !strings.HasSuffix(host, suffix) {
		return "", false
	}
	label = host[:len(host)-len(suffix)]
	if label == "" || strings.Contains(label, ".") {
		return "", false
	}
	return label, true
}

// reservedLabels are hostnames a tenant may never take: control-plane and
// well-known names that would enable phishing or interception if served as an
// untrusted app. The set is the proxy's defensive backstop; the controller also
// rejects them at registration time (slice 2b).
var reservedLabels = map[string]struct{}{
	"www": {}, "app": {}, "api": {}, "console": {}, "gateway": {},
	"admin": {}, "auth": {}, "login": {}, "account": {}, "mail": {},
	"static": {}, "assets": {}, "cdn": {}, "status": {},
}

// IsReservedLabel reports whether label is reserved and must not route to a
// tenant app.
func IsReservedLabel(label string) bool {
	_, ok := reservedLabels[strings.ToLower(label)]
	return ok
}

// Route is a single expose backend entry: the opaque subdomain label, the
// sandbox it serves, the owning forkd node HTTP endpoint (host:port of :9091),
// the guest port, the per-sandbox bearer the proxy presents to forkd, and the
// access tier. Token is a secret and is never logged.
type Route struct {
	Label        string
	SandboxID    string
	NodeEndpoint string // forkd :9091 host:port (Sandbox.Status.Endpoint)
	Port         int    // guest port
	Token        string // per-sandbox bearer
	Sharing      string // access tier; slice 2a routes "link"
}

// ClaimState is the injectable view a route-sync source maps onto. The slice-2b
// controller reconciler maps a Ready Sandbox (Status.Phase==Ready,
// Status.Endpoint, the <name>-sandbox-token Secret) onto this shape.
type ClaimState struct {
	Label        string
	SandboxID    string
	NodeEndpoint string
	Port         int
	Token        string
	Sharing      string
	Ready        bool
}

// RouteTable is the concurrency-safe map of label to backend route the proxy
// consults on every request. Entries are added when a sandbox is Ready and
// GC'd when it terminates (leaves the Ready set).
type RouteTable struct {
	mu     sync.RWMutex
	routes map[string]Route
}

// NewRouteTable returns an empty RouteTable.
func NewRouteTable() *RouteTable {
	return &RouteTable{routes: make(map[string]Route)}
}

// Upsert inserts or replaces the route for r.Label.
func (t *RouteTable) Upsert(r Route) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.routes[r.Label] = r
}

// Remove deletes the route for label if present (GC on terminate).
func (t *RouteTable) Remove(label string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.routes, label)
}

// Lookup returns the route for label.
func (t *RouteTable) Lookup(label string) (Route, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	r, ok := t.routes[label]
	return r, ok
}

// Len returns the number of routed labels.
func (t *RouteTable) Len() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.routes)
}

// Sync reconciles the table to exactly the Ready claims in states: it upserts a
// route for every Ready claim with a non-empty NodeEndpoint and removes any
// existing route whose label is no longer in the Ready set. This is the GC
// pass: a terminated sandbox drops out of the Ready set and its route is reaped.
func (t *RouteTable) Sync(states []ClaimState) {
	want := make(map[string]Route, len(states))
	for _, c := range states {
		if !c.Ready || c.NodeEndpoint == "" {
			continue
		}
		want[c.Label] = Route{
			Label:        c.Label,
			SandboxID:    c.SandboxID,
			NodeEndpoint: c.NodeEndpoint,
			Port:         c.Port,
			Token:        c.Token,
			Sharing:      c.Sharing,
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	// Remove routes no longer wanted.
	for lbl := range t.routes {
		if _, ok := want[lbl]; !ok {
			delete(t.routes, lbl)
		}
	}
	// Add or update wanted routes.
	for lbl, r := range want {
		t.routes[lbl] = r
	}
}
