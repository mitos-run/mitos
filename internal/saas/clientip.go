package saas

import (
	"context"
	"net"
	"net/http"
	"strings"
)

// clientIPKey is the unexported context key under which the gateway stashes the
// resolved client IP. The quota adapter reads it via ClientIPFromContext so the
// per-IP rate-limit bucket is charged against the trusted source address, never
// against a value the caller can set.
type clientIPKey struct{}

// TrustedProxyHops is the number of reverse-proxy hops in front of the gateway
// that the operator trusts to append a correct X-Forwarded-For entry. It is the
// gateway's XFF trust model, identical in spirit to the preview proxy: the raw
// connection RemoteAddr is the ONLY trusted source unless an explicit trusted
// hop count is configured, because an attacker can set any X-Forwarded-For value
// they like.
//
//   - 0 (the default): X-Forwarded-For is NOT trusted. The client IP is the
//     direct connection RemoteAddr. This is correct when the gateway terminates
//     client connections directly.
//   - n > 0: the gateway sits behind exactly n trusted proxies (for example one
//     ingress controller is 1). The client IP is taken n entries from the RIGHT
//     of the X-Forwarded-For list, since each trusted hop appends the address it
//     saw. Entries to the LEFT of that position are attacker-controlled and are
//     never used. If the list is shorter than n (a spoof attempt or a
//     misconfiguration), the gateway falls back to RemoteAddr, which fails
//     closed: a spoofed header cannot move the per-IP bucket off the real source.
type TrustedProxyHops int

// clientIP resolves the trusted client IP of a request under the configured
// trusted-proxy hop count. With zero trusted hops it returns the RemoteAddr host;
// with n trusted hops it returns the n-th-from-the-right X-Forwarded-For entry,
// falling back to RemoteAddr when the list is too short to trust. It never trusts
// an attacker-set leftmost XFF entry.
func clientIP(r *http.Request, hops TrustedProxyHops) string {
	remote := hostOnly(r.RemoteAddr)
	if hops <= 0 {
		return remote
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return remote
	}
	parts := strings.Split(xff, ",")
	// Each trusted hop appends the address it observed to the right of the list.
	// The address the OUTERMOST trusted proxy saw (the real client, when the chain
	// is fully trusted) is hops entries from the right. An index that would fall
	// off the left of the list means the list is shorter than the trusted chain,
	// so we fail closed to RemoteAddr rather than trusting an attacker-prepended
	// entry.
	idx := len(parts) - int(hops)
	if idx < 0 {
		return remote
	}
	cand := strings.TrimSpace(parts[idx])
	if ip := net.ParseIP(cand); ip != nil {
		return ip.String()
	}
	return remote
}

// hostOnly strips the port from a host:port address, returning the bare host. A
// bare IP (no port) is returned unchanged.
func hostOnly(addr string) string {
	if addr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// withClientIP returns a context carrying the resolved client IP so a downstream
// QuotaEnforcer (the quota.GatewayAdapter via its IPOf seam) can charge the
// per-IP rate-limit bucket against the trusted source address.
func withClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, clientIPKey{}, ip)
}

// ClientIPFromContext returns the client IP the gateway resolved for the request,
// or "" if none was set. The quota adapter passes this as its IPOf seam so the
// per-IP bucket is keyed on the trusted address. An empty result disables the
// per-IP bucket for that request (the per-org bucket still applies), which is the
// safe degradation: it never lets a spoofed header bypass a per-IP limit.
func ClientIPFromContext(ctx context.Context) string {
	ip, _ := ctx.Value(clientIPKey{}).(string)
	return ip
}
