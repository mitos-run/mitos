package network

import (
	"context"
	"fmt"
	"net"

	"mitos.run/mitos/internal/netconf"
)

// runner executes one host command with the given argv and optional stdin. It
// is the injected seam that makes the orchestration below testable without
// root: tests pass a recording runner, the Linux Manager passes a real
// exec-based runner. stdin is fed to the process when non-empty (used for
// `nft -f -`).
type runner func(ctx context.Context, argv []string, stdin string) error

// applyOptions captures the optional, documented host-level behaviors that
// the orchestration may perform in addition to the per-tap setup.
type applyOptions struct {
	// subnetCIDR is the sandbox subnet used for the optional MASQUERADE rule.
	subnetCIDR string
	// uplink is the host egress interface for the optional MASQUERADE rule.
	// When uplink is empty, no MASQUERADE is added and the node's existing
	// NAT is relied upon.
	uplink string
	// enableForwarding, when true, writes 1 to /proc/sys/net/ipv4/ip_forward
	// before creating the tap. The write is performed by the caller-provided
	// forwardEnabler so this stays platform-independent and testable.
	enableForwarding bool
	// proxyEnabled mirrors the node-wide egress proxy flag. When set, every
	// sandbox gets a per-tap prerouting DNAT in setup, so teardown must remove
	// that tap's DNAT chain and dispatch element. It is a node-level setting (the
	// proxy is per-node, not per-sandbox), so gating teardown on it keeps
	// non-proxy nodes from attempting deletes of nat objects that never existed.
	proxyEnabled bool
}

// forwardEnabler enables host IPv4 forwarding. It is injected so the
// orchestration test can assert it is invoked without touching /proc.
type forwardEnabler func() error

// setup runs the ordered host commands to bring up a sandbox's network:
// optionally enable IP forwarding, create the tap, assign the host IP, bring
// the link up, apply the rendered nftables ruleset on stdin, and optionally
// add a MASQUERADE rule. The command order is fixed and asserted by tests.
func setup(
	ctx context.Context,
	run runner,
	enableForward forwardEnabler,
	id netconf.Identity,
	policy netconf.SandboxPolicy,
	resolverIP net.IP,
	opts applyOptions,
) error {
	if opts.enableForwarding {
		if err := enableForward(); err != nil {
			return fmt.Errorf("enable ip forwarding: %w", err)
		}
	}

	// Remove any tap of this name left behind by a build or fork that died before its
	// teardown ran. Creating over it fails with `ioctl(TUNSETIFF): Device or resource
	// busy`, which masks the real first-attempt error and wedges the caller forever
	// (the husk egress path has done this since #428). Best effort: "not found" is the
	// normal case. Tap names are unique per identity, so a tap of this name is a leak,
	// never a live peer: a template build is protected from racing itself by
	// Engine.CreateTemplate's in-flight guard, and sandbox taps are per-sandbox.
	_ = run(ctx, netconf.LinkDelArgs(id.TapName), "")
	if err := run(ctx, netconf.TapAddArgs(id.TapName), ""); err != nil {
		return fmt.Errorf("create tap %s: %w", id.TapName, err)
	}
	if err := run(ctx, netconf.AddrAddArgs(id.HostIP, id.TapName), ""); err != nil {
		return fmt.Errorf("assign host ip to tap %s: %w", id.TapName, err)
	}
	if err := run(ctx, netconf.LinkUpArgs(id.TapName), ""); err != nil {
		return fmt.Errorf("bring tap %s up: %w", id.TapName, err)
	}

	// Install the idempotent shared table/base chain/dispatch map first, then
	// this sandbox's own regular chain and dispatch element. Reapplying the
	// shared skeleton is a no-op when it already exists and never flushes
	// another sandbox's chain, so a second sandbox's Setup cannot drop the
	// first sandbox's traffic.
	if err := run(ctx, netconf.NftApplyArgs(), netconf.RenderSharedTable()); err != nil {
		return fmt.Errorf("apply shared egress table for tap %s: %w", id.TapName, err)
	}
	cidrV4, cidrV6, err := netconf.ParseCIDRList(policy.AllowCIDRs)
	if err != nil {
		return fmt.Errorf("parse CIDR allowlist for tap %s: %w", id.TapName, err)
	}
	spec := netconf.ChainSpec{
		Tap:          id.TapName,
		GuestIP:      id.GuestIP,
		Egress:       policy.Egress,
		Allow:        policy.Allow,
		AllowCIDRsV4: cidrV4,
		AllowCIDRsV6: cidrV6,
		ResolverIP:   resolverIP,
		BlockNetwork: policy.BlockNetwork,
		Counter:      policy.Counter,
	}
	// Per-sandbox egress proxy: fold the proxy accept rule INTO this chain so the
	// guest can reach the per-node proxy listener (the proxy, not the per-sandbox
	// chain, enforces upstream egress policy). The renderer places the accept
	// AFTER the unconditional cloud-metadata drops and ahead of the allowlist
	// verdict, so it can never precede the IMDS drops, and saddr-pins it to the
	// guest IP and the gateway:proxyPort destination so it never broadens reach.
	//
	// SCOPE CAVEAT (M4): this chain is the FORWARD-hook per-sandbox chain. The
	// proxy DNAT below rewrites the sentinel destination to the gateway IP, which
	// is LOCAL to forkd's netns, so the DNATed packet is delivered via the INPUT
	// path, not FORWARD. This forward accept rule therefore does NOT gate the
	// proxied packet; on raw-forkd the path works because the INPUT base chain
	// policy is accept (the rule is defense-in-depth/documentation, not the load
	// bearing accept). The proxied datapath is consequently a raw-forkd path; a
	// husk pod with an INPUT default-deny would need an explicit INPUT accept for
	// the gateway:proxyPort, which is a separate concern from this rule.
	if policy.ProxySentinel != nil {
		spec.ProxyGatewayIP = id.HostIP
		spec.ProxyPort = policy.ProxyPort
	}
	chain := netconf.RenderSandboxChainSpec(spec)
	if err := run(ctx, netconf.NftApplyArgs(), chain); err != nil {
		return fmt.Errorf("apply egress chain for tap %s: %w", id.TapName, err)
	}

	// Install the prerouting DNAT that redirects this fork's sentinel proxy
	// address to its gateway, where the single per-node proxy process listens.
	// The sentinel value is baked identically into every fork; the per-tap DNAT
	// is what routes it to this fork's own proxy context.
	if policy.ProxySentinel != nil {
		dnat := netconf.RenderProxyDNAT(id.TapName, policy.ProxySentinel, policy.ProxyPort, id.HostIP)
		if err := run(ctx, netconf.NftApplyArgs(), dnat); err != nil {
			return fmt.Errorf("apply proxy DNAT for tap %s: %w", id.TapName, err)
		}
	}

	if opts.uplink != "" {
		if err := run(ctx, netconf.MasqueradeAddArgs(opts.subnetCIDR, opts.uplink), ""); err != nil {
			return fmt.Errorf("add masquerade for %s on %s: %w", opts.subnetCIDR, opts.uplink, err)
		}
	}
	return nil
}

