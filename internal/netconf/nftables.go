package netconf

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	v1 "mitos.run/mitos/api/v1"
)

// HostPort is a destination IP and TCP port from an egress allowlist.
type HostPort struct {
	IP   net.IP
	Port int
}

// SharedTableName returns the single shared nftables table that holds every
// sandbox's egress rules. All sandboxes share ONE table and ONE base chain so
// that adding or removing one sandbox never disturbs another's traffic.
func SharedTableName() string {
	return "mitos_egress"
}

// BaseChainName returns the single base chain hooked on the forward path. It
// has policy accept and only dispatches by interface into per-sandbox regular
// chains; it never drops, so non-sandbox host forwarding is unaffected.
func BaseChainName() string {
	return "forward"
}

// NatTableName returns the nft ip-family table that holds the per-pod source
// NAT for the guest subnet. It is separate from the inet filter table so the
// masquerade rule can never disturb the egress filter chains.
func NatTableName() string {
	return "mitos_nat"
}

// RenderMasquerade renders the nft ruleset that source-NATs the guest's traffic
// to the pod's address as it leaves the pod network namespace. The husk VM's
// source is a private /30 (huskGuestIP) that is unroutable beyond the tap, so
// without this SNAT every allowed egress connection sends but never receives
// return traffic. The rule is scoped to the guest source IP so only VM traffic
// is masqueraded, and the postrouting chain is flushed before the rule is
// (re)added so re-delivery is idempotent. Pairs with IPv4 forwarding enabled in
// the pod netns (the kernel will not route tap to uplink otherwise).
func RenderMasquerade(guestIP net.IP) string {
	table := NatTableName()
	var b strings.Builder
	fmt.Fprintf(&b, "add table ip %s\n", table)
	fmt.Fprintf(&b, "add chain ip %s postrouting { type nat hook postrouting priority 100 ; policy accept ; }\n", table)
	fmt.Fprintf(&b, "flush chain ip %s postrouting\n", table)
	fmt.Fprintf(&b, "add rule ip %s postrouting ip saddr %s masquerade\n", table, guestIP.String())
	return b.String()
}

// RenderMasqueradeDelete renders the teardown of the per-pod NAT table. The
// table is added before being deleted so the delete is idempotent (nft errors
// on deleting an absent table).
func RenderMasqueradeDelete() string {
	table := NatTableName()
	return fmt.Sprintf("add table ip %s\ndelete table ip %s\n", table, table)
}

// MetadataAddrs are the cloud instance-metadata endpoints that are an
// UNCONDITIONAL hard drop on every sandbox chain, even under EgressAllow: the
// IPv4 IMDS address (shared by AWS, GCP metadata.google.internal, and Azure
// IMDS), the IPv4 link-local /16 (covers ECS task metadata 169.254.170.2 and
// any other link-local metadata service), and the IPv6 IMDS address. Reaching
// these lets a guest steal the node's cloud IAM credentials, which is never an
// intended egress; the allowlist cannot override it because RenderMetadataBlock
// is emitted BEFORE any allow rule in the chain.
const (
	metadataV4Addr = "169.254.169.254"
	metadataV4Net  = "169.254.0.0/16"
	metadataV6Addr = "fd00:ec2::254"
)

// RenderMetadataBlock renders the unconditional cloud-metadata drops for one
// sandbox chain. The v4 drops are saddr-pinned to the guest as defense in depth
// (same anti-spoof posture as every other rule); the v6 drop is family-scoped.
// The caller MUST emit this block before any allow rule so the allowlist can
// never reach a metadata endpoint.
func RenderMetadataBlock(table, chain string, guestIP net.IP) string {
	saddr := fmt.Sprintf("ip saddr %s", guestIP.String())
	var b strings.Builder
	fmt.Fprintf(&b, "add rule inet %s %s %s ip daddr %s drop\n", table, chain, saddr, metadataV4Addr)
	fmt.Fprintf(&b, "add rule inet %s %s %s ip daddr %s drop\n", table, chain, saddr, metadataV4Net)
	fmt.Fprintf(&b, "add rule inet %s %s ip6 daddr %s drop\n", table, chain, metadataV6Addr)
	return b.String()
}

