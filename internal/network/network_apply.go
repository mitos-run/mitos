package network

import (
	"context"
	"fmt"
	"net"

	"github.com/paperclipinc/sandbox/api/v1alpha1"
	"github.com/paperclipinc/sandbox/internal/netconf"
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
	policy v1alpha1.EgressPolicy,
	allow []netconf.HostPort,
	resolverIP net.IP,
	opts applyOptions,
) error {
	if opts.enableForwarding {
		if err := enableForward(); err != nil {
			return fmt.Errorf("enable ip forwarding: %w", err)
		}
	}

	if err := run(ctx, netconf.TapAddArgs(id.TapName), ""); err != nil {
		return fmt.Errorf("create tap %s: %w", id.TapName, err)
	}
	if err := run(ctx, netconf.AddrAddArgs(id.HostIP, id.TapName), ""); err != nil {
		return fmt.Errorf("assign host ip to tap %s: %w", id.TapName, err)
	}
	if err := run(ctx, netconf.LinkUpArgs(id.TapName), ""); err != nil {
		return fmt.Errorf("bring tap %s up: %w", id.TapName, err)
	}

	ruleset := netconf.RenderEgressRuleset(id.TapName, id.GuestIP, policy, allow, resolverIP)
	if err := run(ctx, netconf.NftApplyArgs(), ruleset); err != nil {
		return fmt.Errorf("apply egress ruleset for tap %s: %w", id.TapName, err)
	}

	if opts.uplink != "" {
		if err := run(ctx, netconf.MasqueradeAddArgs(opts.subnetCIDR, opts.uplink), ""); err != nil {
			return fmt.Errorf("add masquerade for %s on %s: %w", opts.subnetCIDR, opts.uplink, err)
		}
	}
	return nil
}

// teardown runs the ordered host commands to remove a sandbox's network:
// delete the tap (which also removes its addresses) and delete the per-tap
// nftables table. Teardown is best-effort: it attempts both steps and returns
// the first error so a partial failure does not leak the other resource.
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
	if err := run(ctx, netconf.NftDeleteTableArgs(id.TapName), ""); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("delete egress table for tap %s: %w", id.TapName, err)
	}
	return firstErr
}