// teardown runs the ordered host commands to remove a sandbox's network:
// delete the tap (which also removes its addresses), remove this sandbox's
// dispatch element from the shared verdict map, and delete its per-sandbox
// chain. The shared table, base chain, and map are left intact because other
// sandboxes may still use them. Teardown is best-effort: it attempts every
// step and returns the first error so a partial failure does not leak the
// other resources.
func teardown(ctx context.Context, run runner, id netconf.Identity, opts applyOptions) error {
	var firstErr error

	if opts.uplink != "" {
		if err := run(ctx, netconf.MasqueradeDelArgs(opts.subnetCIDR, opts.uplink), ""); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("delete masquerade for %s on %s: %w", opts.subnetCIDR, opts.uplink, err)
		}
	}
	if err := run(ctx, netconf.LinkDelArgs(id.TapName), ""); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("delete tap %s: %w", id.TapName, err)
	}
	// Remove the dispatch element before the chain: while the element exists it
	// references the chain, so the chain delete would be refused.
	if err := run(ctx, netconf.NftDeleteDispatchElementArgs(id.TapName), ""); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("delete egress dispatch element for tap %s: %w", id.TapName, err)
	}
	if err := run(ctx, netconf.NftDeleteSandboxChainArgs(id.TapName), ""); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("delete egress chain for tap %s: %w", id.TapName, err)
	}
	// Delete the dynamic allow set after its chain: the chain's accept rule
	// references the set, so the set delete must follow the chain delete. This
	// stops a reused tap from inheriting stale pinned (ip . port) elements.
	if err := run(ctx, netconf.NftDeleteSandboxAllowSetArgs(id.TapName), ""); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("delete egress allow set for tap %s: %w", id.TapName, err)
	}
	// Delete the per-sandbox egress counter after its chain (the counting rule
	// references it), so a reused tap starts with a fresh zeroed counter. The
	// counter may not exist (it is opt-in), so this is best-effort like the rest.
	if err := run(ctx, netconf.NftDeleteSandboxEgressCounterArgs(id.TapName), ""); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("delete egress counter for tap %s: %w", id.TapName, err)
	}
	// Per-fork egress proxy DNAT teardown (node-wide proxy only): remove this
	// tap's dispatch element before its DNAT chain (the element references the
	// chain). setup installs both for every sandbox on a proxy node, so without
	// this the per-tap DNAT leaks and a reused tap grows the prerouting dispatch
	// unbounded. Mirrors the inet dispatch-element-then-chain teardown above.
	if opts.proxyEnabled {
		if err := run(ctx, netconf.NftDeleteProxyDNATDispatchElementArgs(id.TapName), ""); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("delete proxy DNAT dispatch element for tap %s: %w", id.TapName, err)
		}
		if err := run(ctx, netconf.NftDeleteProxyDNATChainArgs(id.TapName), ""); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("delete proxy DNAT chain for tap %s: %w", id.TapName, err)
		}
	}
	return firstErr
}