// DispatchMapName returns the verdict map keyed by interface name. The base
// chain looks up the inbound interface (the tap) in this map and jumps to that
// sandbox's regular chain. Adding/removing a sandbox is a single map element
// add/delete by key, with no rule handles to track.
func DispatchMapName() string {
	return "tapdispatch"
}

// InputBaseChainName returns the base chain hooked on the INPUT path. The
// forward chain governs traffic transiting the pod (guest to the internet); the
// input chain governs traffic the guest sends to an address that is LOCAL to the
// pod network namespace (the tap gateway, the resolver address, any in-pod
// listener). Forward-only filtering never sees that traffic, so without this a
// guest could reach pod-local listeners (the husk-stub sandbox API and mTLS
// control) regardless of egress policy.
func InputBaseChainName() string {
	return "input"
}

// InputDispatchMapName returns the interface-keyed verdict map the input base
// chain dispatches through, the input-path analog of DispatchMapName. It is
// distinct so the input and forward dispatch never collide.
func InputDispatchMapName() string {
	return "tapdispatch_in"
}

// SandboxInputChainName returns the per-sandbox regular chain on the input path
// for a tap. It holds the guest's only allowed pod-local destination (the
// resolver on 53) and a drop for everything else, reached only via the input
// dispatch jump for this tap.
func SandboxInputChainName(tap string) string {
	return "sbin_" + tap
}

// SandboxChainName returns the per-sandbox regular chain name for a tap. The
// chain holds that sandbox's accepts and a final drop; because it is reached
// only via the dispatch jump for this tap, its drop is a verdict for this
// sandbox's packets only and cannot affect other sandboxes.
func SandboxChainName(tap string) string {
	return "sb_" + tap
}

// proxyDNATBaseChainName returns the nat-table prerouting base chain that
// dispatches per-tap sentinel DNAT through a verdict map. It mirrors
// BaseChainName on the inet forward path: a single hooked base chain that holds
// no per-tap state and only dispatches by inbound interface into per-tap chains,
// so adding or removing one fork never disturbs another's DNAT.
func proxyDNATBaseChainName() string {
	return "prerouting"
}

// ProxyDNATDispatchMapName returns the ifname-keyed verdict map the prerouting
// base chain dispatches through into per-tap DNAT chains. It is the nat-path
// analog of DispatchMapName: adding or removing a fork's DNAT is a single map
// element add/delete by tap key, with no rule handles to track.
func ProxyDNATDispatchMapName() string {
	return "proxydnat"
}

// ProxyDNATChainName returns the per-tap regular nat chain holding that fork's
// sentinel DNAT rule. It is reached only via the proxy DNAT dispatch jump for
// its tap, mirroring SandboxChainName on the inet path, so teardown can remove
// it by name; this stops a reused tap from inheriting a stale DNAT and keeps the
// prerouting dispatch from growing unbounded.
func ProxyDNATChainName(tap string) string {
	return "proxydnat_" + tap
}

