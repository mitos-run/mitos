package husk

import (
	"context"
	"fmt"
	"net"
	"time"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/dnsproxy"
	"mitos.run/mitos/internal/netconf"
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
	Egress v1.EgressPolicy
	// Allow is the raw allowlist; IP:port entries become static chain accepts,
	// name entries are enforced by the DNS proxy (handled by the caller).
	Allow []string
	// BlockNetwork drops ALL egress (Modal block_network=True), overriding Egress
	// and the allowlists.
	BlockNetwork bool
	// AllowCIDRs is the egress CIDR allowlist (Modal outbound_cidr_allowlist).
	AllowCIDRs []string
	// Inbound governs unsolicited inbound to the guest; empty means deny-by-default
	// (the secure default), the existing input-chain behavior.
	Inbound v1.InboundPolicy
	// InboundCIDRs narrows an InboundAllow to source CIDRs (Modal
	// inbound_cidr_allowlist).
	InboundCIDRs []string
	// ResolverIP is the in-pod DNS proxy address the chain allows on port 53 and
	// the guest is pointed at. Nil disables the DNS allow rule (IP-only mode).
	ResolverIP net.IP
}

// netfilterPolicyConfig builds the egress and inbound POLICY fields of a
// NetfilterConfig that every husk VM shares, single-VM and multi-VM alike, and
// applies the fail-closed deny-by-default egress. The caller fills in the
// per-activation fields that differ (Tap, GuestIP, HostIP, and ResolverIP for
// the single-VM DNS-proxy path), so the shared policy assembly lives in one
// place and cannot drift between the two paths.
func netfilterPolicyConfig(req ActivateRequest) NetfilterConfig {
	cfg := NetfilterConfig{
		Egress:       v1.EgressPolicy(req.Egress),
		Allow:        req.Allow,
		BlockNetwork: req.BlockNetwork,
		AllowCIDRs:   req.AllowCIDRs,
		Inbound:      v1.InboundPolicy(req.Inbound),
		InboundCIDRs: req.InboundCIDRs,
	}
	if cfg.Egress == "" {
		cfg.Egress = v1.EgressDeny
	}
	return cfg
}

// netfilterRunner executes one host command with optional stdin in the pod
// netns. It is injected so the orchestration is unit-testable without root; the
// production stub wires it to an exec-based runner.
type netfilterRunner func(ctx context.Context, argv []string, stdin string) error

