package fork

import (
	"net"
	"strings"
	"testing"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/dnsproxy"
	"mitos.run/mitos/internal/egressproxy"
	"mitos.run/mitos/internal/firecracker"
	"mitos.run/mitos/internal/netconf"
	"mitos.run/mitos/internal/network"
)

// testEngineOption mutates a test Engine so network-enabled engine helpers can
// opt into extra wiring (e.g. the egress proxy) without growing a tuple return.
type testEngineOption func(*Engine)

// withEgressProxy wires the per-node egress proxy registry plus the fork-stable
// sentinel endpoint into a test Engine.
func withEgressProxy(reg *egressproxy.Registry, sentinel net.IP, port int) testEngineOption {
	return func(e *Engine) {
		e.egressProxy = reg
		e.proxySentinel = sentinel
		e.proxyPort = port
	}
}

// newTestEngineWithNetwork builds an Engine with networking wired (FakeManager +
// a real Allocator) but WITHOUT touching /dev/kvm or Firecracker, then applies
// the given options.
func newTestEngineWithNetwork(t *testing.T, opts ...testEngineOption) *Engine {
	t.Helper()
	fm := &network.FakeManager{}
	alloc, err := netconf.NewAllocator("10.200.0.0/16", "sb")
	if err != nil {
		t.Fatalf("NewAllocator: %v", err)
	}
	e := &Engine{netMgr: fm, netAlloc: alloc}
	for _, o := range opts {
		o(e)
	}
	return e
}

func TestPrepareForkNetworkRegistersEgressProxy(t *testing.T) {
	reg := egressproxy.NewRegistry()
	e := newTestEngineWithNetwork(t, withEgressProxy(reg, net.ParseIP("169.254.169.2"), 3128))
	fn, err := e.prepareForkNetwork("sbx-1", ForkOpts{Network: &NetworkOpts{}})
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := reg.Lookup(fn.identity.GuestIP); !ok || id != "sbx-1" {
		t.Fatalf("guest IP not registered with proxy: %v %v", id, ok)
	}
	// guestNet carries the fork-stable sentinel endpoint.
	if fn.guestNet.ProxyEndpoint != "169.254.169.2:3128" {
		t.Fatalf("proxy endpoint not delivered: %q", fn.guestNet.ProxyEndpoint)
	}
	e.teardownForkNetwork("sbx-1", fn.identity)
	if _, ok := reg.Lookup(fn.identity.GuestIP); ok {
		t.Fatal("guest IP still registered after teardown")
	}
}

// newNetEngine builds an Engine with networking wired but WITHOUT touching
// /dev/kvm or Firecracker, so the network helpers are unit testable. The
// FakeManager records Setup/Teardown; the Allocator hands out distinct
// identities.
func newNetEngine(t *testing.T) (*Engine, *network.FakeManager, *netconf.Allocator) {
	t.Helper()
	fm := &network.FakeManager{}
	alloc, err := netconf.NewAllocator("10.200.0.0/16", "sb")
	if err != nil {
		t.Fatalf("NewAllocator: %v", err)
	}
	e := &Engine{
		netMgr:     fm,
		netAlloc:   alloc,
		resolverIP: net.ParseIP("10.200.0.1"),
	}
	return e, fm, alloc
}

func TestPrepareForkNetworkDisabled(t *testing.T) {
	// No manager/allocator: networking disabled, helper returns nil and no
	// override is produced regardless of opts.
	e := &Engine{}
	fn, err := e.prepareForkNetwork("sb1", ForkOpts{Network: &NetworkOpts{EgressPolicy: "deny"}})
	if err != nil {
		t.Fatalf("prepareForkNetwork: %v", err)
	}
	if fn != nil {
		t.Errorf("expected nil forkNetwork when disabled, got %+v", fn)
	}
}

