package dnsproxy

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// Server is the controlled resolver. It answers DNS queries from sandboxes,
// resolving only names on the querying sandbox's allowlist and pinning each
// resolved address into that sandbox's dynamic nftables set before returning
// the answer. Names that are not allowlisted are refused; upstream failures are
// reported as SERVFAIL and pin nothing.
//
// Both A (IPv4) and AAAA (IPv6) are enforced: an allowed name's A records are
// pinned into the sandbox's v4 set and its AAAA records into the v6 set, each
// for the record's TTL. Any other query type is refused.
type Server struct {
	registry *Registry
	pinner   Pinner
	// upstreams are the host:port real resolvers allowed queries are forwarded
	// to, tried in order until one responds (for example 1.1.1.1:53 then
	// 8.8.8.8:53). Multiple independent resolvers keep name-based egress working
	// through a single resolver outage.
	upstreams []string
	// ttlFloor is the minimum pin lifetime. A record's TTL is raised to this
	// floor so a very short TTL does not expire the pin before the guest
	// connects.
	ttlFloor time.Duration
	// tapFor maps a guest source IP to its tap device name, used as the set key
	// when pinning.
	tapFor func(net.IP) string

	client *dns.Client
	logger *slog.Logger

	udp *dns.Server
	tcp *dns.Server
}

// NewServer builds a Server. logger may be nil, in which case a discarding
// logger is used.
func NewServer(registry *Registry, pinner Pinner, upstreams []string, ttlFloor time.Duration, tapFor func(net.IP) string, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	return &Server{
		registry:  registry,
		pinner:    pinner,
		upstreams: upstreams,
		ttlFloor:  ttlFloor,
		tapFor:    tapFor,
		// A bounded per-query timeout so a dead upstream fails fast and the next
		// one is tried, rather than hanging the guest's resolution.
		client: &dns.Client{Timeout: upstreamQueryTimeout},
		logger: logger,
	}
}

// upstreamQueryTimeout bounds each forward to a single upstream so failover to
// the next configured resolver is prompt.
const upstreamQueryTimeout = 3 * time.Second

