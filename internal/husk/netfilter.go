package husk

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/dnsproxy"
	"github.com/paperclipinc/mitos/internal/netconf"
)

// NetfilterConfig is the in-pod egress filter configuration for one husk VM.
// The husk-stub owns exactly one tap in the husk pod's network namespace, so
// the single-VM-per-pod case is the per-tap chain from the raw-forkd dataplane
// applied once. All fields are config (no secrets) and safe to log.
type NetfilterConfig struct {
	// Tap is the host-side tap device name in the pod netns the VM's NIC is
	// bound to. It is the dispatch key and the per-sandbox chain suffix.
	Tap string
	// GuestIP is the VM's source address; every accept and the metadata drop are
	// saddr-pinned to it as anti-spoof defense in depth.
	GuestIP net.IP
	// HostIP is the pod-side address of the point-to-point link assigned to the
	// tap (the VM's gateway).
	HostIP net.IP
	// Egress is the template's policy; deny is the fail-closed default verdict.
	Egress v1alpha1.EgressPolicy
	// Allow is the raw allowlist; IP:port entries become static chain accepts,
	// name entries are enforced by the DNS proxy (handled by the caller).
	Allow []string
	// ResolverIP is the in-pod DNS proxy address the chain allows on port 53 and
	// the guest is pointed at. Nil disables the DNS allow rule (IP-only mode).
	ResolverIP net.IP
}

// netfilterRunner executes one host command with optional stdin in the pod
// netns. It is injected so the orchestration is unit-testable without root; the
// production stub wires it to an exec-based runner.
type netfilterRunner func(ctx context.Context, argv []string, stdin string) error

// applyEgressFilter brings up the VM's tap and installs its default-deny egress
// chain (with the unconditional metadata block) in the husk pod netns. It is
// the single-VM-per-pod analog of internal/network.setup: create the tap,
// assign the host IP, bring it up, apply the idempotent shared table, then this
// VM's per-tap chain. A malformed allowlist fails the whole call (fail-closed:
// a VM never comes up with a half-applied filter).
func applyEgressFilter(ctx context.Context, run netfilterRunner, enableForwarding func() error, cfg NetfilterConfig) error {
	enforceable, _, err := netconf.SplitAllowList(cfg.Allow)
	if err != nil {
		return fmt.Errorf("husk netfilter: parse allowlist: %w", err)
	}
	// IPv4 forwarding in the pod netns: the kernel will not route the guest /30
	// between the tap and the pod uplink without it, so the SNAT below would have
	// nothing to NAT. Nil seam (tests) skips it. Done first so a failure aborts
	// before any tap/chain state is created (fail-closed: no half-open datapath).
	if enableForwarding != nil {
		if err := enableForwarding(); err != nil {
			return fmt.Errorf("husk netfilter: enable ipv4 forwarding: %w", err)
		}
	}
	if err := run(ctx, netconf.TapAddArgs(cfg.Tap), ""); err != nil {
		return fmt.Errorf("husk netfilter: create tap %s: %w", cfg.Tap, err)
	}
	if err := run(ctx, netconf.AddrAddArgs(cfg.HostIP, cfg.Tap), ""); err != nil {
		return fmt.Errorf("husk netfilter: assign host ip to tap %s: %w", cfg.Tap, err)
	}
	if err := run(ctx, netconf.LinkUpArgs(cfg.Tap), ""); err != nil {
		return fmt.Errorf("husk netfilter: bring tap %s up: %w", cfg.Tap, err)
	}
	// Bind the in-pod DNS resolver address to the tap so the per-pod DNS proxy can
	// listen on it and the guest's queries (sent to it via the tap gateway) are
	// delivered locally instead of being forwarded out (now that ip_forward is on).
	if cfg.ResolverIP != nil {
		if err := run(ctx, netconf.ResolverAddrAddArgs(cfg.ResolverIP, cfg.Tap), ""); err != nil {
			return fmt.Errorf("husk netfilter: bind resolver %s to tap %s: %w", cfg.ResolverIP, cfg.Tap, err)
		}
	}
	if err := run(ctx, netconf.NftApplyArgs(), netconf.RenderSharedTable()); err != nil {
		return fmt.Errorf("husk netfilter: apply shared egress table: %w", err)
	}
	chain := netconf.RenderSandboxChain(cfg.Tap, cfg.GuestIP, cfg.Egress, enforceable, cfg.ResolverIP)
	if err := run(ctx, netconf.NftApplyArgs(), chain); err != nil {
		return fmt.Errorf("husk netfilter: apply egress chain for tap %s: %w", cfg.Tap, err)
	}
	// Source-NAT the guest's allowed egress to the pod address so return traffic
	// for an allowed connection can find its way back; without it the private /30
	// source is unroutable beyond the tap and every allowed connection hangs.
	if err := run(ctx, netconf.NftApplyArgs(), netconf.RenderMasquerade(cfg.GuestIP)); err != nil {
		return fmt.Errorf("husk netfilter: apply masquerade for guest %s: %w", cfg.GuestIP, err)
	}
	// Input-path guard: the forward chain above governs transit traffic, but a
	// packet the guest sends to a pod-LOCAL address (the tap gateway, the resolver,
	// the husk-stub sandbox API and mTLS control listeners) is delivered on the
	// input hook, which the forward chain never sees. Install the input base chain
	// and this tap's input chain so the guest can reach ONLY the resolver on 53 and
	// nothing else pod-local, regardless of egress policy.
	if err := run(ctx, netconf.NftApplyArgs(), netconf.RenderSharedInputTable()); err != nil {
		return fmt.Errorf("husk netfilter: apply shared input table: %w", err)
	}
	if err := run(ctx, netconf.NftApplyArgs(), netconf.RenderSandboxInputChain(cfg.Tap, cfg.GuestIP, cfg.ResolverIP)); err != nil {
		return fmt.Errorf("husk netfilter: apply input chain for tap %s: %w", cfg.Tap, err)
	}
	return nil
}