// applyEgressFilter brings up the VM's tap and installs its default-deny egress
// chain (with the unconditional metadata block) in the husk pod netns. It is
// the single-VM-per-pod analog of internal/network.setup. To keep the fork hot
// path cheap it collapses the ~8 sequential fork+exec of ip/nft into just three
// processes: one idempotent tap pre-delete, one `ip -batch` that creates the tap
// + assigns the host IP + brings it up + binds the resolver, and one atomic
// `nft -f` that applies the shared table, this VM's per-tap chain, the SNAT, the
// shared input table, and this tap's input chain. A malformed allowlist fails
// the whole call (fail-closed: a VM never comes up with a half-applied filter).
func applyEgressFilter(ctx context.Context, run netfilterRunner, enableForwarding func() error, cfg NetfilterConfig) (err error) {
	enforceable, _, err := netconf.SplitAllowList(cfg.Allow)
	if err != nil {
		return fmt.Errorf("husk netfilter: parse allowlist: %w", err)
	}
	cidrV4, cidrV6, err := netconf.ParseCIDRList(cfg.AllowCIDRs)
	if err != nil {
		return fmt.Errorf("husk netfilter: parse CIDR allowlist: %w", err)
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
	// The tap name is deterministic per template, so a claimed warm husk may
	// already hold a tap with this name (its dormant VM's NIC), and a prior
	// attempt may have leaked one. Either makes the create below fail with EBUSY
	// (Device or resource busy), which masks the real first-attempt error and
	// permanently poisons the warm pod (issue #428). Remove any pre-existing tap
	// first so creation is idempotent; best-effort, since "not found" is the
	// normal case on a clean activation.
	_ = run(ctx, netconf.LinkDelArgs(cfg.Tap), "")
	// Arm the fail-closed teardown BEFORE the batched tap setup below. The
	// ip -batch invocation may create the tap and then fail on a LATER line in the
	// SAME process, which would leak the tap: its name is deterministic per
	// template, so a leak makes the next activation fail right at tap creation
	// with EBUSY (Device or resource busy), masking the real first-attempt error
	// and permanently poisoning the warm pod (issue #428). Tearing down on every
	// error path removes a partially created tap (link del of an absent tap is an
	// ignored no-op), and the original error is preserved as the named return so
	// the true root-cause step error is surfaced, not the EBUSY of a later retry.
	defer func() {
		if err != nil {
			// Detached from ctx so cleanup still runs when the failure was a
			// context cancellation or deadline (otherwise the same canceled ctx
			// would fail the teardown commands and re-leak the tap).
			_ = teardownEgressFilter(context.WithoutCancel(ctx), run, cfg.Tap)
		}
	}()
	// One ip process instead of ~4: create the tap, assign the host /30, bring the
	// link up, and (single-VM DNS path) bind the in-pod resolver /32. ip -batch
	// aborts on the first failing line and exits non-zero, so a partial tap setup
	// fails the whole call (fail-closed) and triggers the teardown armed above.
	// The resolver bind lets the per-pod DNS proxy listen on the tap so the guest's
	// queries are delivered locally instead of forwarded out (now ip_forward is on).
	if err := run(ctx, netconf.IPBatchArgs(), netconf.RenderIPBatch(cfg.Tap, cfg.HostIP, cfg.ResolverIP)); err != nil {
		return fmt.Errorf("husk netfilter: set up tap %s: %w", cfg.Tap, err)
	}
	// One nft process instead of ~5, applied as a SINGLE atomic transaction:
	//   - the pod-global shared forward table (idempotent skeleton),
	//   - this VM's per-tap egress chain (metadata drop + allowlist + counter),
	//   - the guest SNAT (masquerade) so allowed return traffic is routable,
	//   - the shared input table, and this tap's input chain (pod-local guard).
	// Every statement is an idempotent `add`/`flush` of a named object, so a
	// second CO-LOCATED VM's transaction re-adds the pod-global skeleton without
	// disturbing the first VM's per-tap chain, counter, or dispatch element: the
	// shared table is shared, each VM keeps its OWN tap + default-deny chain +
	// counter. nft applies the whole file atomically, so a malformed allowlist or
	// any rejected statement installs NOTHING (fail-closed: a VM never comes up
	// half-filtered), and the teardown above removes the tap.
	chain := netconf.RenderSandboxChainSpec(netconf.ChainSpec{
		Tap:          cfg.Tap,
		GuestIP:      cfg.GuestIP,
		Egress:       cfg.Egress,
		Allow:        enforceable,
		AllowCIDRsV4: cidrV4,
		AllowCIDRsV6: cidrV6,
		ResolverIP:   cfg.ResolverIP,
		BlockNetwork: cfg.BlockNetwork,
		// Always wire the per-sandbox egress counter so the metering pipeline
		// (#211) can read this sandbox's egress bytes. Passive: no verdict.
		Counter: true,
	})
	inputChain := netconf.RenderSandboxInputChainSpec(netconf.InputChainSpec{
		Tap:          cfg.Tap,
		GuestIP:      cfg.GuestIP,
		ResolverIP:   cfg.ResolverIP,
		Inbound:      cfg.Inbound,
		InboundCIDRs: cfg.InboundCIDRs,
	})
	// Each Render* returns a newline-terminated block, so concatenation yields one
	// well-formed nft ruleset file. Order matters within the transaction: the
	// shared forward table (which defines the dispatch map) precedes the chain that
	// adds a dispatch element into it, and the shared input table precedes this
	// tap's input chain, so every forward reference resolves inside the transaction.
	nftDoc := netconf.RenderSharedTable() + chain + netconf.RenderMasquerade(cfg.GuestIP) +
		netconf.RenderSharedInputTable() + inputChain
	if err := run(ctx, netconf.NftApplyArgs(), nftDoc); err != nil {
		return fmt.Errorf("husk netfilter: apply egress filter for tap %s: %w", cfg.Tap, err)
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
	if err := run(ctx, netconf.NftDeleteSandboxEgressCounterArgs(tap), ""); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("husk netfilter: delete egress counter for tap %s: %w", tap, err)
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
