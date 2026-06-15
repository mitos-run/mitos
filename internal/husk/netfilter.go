package husk

import (
	"context"
	"fmt"
	"net"

	"github.com/paperclipinc/mitos/api/v1alpha1"
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
func applyEgressFilter(ctx context.Context, run netfilterRunner, cfg NetfilterConfig) error {
	enforceable, _, err := netconf.SplitAllowList(cfg.Allow)
	if err != nil {
		return fmt.Errorf("husk netfilter: parse allowlist: %w", err)
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
	if err := run(ctx, netconf.NftApplyArgs(), netconf.RenderSharedTable()); err != nil {
		return fmt.Errorf("husk netfilter: apply shared egress table: %w", err)
	}
	chain := netconf.RenderSandboxChain(cfg.Tap, cfg.GuestIP, cfg.Egress, enforceable, cfg.ResolverIP)
	if err := run(ctx, netconf.NftApplyArgs(), chain); err != nil {
		return fmt.Errorf("husk netfilter: apply egress chain for tap %s: %w", cfg.Tap, err)
	}
	return nil
}

// teardownEgressFilter removes this VM's tap and per-tap egress state. It is
// best-effort and returns the first error so a partial teardown does not leak
// the rest. Called by the stub on Close (wired in the stub Close hook).
//
//nolint:unused // wired into Stub.Close in the following commit (same branch).
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
	return firstErr
}