func TestPrepareForkNetworkNoOpts(t *testing.T) {
	// Networking enabled but the request carries no NetworkOpts: no-op.
	e, fm, _ := newNetEngine(t)
	fn, err := e.prepareForkNetwork("sb1", ForkOpts{})
	if err != nil {
		t.Fatalf("prepareForkNetwork: %v", err)
	}
	if fn != nil {
		t.Errorf("expected nil forkNetwork without NetworkOpts, got %+v", fn)
	}
	if len(fm.SetupLog) != 0 {
		t.Errorf("Setup must not be called without NetworkOpts: %+v", fm.SetupLog)
	}
}

func TestPrepareForkNetworkSetupAndOverride(t *testing.T) {
	e, fm, _ := newNetEngine(t)
	opts := ForkOpts{Network: &NetworkOpts{
		EgressPolicy: "deny",
		AllowList:    []string{"10.0.0.5:443", "api.example.com:443"},
	}}
	fn, err := e.prepareForkNetwork("sb1", opts)
	if err != nil {
		t.Fatalf("prepareForkNetwork: %v", err)
	}
	if fn == nil {
		t.Fatal("expected a forkNetwork")
	}

	// Setup called exactly once with the parsed policy and the enforceable
	// (IP:port) allow entry only; the DNS-name entry is dropped.
	if len(fm.SetupLog) != 1 {
		t.Fatalf("expected 1 Setup call, got %d", len(fm.SetupLog))
	}
	call := fm.SetupLog[0]
	if call.Policy.Egress != v1.EgressDeny {
		t.Errorf("policy = %q, want deny", call.Policy.Egress)
	}
	if len(call.Policy.Allow) != 1 || !call.Policy.Allow[0].IP.Equal(net.ParseIP("10.0.0.5")) || call.Policy.Allow[0].Port != 443 {
		t.Errorf("allow = %+v, want [10.0.0.5:443]", call.Policy.Allow)
	}
	// The egress byte counter is always wired so the metering pipeline (#211)
	// can read per-sandbox egress bytes.
	if !call.Policy.Counter {
		t.Error("expected the egress counter to be wired on the fork's network policy")
	}
	if !call.ResolverIP.Equal(net.ParseIP("10.200.0.1")) {
		t.Errorf("resolver = %v, want 10.200.0.1", call.ResolverIP)
	}

	// The NIC override remaps the baked iface id to this fork's tap, with the
	// identity's MAC and tap matching what Setup received.
	if len(fn.overrides) != 1 {
		t.Fatalf("expected 1 override, got %d", len(fn.overrides))
	}
	ov := fn.overrides[0]
	if ov.IfaceID != firecracker.NetIfaceID {
		t.Errorf("override iface = %q, want %q", ov.IfaceID, firecracker.NetIfaceID)
	}
	if ov.HostDevName != call.Identity.TapName {
		t.Errorf("override tap %q != identity tap %q", ov.HostDevName, call.Identity.TapName)
	}

	// The guest network config carries the distinct guest IP + host gateway.
	if fn.guestNet == nil {
		t.Fatal("expected guest network config")
	}
	if fn.guestNet.GuestIP != call.Identity.GuestIP.String() {
		t.Errorf("guest IP %q != identity %v", fn.guestNet.GuestIP, call.Identity.GuestIP)
	}
	if fn.guestNet.GatewayIP != call.Identity.HostIP.String() {
		t.Errorf("gateway %q != host IP %v", fn.guestNet.GatewayIP, call.Identity.HostIP)
	}
	if fn.guestNet.PrefixLen != 30 {
		t.Errorf("prefix = %d, want 30", fn.guestNet.PrefixLen)
	}
}