// RenderProxyDNAT redirects the fork-stable sentinel proxy address to THIS
// fork's gateway, where the per-node egress proxy listens. The sentinel value
// is baked identically into every fork's HTTP_PROXY; the per-tap DNAT is what
// makes it route to this fork's own proxy context. All values are addresses,
// safe to log.
//
// It mirrors the inet forward dispatch idiom (RenderSharedTable plus a per-tap
// chain): the ip nat table, the hooked prerouting base chain, and the
// ifname-keyed dispatch map are created idempotently with `add` (a no-op if they
// already exist), the base chain is flushed only to re-add its single dispatch
// rule (the base chain holds no per-tap state), and this fork's OWN regular DNAT
// chain plus the dispatch element keyed by its tap are added. Because the DNAT
// lives in a per-tap chain reached by a map element, teardown removes it by tap
// key and name, so it never leaks and tap reuse is clean.
func RenderProxyDNAT(tap string, sentinel net.IP, proxyPort int, gatewayIP net.IP) string {
	table := NatTableName()
	base := proxyDNATBaseChainName()
	dispatch := ProxyDNATDispatchMapName()
	chain := ProxyDNATChainName(tap)

	var b strings.Builder
	fmt.Fprintf(&b, "add table ip %s\n", table)
	fmt.Fprintf(&b, "add chain ip %s %s { type nat hook prerouting priority -100 ; policy accept ; }\n", table, base)
	fmt.Fprintf(&b, "add map ip %s %s { type ifname : verdict ; }\n", table, dispatch)
	// Flush only the base chain's rules before re-adding the single dispatch rule
	// (the base chain holds no per-tap state; that lives in the map and the per-tap
	// chains), so re-applying this skeleton on each fork stays idempotent. Mirrors
	// RenderSharedTable.
	fmt.Fprintf(&b, "flush chain ip %s %s\n", table, base)
	fmt.Fprintf(&b, "add rule ip %s %s iifname vmap @%s\n", table, base, dispatch)
	// This fork's own DNAT chain, the rule that redirects the fork-stable sentinel
	// to this fork's gateway, and the dispatch element routing this tap into it.
	fmt.Fprintf(&b, "add chain ip %s %s\n", table, chain)
	fmt.Fprintf(&b, "add rule ip %s %s ip daddr %s tcp dport %d dnat to %s:%d\n",
		table, chain, sentinel, proxyPort, gatewayIP, proxyPort)
	fmt.Fprintf(&b, "add element ip %s %s { %q : jump %s }\n", table, dispatch, tap, chain)
	return b.String()
}

// RenderProxyAccept allows the guest to reach the per-node proxy listener on
// the gateway address, ahead of the allowlist. The proxy enforces upstream
// egress policy; the rule here just opens the path to the proxy listener so
// the guest can connect before the per-sandbox chain's final verdict.
func RenderProxyAccept(table, chain string, guestIP, gatewayIP net.IP, proxyPort int) string {
	return fmt.Sprintf("add rule inet %s %s ip saddr %s ip daddr %s tcp dport %d accept\n",
		table, chain, guestIP, gatewayIP, proxyPort)
}

// SandboxAllowSetName returns the per-sandbox dynamic allow set name for a tap.
// The set holds (ipv4_addr . inet_service) elements with a timeout flag and is
// populated at runtime by the DNS proxy as it resolves allowlisted names. The
// per-sandbox chain accepts traffic whose (daddr . dport) is present in this
// set, so a resolved name's address is reachable only until its TTL expires.
// It is named the same way as SandboxChainName so a tap's chain and set share a
// stable, collision-free identity.
func SandboxAllowSetName(tap string) string {
	return "sb_" + tap + "_dyn"
}

// SandboxAllowSet6Name returns the per-sandbox dynamic IPv6 allow set name for a
// tap. It mirrors SandboxAllowSetName but holds (ipv6_addr . inet_service)
// elements: the DNS proxy pins resolved AAAA addresses here so a name's IPv6
// address is reachable for its TTL, just as A addresses are pinned into the v4
// set. It is named distinctly from the v4 set so the two coexist in one chain.
func SandboxAllowSet6Name(tap string) string {
	return "sb_" + tap + "_dyn6"
}

