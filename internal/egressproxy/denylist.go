package egressproxy

import (
	"context"
	"errors"
	"fmt"
	"net"
)

// ErrDestinationDenied is returned by the screening dial path when the target
// is, or resolves to, an address in the hard host-side denylist. The Proxy maps
// it to a 403 refusal (never a dial). It is distinct from a transport dial error
// (mapped to 502) so a denied destination is always reported as policy, not as a
// reachability failure.
var ErrDestinationDenied = errors.New("egress destination denied by policy")

// deniedNets is the hard host-side destination denylist enforced BEFORE any
// upstream dial, for both the CONNECT and the plain-HTTP path.
//
// WHY THIS EXISTS: the per-node egress proxy dials upstreams from the forkd HOST
// process (source = node IP, OUTPUT path), so the per-sandbox nftables egress
// chain (FORWARD hook, matched on the guest source IP, which carries the
// unconditional cloud-metadata drop) does NOT cover proxied traffic. Without
// this denylist a guest could `CONNECT 169.254.169.254:80` to steal the node's
// cloud IAM credentials (IMDS), or `CONNECT 127.0.0.1:9090` / `:9091` to reach
// forkd's own gRPC and sandbox API (SSRF). This denylist is the security floor
// that replaces the nft metadata block on the proxied path.
//
// deniedIP (below) also applies stdlib IP classifiers (IsUnspecified, IsLoopback,
// IsLinkLocalUnicast, IsLinkLocalMulticast) which cover classes not expressible
// as a single CIDR (e.g. 0.0.0.0 and :: are "unspecified" with no CIDR). The
// explicit CIDRs here cover ranges that fall outside those classifiers:
// fd00:ec2::254 (AWS IMDSv6 unicast within fd00::/8) and 64:ff9b::/96 (NAT64
// well-known prefix, covering 64:ff9b::a9fe:a9fe = NAT64 of 169.254.169.254).
//
// Per-sandbox ALLOWLIST (domain/SNI) policy on the proxied path is deliberately
// OUT OF SCOPE here and tracked as issue #494; this denylist is the floor, not
// the allowlist.
var deniedNets = func() []*net.IPNet {
	cidrs := []string{
		"169.254.0.0/16",    // IPv4 link-local incl. cloud metadata 169.254.169.254 (IMDS) and ECS task metadata
		"127.0.0.0/8",       // IPv4 loopback: forkd gRPC :9090 and sandbox API :9091 live here
		"::1/128",           // IPv6 loopback
		"fe80::/10",         // IPv6 link-local
		"fd00:ec2::254/128", // AWS IMDSv6
		"64:ff9b::/96",      // NAT64 well-known prefix; 64:ff9b::a9fe:a9fe = NAT64 of 169.254.169.254; matches nft RenderMetadataBlock coverage
		"::/96",             // deprecated IPv4-compatible block; ::a9fe:a9fe (::169.254.169.254) lives here; rarely routable but safe to deny
	}
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			// Static, compile-time-constant inputs: a parse error is a programming
			// bug, not a runtime condition.
			panic("egressproxy: invalid denylist CIDR " + c)
		}
		out = append(out, n)
	}
	return out
}()

// deniedIP reports whether ip falls within any hard-denied range. A nil ip is
// treated as denied (fail closed). IPv4-mapped IPv6 forms are normalized so an
// attacker cannot dodge the IPv4 ranges via the ::ffff: form.
//
// NOTE (documented best-effort residual): this does NOT enumerate the node's own
// non-loopback addresses, so a guest could in principle reach a service bound to
// the node's primary IP. A host nftables INPUT rule scoping the proxy port to the
// sandbox subnet is the recommended defense-in-depth complement (see the egress
// proxy row in docs/threat-model.md).
func deniedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	// Stdlib IP classifiers catch address classes not expressible as a single CIDR
	// and handle both IPv4 and IPv6 forms (including mapped forms) correctly:
	//   IsUnspecified: 0.0.0.0 and ::; Linux routes these to loopback, enabling
	//     SSRF to forkd (gRPC :9090, sandbox API :9091) via CONNECT 0.0.0.0:9090.
	//   IsLoopback: 127.0.0.0/8 and ::1 (defense in depth alongside deniedNets).
	//   IsLinkLocalUnicast: 169.254.0.0/16 and fe80::/10 (defense in depth).
	//   IsLinkLocalMulticast: 224.0.0.0/24 and ff02::/16.
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	for _, n := range deniedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ipResolver resolves a host name to its IP addresses. *net.Resolver satisfies
// it, and tests inject a stub so screening never touches real DNS.
type ipResolver interface {
	LookupIP(ctx context.Context, network, host string) ([]net.IP, error)
}

// screenDestination screens hostport against the hard denylist and returns the
// host:port to ACTUALLY dial. For an IP literal it screens the literal directly.
// For a name it resolves the name, rejects if ANY resolved A/AAAA is denied, and
// returns a vetted resolved IP as the dial target (an IP literal). Returning the
// IP, not the name, makes the dial DNS-rebinding-safe: the dialer cannot
// re-resolve the name to a different, denied address between the check and the
// dial. It returns ErrDestinationDenied when the literal or any resolved IP is
// denied.
func (p *Proxy) screenDestination(ctx context.Context, hostport string) (string, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return "", fmt.Errorf("split host:port: %w", err)
	}
	if ip := net.ParseIP(host); ip != nil {
		if deniedIP(ip) {
			return "", ErrDestinationDenied
		}
		return hostport, nil
	}
	ips, err := p.resolveIP.LookupIP(ctx, "ip", host)
	if err != nil {
		return "", fmt.Errorf("resolve host: %w", err)
	}
	if len(ips) == 0 {
		return "", fmt.Errorf("resolve host: no addresses returned")
	}
	// Reject the whole target if ANY resolved address is denied (a name pointing
	// at a denied IP, including the DNS-rebinding case), then dial a vetted IP.
	for _, ip := range ips {
		if deniedIP(ip) {
			return "", ErrDestinationDenied
		}
	}
	return net.JoinHostPort(ips[0].String(), port), nil
}

// dialUpstream screens the destination against the denylist, then dials the
// vetted target through the injected Dialer. All upstream connections flow
// through here so the denylist applies regardless of the injected Dialer.
func (p *Proxy) dialUpstream(ctx context.Context, hostport string) (net.Conn, error) {
	target, err := p.screenDestination(ctx, hostport)
	if err != nil {
		return nil, err
	}
	return p.dialer.Dial(ctx, target)
}