// TestPrepareForkNetworkThreadsNewDimensions asserts block_network, the CIDR
// allowlist, and the inbound policy from NetworkOpts reach the Manager's
// SandboxPolicy (issue #219).
func TestPrepareForkNetworkThreadsNewDimensions(t *testing.T) {
	e, fm, _ := newNetEngine(t)
	opts := ForkOpts{Network: &NetworkOpts{
		EgressPolicy: "deny",
		BlockNetwork: true,
		AllowCIDRs:   []string{"10.0.0.0/8"},
		Inbound:      "allow",
		InboundCIDRs: []string{"203.0.113.0/24"},
	}}
	if _, err := e.prepareForkNetwork("sb1", opts); err != nil {
		t.Fatalf("prepareForkNetwork: %v", err)
	}
	if len(fm.SetupLog) != 1 {
		t.Fatalf("expected 1 Setup call, got %d", len(fm.SetupLog))
	}
	p := fm.SetupLog[0].Policy
	if !p.BlockNetwork {
		t.Error("block_network not threaded to Manager")
	}
	if len(p.AllowCIDRs) != 1 || p.AllowCIDRs[0] != "10.0.0.0/8" {
		t.Errorf("allow_cidrs not threaded: %v", p.AllowCIDRs)
	}
	if p.Inbound != "allow" || len(p.InboundCIDRs) != 1 {
		t.Errorf("inbound not threaded: %q %v", p.Inbound, p.InboundCIDRs)
	}
}

func TestPrepareForkNetworkDistinctPerFork(t *testing.T) {
	e, _, _ := newNetEngine(t)
	opts := ForkOpts{Network: &NetworkOpts{EgressPolicy: "deny"}}

	a, err := e.prepareForkNetwork("sb-a", opts)
	if err != nil {
		t.Fatalf("prepare a: %v", err)
	}
	b, err := e.prepareForkNetwork("sb-b", opts)
	if err != nil {
		t.Fatalf("prepare b: %v", err)
	}
	if a.identity.TapName == b.identity.TapName {
		t.Errorf("tap names collide: %q", a.identity.TapName)
	}
	if a.identity.GuestMAC == b.identity.GuestMAC {
		t.Errorf("MACs collide: %q", a.identity.GuestMAC)
	}
	if a.identity.GuestIP.Equal(b.identity.GuestIP) {
		t.Errorf("guest IPs collide: %v", a.identity.GuestIP)
	}
	if a.overrides[0].HostDevName == b.overrides[0].HostDevName {
		t.Errorf("override taps collide: %q", a.overrides[0].HostDevName)
	}
}

// mustFork sets up a NETWORKED source sandbox on a non-KVM test engine without
// booting Firecracker: it acquires a real per-fork network identity through
// prepareForkNetwork (the same seam fork() uses), records the resulting Sandbox
// (with its retained netOpts) in the engine map, and returns the ForkResult
// view a real fork would. This lets ForkRunning's gate + live-fork wiring be
// exercised against a real source identity without KVM.
func mustFork(t *testing.T, e *Engine, id string) *ForkResult {
	t.Helper()
	if e.sandboxes == nil {
		e.sandboxes = map[string]*Sandbox{}
	}
	opts := ForkOpts{Network: &NetworkOpts{EgressPolicy: "deny"}}
	fn, err := e.prepareForkNetwork(id, opts)
	if err != nil {
		t.Fatalf("mustFork prepare network for %s: %v", id, err)
	}
	e.sandboxes[id] = &Sandbox{ID: id, netID: fn.identity, netOpts: opts.Network}
	return &ForkResult{SandboxID: id, GuestNetwork: fn.guestNet}
}

// TestForkRunningNetworkedFailsClosedWithoutProxy asserts that a live fork
// (ForkRunning) of a networked sandbox is rejected with an explicit, actionable
// error referencing #336 when the egress proxy is NOT wired. Without the proxy
// there is no per-fork isolation, so restoring the source's baked NIC into a
// second live VM would collide on tap/MAC/IP; we fail closed rather than
// silently break networking. The stale #18 reference must be gone.
func TestForkRunningNetworkedFailsClosedWithoutProxy(t *testing.T) {
	e := newTestEngineWithNetwork(t) // networking on, no proxy
	src := mustFork(t, e, "src")
	_, err := e.ForkRunning(src.SandboxID, "child", true)
	if err == nil || !strings.Contains(err.Error(), "#336") {
		t.Fatalf("must fail closed referencing #336, got %v", err)
	}
	if strings.Contains(err.Error(), "#18") {
		t.Fatalf("stale #18 reference: %v", err)
	}
}