// ParseUpstreams splits a comma-separated upstream list (for example
// "1.1.1.1:53,8.8.8.8:53") into trimmed, non-empty host:port entries. The order
// is preserved so the first entry is the primary resolver.
func ParseUpstreams(csv string) []string {
	var out []string
	for _, part := range strings.Split(csv, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ServeDNS implements dns.Handler. It is the whole enforcement path: attribute
// the query to a sandbox by source IP, check the allowlist, forward allowed
// queries upstream, pin the resolved addresses, and return the answer.
func (s *Server) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	if len(r.Question) == 0 {
		s.refuse(w, r)
		return
	}
	q := r.Question[0]
	clientIP := clientIPOf(w.RemoteAddr())
	if clientIP == nil {
		s.refuse(w, r)
		return
	}

	// Only A and AAAA are forwarded and pinned. Every other qtype is refused so
	// the resolver cannot be used as a covert tunnel.
	if q.Qtype != dns.TypeA && q.Qtype != dns.TypeAAAA {
		s.refuse(w, r)
		return
	}

	ports, ok := s.registry.Lookup(clientIP, q.Name)
	if !ok {
		// The name is not a secret; logging it aids debugging of egress denials.
		s.logger.Debug("dns refused: name not allowlisted",
			"guest", clientIP.String(), "name", q.Name)
		s.refuse(w, r)
		return
	}

	// Resolve the source's tap before forwarding. An empty tap means a
	// registry/allocator desync, or a query from an IP that has already been
	// released: there is no set to pin into, so any answer would be unreachable
	// for the guest. Refuse rather than forward upstream and pin a bogus set.
	tap := s.tapFor(clientIP)
	if tap == "" {
		s.logger.Debug("dns refused: source guest has no tap mapping",
			"guest", clientIP.String(), "name", q.Name)
		s.refuse(w, r)
		return
	}

	var resp *dns.Msg
	var lastErr error
	for _, up := range s.upstreams {
		r2, _, err := s.client.Exchange(r, up)
		if err == nil && r2 != nil {
			resp = r2
			break
		}
		lastErr = err
		s.logger.Debug("dns upstream failure, trying next",
			"guest", clientIP.String(), "name", q.Name, "upstream", up, "err", err)
	}
	if resp == nil {
		s.logger.Debug("all dns upstreams failed",
			"guest", clientIP.String(), "name", q.Name, "err", lastErr)
		m := new(dns.Msg)
		m.SetRcode(r, dns.RcodeServerFailure)
		_ = w.WriteMsg(m)
		return
	}

	kept := make([]dns.RR, 0, len(resp.Answer))
	for _, ans := range resp.Answer {
		// Pin each A and AAAA answer. The pinner routes a v4 address into the v4
		// set and a v6 address into the v6 set so the element type matches.
		var addr net.IP
		switch rr := ans.(type) {
		case *dns.A:
			addr = rr.A
		case *dns.AAAA:
			addr = rr.AAAA
		default:
			// Preserve non-address records (e.g. CNAME) in the response chain.
			kept = append(kept, ans)
			continue
		}
		// Refuse to pin a non-publicly-routable address: an allowlisted name whose
		// authoritative DNS an attacker influences could otherwise be steered at
		// internal cluster services, node-local, or other private targets (DNS
		// rebinding to internal). Strip it from the response too so the guest
		// neither learns nor (since it is not pinned, the default-deny chain drops
		// it) can reach it. The nft metadata block covers IMDS; this is the
		// resolver-side complement for the broader private ranges.
		if isBlockedPinAddr(addr) {
			s.logger.Warn("refusing to pin non-public resolved address (possible DNS rebinding to internal)",
				"guest", clientIP.String(), "name", q.Name, "addr", addr.String())
			continue
		}
		ttl := time.Duration(ans.Header().Ttl) * time.Second
		if ttl < s.ttlFloor {
			ttl = s.ttlFloor
		}
		for _, port := range ports {
			if perr := s.pinner.Pin(clientIP, tap, addr, port, ttl); perr != nil {
				s.logger.Warn("dns pin failed",
					"guest", clientIP.String(), "name", q.Name, "port", port, "err", perr)
			}
		}
		kept = append(kept, ans)
	}
	resp.Answer = kept

	if werr := w.WriteMsg(resp); werr != nil {
		s.logger.Debug("dns write failed", "guest", clientIP.String(), "err", werr)
	}
}

// cgnatNet is the RFC6598 shared address space (100.64.0.0/10), used by some
// Kubernetes and cloud networks and not covered by net.IP.IsPrivate.
var cgnatNet = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// isBlockedPinAddr reports whether a resolved address must NOT be pinned into a
// guest's egress allow set (and must be stripped from the answer). It blocks
// every non-globally-routable range: RFC1918 and IPv6 ULA (net.IP.IsPrivate),
// loopback, link-local uni/multicast, multicast, the unspecified address, and
// RFC6598 CGNAT. This stops an allowlisted name whose DNS an attacker controls
// from being rebound at internal cluster services, node-local addresses, or
// other private targets. Globally-routable addresses (the legitimate egress
// targets) are unaffected.
func isBlockedPinAddr(ip net.IP) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() ||
		ip.IsPrivate() {
		return true
	}
	if v4 := ip.To4(); v4 != nil && cgnatNet.Contains(v4) {
		return true
	}
	return false
}

func (s *Server) refuse(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetRcode(r, dns.RcodeRefused)
	_ = w.WriteMsg(m)
}

// ListenAndServe starts the resolver on addr for both UDP and TCP. It blocks
// until one of the listeners fails or Shutdown is called, returning the first
// error.
func (s *Server) ListenAndServe(addr string) error {
	s.udp = &dns.Server{Addr: addr, Net: "udp", Handler: s}
	s.tcp = &dns.Server{Addr: addr, Net: "tcp", Handler: s}

	errCh := make(chan error, 2)
	go func() { errCh <- s.udp.ListenAndServe() }()
	go func() { errCh <- s.tcp.ListenAndServe() }()
	return <-errCh
}

// Shutdown stops both listeners. It is safe to call before ListenAndServe has
// fully started; nil listeners are skipped.
func (s *Server) Shutdown(ctx context.Context) error {
	var firstErr error
	if s.udp != nil {
		if err := s.udp.ShutdownContext(ctx); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("shutdown udp resolver: %w", err)
		}
	}
	if s.tcp != nil {
		if err := s.tcp.ShutdownContext(ctx); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("shutdown tcp resolver: %w", err)
		}
	}
	return firstErr
}

// clientIPOf extracts the IP from a DNS client's remote address (UDP or TCP).
func clientIPOf(addr net.Addr) net.IP {
	switch a := addr.(type) {
	case *net.UDPAddr:
		return a.IP
	case *net.TCPAddr:
		return a.IP
	default:
		host, _, err := net.SplitHostPort(addr.String())
		if err != nil {
			return nil
		}
		return net.ParseIP(host)
	}
}

// discard is an io.Writer that drops everything, used for the default logger.
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