// teardownEgressFilter removes this VM's tap and per-tap egress state. It is
// best-effort and returns the first error so a partial teardown does not leak
// the rest. Called by the stub on Close.
func teardownEgressFilter(ctx context.Context, run netfilterRunner, tap string) error {
	var firstErr error
	if err := run(ctx, netconf.LinkDelArgs(tap), ""); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("husk netfilter: delete tap %s: %w", tap, err)
	}
	if err := run(ctx, netconf.NftDeleteDispatchElementArgs(tap), ""); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("husk netfilter: delete dispatch element for tap %s: %w", tap, err)
	}
	if err := run(ctx, netconf.NftDeleteSandboxChainArgs(tap), ""); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("husk netfilter: delete chain for tap %s: %w", tap, err)
	}
	if err := run(ctx, netconf.NftDeleteSandboxAllowSetArgs(tap), ""); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("husk netfilter: delete allow set for tap %s: %w", tap, err)
	}
	if err := run(ctx, netconf.NftDeleteInputDispatchElementArgs(tap), ""); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("husk netfilter: delete input dispatch element for tap %s: %w", tap, err)
	}
	if err := run(ctx, netconf.NftDeleteSandboxInputChainArgs(tap), ""); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("husk netfilter: delete input chain for tap %s: %w", tap, err)
	}
	if err := run(ctx, netconf.NftApplyArgs(), netconf.RenderMasqueradeDelete()); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("husk netfilter: delete masquerade table: %w", err)
	}
	return firstErr
}

// buildEgressDNSRegistry parses the name entries from the allowlist and returns
// a dnsproxy.Registry with this VM's guest IP registered for them, plus the raw
// name->ports map (returned for logging/assertion). IP:port entries are ignored
// here (the chain enforces them statically). An invalid name or wildcard fails
// the whole call (fail-closed: a bad allowlist never yields a partially
// enforced resolver). An empty name set is valid: the proxy still runs and
// resolves nothing, which is the documented IP-only allowlist mode.
func buildEgressDNSRegistry(guestIP string, allow []string) (*dnsproxy.Registry, map[string][]int, error) {
	names, err := netconf.ParseNameAllowList(allow)
	if err != nil {
		return nil, nil, fmt.Errorf("husk netfilter: parse name allowlist: %w", err)
	}
	reg := dnsproxy.NewRegistry()
	ip := net.ParseIP(guestIP)
	if ip == nil {
		return nil, nil, fmt.Errorf("husk netfilter: invalid guest ip %q", guestIP)
	}
	reg.Register(ip, names)
	return reg, names, nil
}

// newEgressDNSProxy builds the per-pod DNS proxy: it resolves only registered
// names and pins each resolved address into THIS tap's dynamic allow set via an
// nft pinner, the same model raw-forkd uses (cmd/forkd buildDNSProxy). tap is
// fixed (one VM per pod), so tapFor always returns it. upstreams are the real
// resolvers the proxy forwards allowed queries to, tried in order. The returned
// server is started by the caller with ListenAndServe on the resolver address.
func newEgressDNSProxy(reg *dnsproxy.Registry, tap string, upstreams []string, run func(argv []string) error) *dnsproxy.Server {
	pinner := dnsproxy.NewNftPinner(run)
	tapFor := func(net.IP) string { return tap }
	return dnsproxy.NewServer(reg, pinner, upstreams, dnsProxyTTLFloor, tapFor, nil)
}

// dnsProxyTTLFloor matches the raw-forkd proxy's TTL floor so a pinned address
// lives at least this long even when the record's TTL is shorter.
const dnsProxyTTLFloor = 30 * time.Second