// TestForkRunningLiveForkPreparesResetIdentity asserts the substantive
// live-fork network behavior the egress-proxy path (#336) unblocks, at the
// prepareForkNetwork seam ForkRunning drives through fork(): with the proxy
// active, a live fork (ForkOpts.LiveFork=true) gets a FRESH per-fork identity
// distinct from the source, that identity is registered with the proxy, the
// fork-stable proxy endpoint is delivered, and the guest config carries
// ResetUpstreams=true so captured sockets are dropped. A cold fork
// (LiveFork=false) of the SAME engine leaves ResetUpstreams off, proving the
// flag is the only behavioral difference.
func TestForkRunningLiveForkPreparesResetIdentity(t *testing.T) {
	reg := egressproxy.NewRegistry()
	e := newTestEngineWithNetwork(t, withEgressProxy(reg, net.ParseIP("169.254.169.2"), 3128))

	src := mustFork(t, e, "src")

	child, err := e.prepareForkNetwork("child", ForkOpts{
		Network:  &NetworkOpts{EgressPolicy: "deny"},
		LiveFork: true,
	})
	if err != nil {
		t.Fatalf("live fork network prepare: %v", err)
	}
	if child.guestNet.GuestIP == src.GuestNetwork.GuestIP {
		t.Fatalf("child must get a FRESH per-fork identity, got source IP %s", child.guestNet.GuestIP)
	}
	if !child.guestNet.ResetUpstreams {
		t.Fatal("live fork must set ResetUpstreams")
	}
	if child.guestNet.ProxyEndpoint != "169.254.169.2:3128" {
		t.Fatalf("live fork must route through the proxy endpoint, got %q", child.guestNet.ProxyEndpoint)
	}
	if id, ok := reg.Lookup(child.identity.GuestIP); !ok || id != "child" {
		t.Fatalf("child guest IP not registered with proxy: %v %v", id, ok)
	}

	// A cold fork on the same engine leaves ResetUpstreams off: the flag is the
	// only difference between the two paths.
	cold, err := e.prepareForkNetwork("cold", ForkOpts{Network: &NetworkOpts{EgressPolicy: "deny"}})
	if err != nil {
		t.Fatalf("cold fork network prepare: %v", err)
	}
	if cold.guestNet.ResetUpstreams {
		t.Fatal("cold fork must NOT set ResetUpstreams")
	}
}

// TestForkRunningUnknownSandboxWithNetworking keeps the not-found path intact:
// a missing source still reports not found, not the networking error.
func TestForkRunningUnknownSandboxWithNetworking(t *testing.T) {
	e, _, _ := newNetEngine(t)
	e.sandboxes = map[string]*Sandbox{}
	_, err := e.ForkRunning("nope", "child", false)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected not-found error, got %v", err)
	}
}

func TestTeardownForkNetwork(t *testing.T) {
	// Use a /30 allocator (exactly one block) so Release is observable: the
	// slot is exhausted while held and free again after teardown.
	fm := &network.FakeManager{}
	alloc, err := netconf.NewAllocator("10.200.0.0/30", "sb")
	if err != nil {
		t.Fatalf("NewAllocator: %v", err)
	}
	e := &Engine{netMgr: fm, netAlloc: alloc, resolverIP: net.ParseIP("10.200.0.1")}
	opts := ForkOpts{Network: &NetworkOpts{EgressPolicy: "deny"}}

	fn, err := e.prepareForkNetwork("sb1", opts)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// The single /30 block is now held: a different sandbox cannot acquire.
	if _, err := alloc.Acquire("other"); err == nil {
		t.Fatal("expected exhaustion while sb1 holds the only block")
	}

	e.teardownForkNetwork("sb1", fn.identity)

	if len(fm.Teardowns) != 1 || fm.Teardowns[0].TapName != fn.identity.TapName {
		t.Errorf("Teardown not called for identity: %+v", fm.Teardowns)
	}
	// Release freed the block: a new sandbox can now acquire it.
	if _, err := alloc.Acquire("other"); err != nil {
		t.Fatalf("expected block free after teardown, got %v", err)
	}
}