// ParseAllowEntry parses a single allowlist entry of the form host:port. When
// host is a literal IPv4 address it returns the HostPort and isName=false.
// When host is a DNS name it returns isName=true (these cannot be enforced in
// PR1 without a controlled resolver, so the renderer omits them). A malformed
// entry (missing port, bad port, empty host) returns an error.
func ParseAllowEntry(s string) (hp HostPort, isName bool, err error) {
	host, portStr, splitErr := net.SplitHostPort(s)
	if splitErr != nil {
		return HostPort{}, false, fmt.Errorf("parse allow entry %q: %w", s, splitErr)
	}
	if host == "" {
		return HostPort{}, false, fmt.Errorf("parse allow entry %q: empty host", s)
	}
	port, perr := strconv.Atoi(portStr)
	if perr != nil {
		return HostPort{}, false, fmt.Errorf("parse allow entry %q: invalid port: %w", s, perr)
	}
	if port < 1 || port > 65535 {
		return HostPort{}, false, fmt.Errorf("parse allow entry %q: port %d out of range", s, port)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// A DNS name: parsed but not enforceable in PR1.
		return HostPort{}, true, nil
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return HostPort{}, false, fmt.Errorf("parse allow entry %q: only IPv4 destinations are supported", s)
	}
	return HostPort{IP: ip4, Port: port}, false, nil
}

// SplitAllowList parses a raw allowlist (e.g. NetworkPolicy.Allow) into the
// enforceable IP:port HostPorts and the list of skipped name-based entries
// (returned verbatim so forkd can log a clear warning). A malformed entry
// fails the whole call.
func SplitAllowList(entries []string) (enforceable []HostPort, skipped []string, err error) {
	for _, e := range entries {
		hp, isName, perr := ParseAllowEntry(e)
		if perr != nil {
			return nil, nil, perr
		}
		if isName {
			skipped = append(skipped, e)
			continue
		}
		enforceable = append(enforceable, hp)
	}
	return enforceable, skipped, nil
}

// ParseNameAllowList parses a raw allowlist into the name-based entries the DNS
// proxy enforces: a map from a lowercased DNS name to the sorted, de-duplicated
// set of TCP ports allowed for it. IP:port entries are ignored (they are
// enforced statically by the chain, not by the resolver). A malformed entry
// fails the whole call. The result is the map the dnsproxy Registry.Register
// takes; an empty map (no name entries) means the sandbox has no name egress.
//
// A wildcard name is permitted but is the egress boundary, so it is validated
// here: it must be exactly a single leading "*." followed by a valid domain.
// "*", "*.", "*foo.com", "a.*.com", "**.com", and any name with more than one
// "*" are REJECTED rather than silently treated as a literal name. A rejected
// wildcard fails the whole call with a clear error.
func ParseNameAllowList(entries []string) (map[string][]int, error) {
	names := make(map[string][]int)
	seen := make(map[string]map[int]bool)
	for _, e := range entries {
		host, portStr, splitErr := net.SplitHostPort(e)
		if splitErr != nil {
			return nil, fmt.Errorf("parse allow entry %q: %w", e, splitErr)
		}
		if host == "" {
			return nil, fmt.Errorf("parse allow entry %q: empty host", e)
		}
		port, perr := strconv.Atoi(portStr)
		if perr != nil {
			return nil, fmt.Errorf("parse allow entry %q: invalid port: %w", e, perr)
		}
		if port < 1 || port > 65535 {
			return nil, fmt.Errorf("parse allow entry %q: port %d out of range", e, port)
		}
		if net.ParseIP(host) != nil {
			// An IP:port entry: enforced statically by the chain, not the resolver.
			continue
		}
		if verr := validateNameAllowEntry(host); verr != nil {
			return nil, fmt.Errorf("parse allow entry %q: %w", e, verr)
		}
		key := strings.ToLower(strings.TrimSuffix(host, "."))
		if seen[key] == nil {
			seen[key] = make(map[int]bool)
		}
		if !seen[key][port] {
			seen[key][port] = true
			names[key] = append(names[key], port)
		}
	}
	for _, ports := range names {
		sort.Ints(ports)
	}
	return names, nil
}

// validateNameAllowEntry validates a DNS-name allow entry (host part, no port)
// at the egress boundary. A wildcard is the security-critical case: it must be
// exactly a single leading "*." plus a valid domain. The check runs on the
// trailing-dot-stripped host; a "*" anywhere except as the entire first label
// is rejected. A non-wildcard name must be a valid domain.
func validateNameAllowEntry(host string) error {
	name := strings.TrimSuffix(host, ".")
	if name == "" {
		return fmt.Errorf("empty name")
	}
	if strings.HasPrefix(name, "*.") {
		domain := strings.TrimPrefix(name, "*.")
		if strings.Contains(domain, "*") {
			return fmt.Errorf("wildcard must be a single leading %q label", "*.")
		}
		if !isValidDomain(domain) {
			return fmt.Errorf("wildcard %q must be %q followed by a valid domain", name, "*.")
		}
		return nil
	}
	if strings.Contains(name, "*") {
		return fmt.Errorf("a wildcard must be exactly a single leading %q label, got %q", "*.", name)
	}
	if !isValidDomain(name) {
		return fmt.Errorf("invalid domain %q", name)
	}
	return nil
}

// isValidDomain reports whether s is a syntactically valid DNS domain: at least
// two dot-separated labels, each label non-empty, no embedded "*", and only
// letters, digits, and hyphens with no leading or trailing hyphen per label.
// This is a deliberately conservative syntax check, not a registry lookup.
func isValidDomain(s string) bool {
	if s == "" {
		return false
	}
	labels := strings.Split(s, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if !isValidLabel(label) {
			return false
		}
	}
	return true
}

// isValidLabel reports whether label is a valid DNS label: non-empty, no longer
// than 63 octets, alphanumeric or hyphen, and not starting or ending with a
// hyphen.
func isValidLabel(label string) bool {
	if label == "" || len(label) > 63 {
		return false
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '-':
		default:
			return false
		}
	}
	return true
}

// RenderSharedTable renders the idempotent skeleton every sandbox shares: one
// `inet` table, one base chain hooked on the forward path with policy ACCEPT,
// and an empty interface-keyed verdict map the base chain dispatches through.
//
// All statements are `add` of named objects, which nftables treats as
// idempotent: re-applying this against an existing table is a no-op and does
// NOT flush the table or its chains, so a second sandbox's Setup never
// disturbs the first sandbox's chain or dispatch element.
//
// The base chain never drops. Its policy is accept so unrelated host
// forwarding passes; sandbox isolation is enforced entirely by each sandbox's
// own regular chain (rendered by RenderSandboxChain), which ends in drop and
// is reached only via the per-tap dispatch jump. This is the fix for the
// cross-fork drop: there is no shared policy-drop base chain whose drop would
// be terminal for every tap on the forward hook.
func RenderSharedTable() string {
	table := SharedTableName()
	base := BaseChainName()
	dispatch := DispatchMapName()

	var b strings.Builder
	fmt.Fprintf(&b, "add table inet %s\n", table)
	fmt.Fprintf(&b, "add chain inet %s %s { type filter hook forward priority 0 ; policy accept ; }\n", table, base)
	fmt.Fprintf(&b, "add map inet %s %s { type ifname : verdict ; }\n", table, dispatch)
	// The single dispatch rule sends traffic whose inbound interface (the tap)
	// is a known sandbox tap into that sandbox's regular chain. Interfaces not
	// in the map fall through to the accept policy untouched. Adding the same
	// rule twice would duplicate it, but a duplicate dispatch lookup is inert
	// (the map is the single source of truth), and Setup only applies this
	// skeleton before each sandbox add; the rule body is fixed so nft collapses
	// it. To stay strictly idempotent we flush only this base chain's rules
	// before re-adding the single dispatch rule, which is safe because the base
	// chain holds no per-sandbox state (all of that lives in the map and the
	// per-sandbox chains).
	fmt.Fprintf(&b, "flush chain inet %s %s\n", table, base)
	fmt.Fprintf(&b, "add rule inet %s %s iifname vmap @%s\n", table, base, dispatch)
	return b.String()
}