// newDNSEngine builds an Engine with networking AND DNS-based name egress wired
// to a real in-memory dnsproxy.Registry, without touching KVM or Firecracker.
func newDNSEngine(t *testing.T) (*Engine, *network.FakeManager, *dnsproxy.Registry) {
	t.Helper()
	fm := &network.FakeManager{}
	alloc, err := netconf.NewAllocator("10.200.0.0/16", "sb")
	if err != nil {
		t.Fatalf("NewAllocator: %v", err)
	}
	reg := dnsproxy.NewRegistry()
	e := &Engine{
		netMgr:         fm,
		netAlloc:       alloc,
		resolverIP:     net.ParseIP("169.254.1.1"),
		dnsRegistry:    reg,
		enableDNSEgres: true,
	}
	return e, fm, reg
}

func TestPrepareForkNetworkRegistersNames(t *testing.T) {
	e, _, reg := newDNSEngine(t)
	opts := ForkOpts{Network: &NetworkOpts{
		EgressPolicy: "deny",
		AllowList:    []string{"10.0.0.5:443", "api.example.com:443", "api.example.com:8443"},
	}}
	fn, err := e.prepareForkNetwork("sb1", opts)
	if err != nil {
		t.Fatalf("prepareForkNetwork: %v", err)
	}
	if fn == nil {
		t.Fatal("expected a forkNetwork")
	}

	// The sandbox's guest IP is registered with its name allowlist.
	ports, ok := reg.Lookup(fn.identity.GuestIP, "api.example.com")
	if !ok {
		t.Fatal("expected api.example.com registered for the guest IP")
	}
	if len(ports) != 2 {
		t.Errorf("api.example.com ports = %v, want 2 ports", ports)
	}
	// An unlisted name is not registered.
	if _, ok := reg.Lookup(fn.identity.GuestIP, "evil.example.com"); ok {
		t.Error("evil.example.com must not be registered")
	}

	// The resolver IP is delivered to the guest for resolv.conf.
	if fn.guestNet.ResolverIP != "169.254.1.1" {
		t.Errorf("guest resolver = %q, want 169.254.1.1", fn.guestNet.ResolverIP)
	}

	// Teardown deregisters the guest IP.
	e.teardownForkNetwork("sb1", fn.identity)
	if _, ok := reg.Lookup(fn.identity.GuestIP, "api.example.com"); ok {
		t.Error("expected guest deregistered after teardown")
	}
}

func TestPrepareForkNetworkIPOnlyDoesNotRegister(t *testing.T) {
	e, _, reg := newDNSEngine(t)
	opts := ForkOpts{Network: &NetworkOpts{
		EgressPolicy: "deny",
		AllowList:    []string{"10.0.0.5:443"},
	}}
	fn, err := e.prepareForkNetwork("sb1", opts)
	if err != nil {
		t.Fatalf("prepareForkNetwork: %v", err)
	}
	// No name entries: nothing is registered for this guest.
	if _, ok := reg.Lookup(fn.identity.GuestIP, "api.example.com"); ok {
		t.Error("an IP-only allowlist must not register any name")
	}
	// The resolver IP is still delivered so the guest resolves through us.
	if fn.guestNet.ResolverIP != "169.254.1.1" {
		t.Errorf("guest resolver = %q, want 169.254.1.1", fn.guestNet.ResolverIP)
	}
}

func TestPrepareForkNetworkDNSDisabledNoRegister(t *testing.T) {
	// Networking on, DNS egress OFF: behavior is unchanged. No registry call,
	// resolverIP nil, and the guest gets no resolver to point at.
	e, _, _ := newNetEngine(t)
	e.resolverIP = nil
	opts := ForkOpts{Network: &NetworkOpts{
		EgressPolicy: "deny",
		AllowList:    []string{"api.example.com:443"},
	}}
	fn, err := e.prepareForkNetwork("sb1", opts)
	if err != nil {
		t.Fatalf("prepareForkNetwork: %v", err)
	}
	if fn.guestNet.ResolverIP != "" {
		t.Errorf("guest resolver = %q, want empty when DNS egress disabled", fn.guestNet.ResolverIP)
	}
}