// RenderSharedInputTable renders the idempotent skeleton for the INPUT path: the
// shared table (same table as the forward filter), an input-hooked base chain
// with policy accept, and an interface-keyed verdict map the base chain
// dispatches through. Like RenderSharedTable every statement is an idempotent
// `add` of a named object, so re-applying it never disturbs an existing input
// chain or dispatch element.
//
// The base chain's policy is ACCEPT so non-sandbox input (kubelet probes to the
// pod, the controller's mTLS dial to the husk control port, which arrive on the
// pod uplink, not the tap) is unaffected; guest isolation on the input path is
// enforced entirely by each tap's own regular chain (RenderSandboxInputChain),
// reached only via the per-tap dispatch jump. It is applied only on the husk
// path, where the filter lives in the isolated pod network namespace; the
// raw-forkd path runs in the node netns and does not install an input hook.
func RenderSharedInputTable() string {
	table := SharedTableName()
	base := InputBaseChainName()
	dispatch := InputDispatchMapName()

	var b strings.Builder
	fmt.Fprintf(&b, "add table inet %s\n", table)
	fmt.Fprintf(&b, "add chain inet %s %s { type filter hook input priority 0 ; policy accept ; }\n", table, base)
	fmt.Fprintf(&b, "add map inet %s %s { type ifname : verdict ; }\n", table, dispatch)
	// Flush only this base chain's rules before re-adding the single dispatch
	// rule (the base chain holds no per-tap state; that lives in the map and the
	// per-tap chains), so the skeleton stays strictly idempotent.
	fmt.Fprintf(&b, "flush chain inet %s %s\n", table, base)
	fmt.Fprintf(&b, "add rule inet %s %s iifname vmap @%s\n", table, base, dispatch)
	return b.String()
}

// RenderSandboxInputChain renders the per-tap input chain `sbin_<tap>` and the
// input dispatch element routing this tap into it. Applied with `nft -f -` after
// RenderSharedInputTable on the husk path.
//
// The input hook processes packets the guest sends to a pod-LOCAL address. The
// only legitimate such destination is the in-pod DNS resolver on port 53; every
// other pod-local destination (the tap gateway, the husk-stub sandbox API, the
// mTLS control listener) is dropped. Accepts are emitted before the drop so DNS
// keeps working, and are saddr-pinned to the guest as anti-spoof defense in
// depth (consistent with the forward chain). Because the chain is reached only
// through this tap's dispatch jump and the input hook implies a local
// destination, the trailing drop denies guest-to-pod-local for this sandbox only.
func RenderSandboxInputChain(tap string, guestIP net.IP, resolverIP net.IP) string {
	return RenderSandboxInputChainSpec(InputChainSpec{
		Tap:        tap,
		GuestIP:    guestIP,
		ResolverIP: resolverIP,
		Inbound:    v1.InboundDeny,
	})
}

// RenderSandboxChain renders the add block for ONE sandbox: its regular chain
// `sb_<tap>` (no hook, no policy) plus the dispatch map element routing this
// tap into it. The block is applied with `nft -f -` after RenderSharedTable.
//
// The chain accepts established/related connections, each allowlisted
// destination IP:port, and DNS (udp/tcp 53) to resolverIP only, then ends in a
// terminal drop (EgressDeny) or accept (EgressAllow). Every accept keys on
// `ip saddr <guestIP>` as defense in depth: even though only this tap reaches
// the chain via the dispatch jump, the saddr check stops a guest from
// spoofing another sandbox's source address on its own tap.
//
// Because the drop/accept verdict is reached only through the per-tap jump, it
// applies to this sandbox's packets alone and cannot terminate another
// sandbox's allowed traffic. The output is deterministic for the same inputs.
func RenderSandboxChain(tap string, guestIP net.IP, policy v1.EgressPolicy, allow []HostPort, resolverIP net.IP) string {
	return RenderSandboxChainSpec(ChainSpec{
		Tap:        tap,
		GuestIP:    guestIP,
		Egress:     policy,
		Allow:      allow,
		ResolverIP: resolverIP,
	})
}
