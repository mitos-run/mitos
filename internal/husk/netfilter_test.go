package husk

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/netconf"
)

type recordedCall struct {
	argv  []string
	stdin string
}

type recordingRunner struct{ calls []recordedCall }

func (r *recordingRunner) run(_ context.Context, argv []string, stdin string) error {
	r.calls = append(r.calls, recordedCall{argv: argv, stdin: stdin})
	return nil
}

// failingRunner records calls and returns err on the call at index failAt, so a
// test can simulate a step failing AFTER the tap is created.
type failingRunner struct {
	calls  []recordedCall
	failAt int
	err    error
}

func (r *failingRunner) run(_ context.Context, argv []string, stdin string) error {
	idx := len(r.calls)
	r.calls = append(r.calls, recordedCall{argv: argv, stdin: stdin})
	if r.err != nil && idx == r.failAt {
		return r.err
	}
	return nil
}

// countTapDelete counts link-delete calls for the tap. applyEgressFilter issues
// one up front (idempotent create), so success has exactly one and a failure
// after tap creation has two (the up-front delete plus the teardown).
func countTapDelete(calls []recordedCall, tap string) int {
	want := strings.Join(netconf.LinkDelArgs(tap), " ")
	n := 0
	for _, c := range calls {
		if strings.Join(c.argv, " ") == want {
			n++
		}
	}
	return n
}

func TestApplyEgressFilterRendersDenyChainWithMetadataBlock(t *testing.T) {
	rr := &recordingRunner{}
	cfg := NetfilterConfig{
		Tap:        "sbtap0",
		GuestIP:    net.ParseIP("10.200.0.2"),
		HostIP:     net.ParseIP("10.200.0.1"),
		Egress:     v1.EgressDeny,
		Allow:      []string{"10.0.0.5:5432"},
		ResolverIP: net.ParseIP("169.254.1.1"),
	}
	var forwardingEnabled bool
	enable := func() error { forwardingEnabled = true; return nil }
	if err := applyEgressFilter(context.Background(), rr.run, enable, cfg); err != nil {
		t.Fatal(err)
	}
	if !forwardingEnabled {
		t.Error("applyEgressFilter did not enable IPv4 forwarding")
	}
	// The fork hot path is collapsed to THREE processes: the idempotent tap
	// pre-delete, one `ip -batch` (tap create + host IP + link up + resolver
	// bind), and one atomic `nft -f` (shared table + chain + SNAT + input table +
	// input chain).
	if len(rr.calls) != 3 {
		t.Fatalf("got %d calls, want 3 (link del, ip -batch, nft -f): %+v", len(rr.calls), rr.calls)
	}
	if got := strings.Join(rr.calls[0].argv, " "); got != "ip link del sbtap0" {
		t.Errorf("first call must be the idempotent tap pre-delete, got %q", got)
	}
	// The ip -batch call carries the tap setup on stdin (no per-command execs).
	batchArgv := strings.Join(rr.calls[1].argv, " ")
	if batchArgv != "ip -batch -" {
		t.Errorf("second call must be ip -batch -, got %q", batchArgv)
	}
	batchStdin := rr.calls[1].stdin
	if !strings.Contains(batchStdin, "tuntap add sbtap0 mode tap") {
		t.Errorf("ip batch missing tap create:\n%s", batchStdin)
	}
	if !strings.Contains(batchStdin, "addr add 10.200.0.1/30 dev sbtap0") {
		t.Errorf("ip batch missing host IP assignment:\n%s", batchStdin)
	}
	if !strings.Contains(batchStdin, "link set sbtap0 up") {
		t.Errorf("ip batch missing link up:\n%s", batchStdin)
	}
	// The resolver IP is bound to the tap as a /32 so the per-pod DNS proxy can
	// listen on it and the guest's queries are delivered locally.
	if !strings.Contains(batchStdin, "addr add 169.254.1.1/32 dev sbtap0") {
		t.Errorf("resolver IP not bound to tap as /32:\n%s", batchStdin)
	}
	// The nft call carries the full ruleset (shared table + chain + SNAT + input)
	// as one atomic document on stdin.
	nftArgv := strings.Join(rr.calls[2].argv, " ")
	if nftArgv != "nft -f -" {
		t.Errorf("third call must be nft -f -, got %q", nftArgv)
	}
	nftStdin := rr.calls[2].stdin
	if !strings.Contains(nftStdin, "ip daddr 169.254.169.254 drop") {
		t.Errorf("nft doc missing metadata block:\n%s", nftStdin)
	}
	if !strings.Contains(nftStdin, "ip daddr 10.0.0.5 tcp dport 5432 accept") {
		t.Errorf("nft doc missing static allow:\n%s", nftStdin)
	}
	if !strings.Contains(nftStdin, netconf.SandboxChainName("sbtap0")) {
		t.Errorf("nft doc missing chain named for tap:\n%s", nftStdin)
	}
	if !strings.Contains(nftStdin, "ip saddr 10.200.0.2 masquerade") {
		t.Errorf("nft doc missing masquerade for guest source:\n%s", nftStdin)
	}
}

// TestApplyEgressFilterBatchesExecs pins the exec-count reduction and proves the
// batched processes carry EXACTLY the same commands the per-command builders
// would have run: the ip batch equals RenderIPBatch and the nft document equals
// the concatenation of the shared table, per-tap chain, SNAT, shared input
// table, and per-tap input chain. This is the regression guard for the perf
// change (was ~8-10 execs/VM, now 3) and its correctness (identical ruleset).
func TestApplyEgressFilterBatchesExecs(t *testing.T) {
	rr := &recordingRunner{}
	cfg := NetfilterConfig{
		Tap:        "sbtap0",
		GuestIP:    net.ParseIP("10.200.0.2"),
		HostIP:     net.ParseIP("10.200.0.1"),
		Egress:     v1.EgressDeny,
		Allow:      []string{"10.0.0.5:5432"},
		ResolverIP: net.ParseIP("169.254.1.1"),
	}
	if err := applyEgressFilter(context.Background(), rr.run, func() error { return nil }, cfg); err != nil {
		t.Fatal(err)
	}
	if len(rr.calls) != 3 {
		t.Fatalf("want 3 execs (link del, ip -batch, nft -f), got %d: %+v", len(rr.calls), rr.calls)
	}
	// The ip -batch stdin is byte-for-byte RenderIPBatch (single source of truth).
	wantBatch := netconf.RenderIPBatch(cfg.Tap, cfg.HostIP, cfg.ResolverIP)
	if rr.calls[1].stdin != wantBatch {
		t.Errorf("ip batch stdin mismatch:\n got=%q\nwant=%q", rr.calls[1].stdin, wantBatch)
	}
	// The nft stdin is the concatenation of the same renders the unbatched path
	// applied one at a time, so the installed ruleset is unchanged.
	chain := netconf.RenderSandboxChainSpec(netconf.ChainSpec{
		Tap: cfg.Tap, GuestIP: cfg.GuestIP, Egress: cfg.Egress,
		Allow:      []netconf.HostPort{{IP: net.ParseIP("10.0.0.5"), Port: 5432}},
		ResolverIP: cfg.ResolverIP, Counter: true,
	})
	inputChain := netconf.RenderSandboxInputChainSpec(netconf.InputChainSpec{
		Tap: cfg.Tap, GuestIP: cfg.GuestIP, ResolverIP: cfg.ResolverIP,
	})
	wantNft := netconf.RenderSharedTable() + chain + netconf.RenderMasquerade(cfg.GuestIP) +
		netconf.RenderSharedInputTable() + inputChain
	if rr.calls[2].stdin != wantNft {
		t.Errorf("nft doc mismatch:\n got=%q\nwant=%q", rr.calls[2].stdin, wantNft)
	}
}

// TestApplyEgressFilterInstallsInputGuard proves the per-pod filter also guards
// the INPUT path: the guest may reach the in-pod resolver on 53 but every other
// guest-sourced packet to a pod-local address (the husk-stub sandbox API and
// mTLS control listeners) is dropped, closing the gap that forward-only
// filtering leaves open.
func TestApplyEgressFilterInstallsInputGuard(t *testing.T) {
	rr := &recordingRunner{}
	cfg := NetfilterConfig{
		Tap:        "sbtap0",
		GuestIP:    net.ParseIP("10.200.0.2"),
		HostIP:     net.ParseIP("10.200.0.1"),
		Egress:     v1.EgressDeny,
		ResolverIP: net.ParseIP("169.254.1.1"),
	}
	if err := applyEgressFilter(context.Background(), rr.run, func() error { return nil }, cfg); err != nil {
		t.Fatal(err)
	}
	// The shared input table and this tap's input chain are folded into the single
	// atomic nft document (the third and last exec).
	nftStdin := rr.calls[len(rr.calls)-1].stdin
	if !strings.Contains(nftStdin, "type filter hook input") {
		t.Errorf("shared input table not applied:\n%s", nftStdin)
	}
	if !strings.Contains(nftStdin, "ip saddr 10.200.0.2 ip daddr 169.254.1.1 udp dport 53 accept") {
		t.Errorf("input chain missing resolver allow:\n%s", nftStdin)
	}
	if !strings.Contains(nftStdin, "ip saddr 10.200.0.2 drop") {
		t.Errorf("input chain missing guest-to-pod-local drop:\n%s", nftStdin)
	}
	if !strings.Contains(nftStdin, netconf.SandboxInputChainName("sbtap0")) {
		t.Errorf("input chain not named for tap:\n%s", nftStdin)
	}
}

func TestBuildDNSProxyRegistersNamesOnly(t *testing.T) {
	reg, names, err := buildEgressDNSRegistry("10.200.0.2", []string{"api.example.com:443", "10.0.0.5:5432"})
	if err != nil {
		t.Fatal(err)
	}
	if reg == nil {
		t.Fatal("nil registry")
	}
	// The IP:port entry is enforced by the chain, not the resolver: only the name
	// entry is registered.
	if len(names) != 1 {
		t.Fatalf("registered names = %v, want only api.example.com", names)
	}
	if _, ok := names["api.example.com"]; !ok {
		t.Errorf("api.example.com not registered: %v", names)
	}
}

func TestBuildDNSProxyRejectsBadWildcard(t *testing.T) {
	if _, _, err := buildEgressDNSRegistry("10.200.0.2", []string{"a.*.com:443"}); err == nil {
		t.Fatal("expected error on invalid wildcard, got nil")
	}
}

func TestApplyEgressFilterRejectsMalformedAllow(t *testing.T) {
	rr := &recordingRunner{}
	cfg := NetfilterConfig{
		Tap:     "sbtap0",
		GuestIP: net.ParseIP("10.200.0.2"),
		HostIP:  net.ParseIP("10.200.0.1"),
		Egress:  v1.EgressDeny,
		Allow:   []string{"not-a-valid-entry"},
	}
	if err := applyEgressFilter(context.Background(), rr.run, nil, cfg); err == nil {
		t.Fatal("expected error on malformed allow entry, got nil")
	}
}

// TestApplyEgressFilterTearsDownTapOnPartialFailure proves that when a step
// after tap creation fails, applyEgressFilter removes the tap it created (issue
// #428). The tap name is deterministic per template, so a leaked tap makes every
// retry fail at tap creation with EBUSY and masks the real first-attempt error.
func TestApplyEgressFilterTearsDownTapOnPartialFailure(t *testing.T) {
	stepErr := errors.New("RTNETLINK answers: operation not permitted")
	// Call index 0 is the idempotent tap pre-delete; index 1 is the ip -batch that
	// CREATES the tap; index 2 is the nft apply, the first step after the tap
	// exists. Fail there to prove a post-create failure still tears the tap down.
	rr := &failingRunner{failAt: 2, err: stepErr}
	cfg := NetfilterConfig{
		Tap:     "sbtapdeadbeef",
		GuestIP: net.ParseIP("10.200.0.2"),
		HostIP:  net.ParseIP("10.200.0.1"),
		Egress:  v1.EgressDeny,
	}
	err := applyEgressFilter(context.Background(), rr.run, nil, cfg)
	if err == nil {
		t.Fatal("expected an error from the failing step, got nil")
	}
	// The real step error is surfaced (wrapped), not masked by a later EBUSY.
	if !errors.Is(err, stepErr) {
		t.Fatalf("want the underlying step error wrapped, got: %v", err)
	}
	// The tap was torn down on the failure path (in addition to the up-front
	// idempotent delete), so a retry will not hit EBUSY: two deletes total.
	if got := countTapDelete(rr.calls, cfg.Tap); got != 2 {
		t.Fatalf("want 2 tap deletes (idempotent + teardown), got %d; calls: %+v", got, rr.calls)
	}
}

// TestApplyEgressFilterDoesNotTearDownOnSuccess proves the happy path leaves the
// tap in place: only the up-front idempotent delete runs (one delete), and no
// teardown delete (teardown is the stub's job on Close, not apply's on success).
func TestApplyEgressFilterDoesNotTearDownOnSuccess(t *testing.T) {
	rr := &recordingRunner{}
	cfg := NetfilterConfig{
		Tap:     "sbtap0",
		GuestIP: net.ParseIP("10.200.0.2"),
		HostIP:  net.ParseIP("10.200.0.1"),
		Egress:  v1.EgressDeny,
	}
	if err := applyEgressFilter(context.Background(), rr.run, nil, cfg); err != nil {
		t.Fatal(err)
	}
	if got := countTapDelete(rr.calls, cfg.Tap); got != 1 {
		t.Fatalf("want exactly 1 tap delete (the idempotent pre-create delete), got %d; calls: %+v", got, rr.calls)
	}
}

// TestApplyEgressFilterBatchesNftIntoOneTransaction pins that the egress filter
// installs its whole nftables ruleset in a SINGLE `nft -f -` invocation.
//
// Two reasons. Latency: every nft call is a fork/exec, and applyEgressFilter runs on
// the warm-claim activate critical path (it was ~13 ms of a ~68 ms activate, most of
// it process startup). Correctness: nft applies one -f payload as ONE transaction, so
// batching removes the window in which a sandbox's tap exists with only part of its
// egress policy installed.
//
// The rendered ruleset must still be the same rules in the same order, so the test
// asserts the concatenated payload contains each section and that the shared tables
// precede the per-sandbox chains that jump into them.
func TestApplyEgressFilterBatchesNftIntoOneTransaction(t *testing.T) {
	r := &recordingRunner{}
	cfg := NetfilterConfig{
		Tap:     "sbtest0",
		GuestIP: net.ParseIP("10.200.0.2"),
		HostIP:  net.ParseIP("10.200.0.1"),
		Egress:  v1.EgressDeny,
	}
	if err := applyEgressFilter(context.Background(), r.run, nil, cfg); err != nil {
		t.Fatalf("applyEgressFilter: %v", err)
	}

	var nftCalls []recordedCall
	for _, c := range r.calls {
		if len(c.argv) > 0 && c.argv[0] == "nft" {
			nftCalls = append(nftCalls, c)
		}
	}
	if len(nftCalls) != 1 {
		var argvs []string
		for _, c := range nftCalls {
			argvs = append(argvs, strings.Join(c.argv, " ")+" <<"+c.stdin[:min(24, len(c.stdin))])
		}
		t.Fatalf("want exactly 1 nft invocation, got %d:\n  %s", len(nftCalls), strings.Join(argvs, "\n  "))
	}

	payload := nftCalls[0].stdin
	for _, want := range []string{"table", cfg.Tap} {
		if !strings.Contains(payload, want) {
			t.Errorf("batched nft payload missing %q:\n%s", want, payload)
		}
	}
	// The per-sandbox chain jumps into the shared table, so the shared table must be
	// declared before it within the single transaction.
	sharedIdx := strings.Index(payload, netconf.RenderSharedTable()[:20])
	chainIdx := strings.Index(payload, cfg.Tap)
	if sharedIdx < 0 || chainIdx < 0 || sharedIdx > chainIdx {
		t.Errorf("shared table must precede the per-sandbox chain in the batched payload (shared=%d chain=%d)", sharedIdx, chainIdx)
	}
}

// --- prepare-time link, claim-time policy -----------------------------------------

// isNftApply reports whether argv invokes nft.
func isNftApply(argv []string) bool {
	return len(argv) > 0 && strings.Contains(argv[0], "nft")
}

// isIPCommand reports whether argv invokes ip (tap create / link delete).
func isIPCommand(argv []string) bool {
	return len(argv) > 0 && strings.Contains(argv[0], "ip")
}

// TestApplyEgressFilterIsTheTwoHalves pins that splitting the call did not change what
// it runs: the composed call still issues exactly the link commands and then the single
// atomic nft transaction.
func TestApplyEgressFilterIsTheTwoHalves(t *testing.T) {
	cfg := NetfilterConfig{Tap: "tap0", GuestIP: net.ParseIP("10.200.0.2"), HostIP: net.ParseIP("10.200.0.1"), Egress: v1.EgressDeny}

	var whole recordingRunner
	if err := applyEgressFilter(context.Background(), whole.run, func() error { return nil }, cfg); err != nil {
		t.Fatalf("applyEgressFilter: %v", err)
	}

	var halves recordingRunner
	if err := ensureEgressLink(context.Background(), halves.run, func() error { return nil }, cfg); err != nil {
		t.Fatalf("ensureEgressLink: %v", err)
	}
	if err := applyEgressPolicy(context.Background(), halves.run, cfg); err != nil {
		t.Fatalf("applyEgressPolicy: %v", err)
	}

	if len(whole.calls) != len(halves.calls) {
		t.Fatalf("composed call ran %d commands, the two halves ran %d", len(whole.calls), len(halves.calls))
	}
	for i := range whole.calls {
		if strings.Join(whole.calls[i].argv, " ") != strings.Join(halves.calls[i].argv, " ") {
			t.Errorf("command %d differs: composed %v, halves %v", i, whole.calls[i].argv, halves.calls[i].argv)
		}
		if whole.calls[i].stdin != halves.calls[i].stdin {
			t.Errorf("command %d stdin differs", i)
		}
	}
}

// TestApplyEgressPolicyTouchesOnlyNft is the point of the split: a claim that lands on a
// pod whose tap was already brought up while it was dormant must pay ONE nft transaction
// and no ip commands. The ip half is roughly two thirds of the ~30 ms this costs today.
func TestApplyEgressPolicyTouchesOnlyNft(t *testing.T) {
	cfg := NetfilterConfig{Tap: "tap0", GuestIP: net.ParseIP("10.200.0.2"), HostIP: net.ParseIP("10.200.0.1"), Egress: v1.EgressDeny}

	var rr recordingRunner
	if err := applyEgressPolicy(context.Background(), rr.run, cfg); err != nil {
		t.Fatalf("applyEgressPolicy: %v", err)
	}
	if len(rr.calls) != 1 {
		t.Fatalf("applyEgressPolicy ran %d commands, want exactly 1 (the nft transaction): %v", len(rr.calls), rr.calls)
	}
	if !isNftApply(rr.calls[0].argv) {
		t.Errorf("the one command is not nft: %v", rr.calls[0].argv)
	}
	for _, c := range rr.calls {
		if isIPCommand(c.argv) {
			t.Errorf("applyEgressPolicy must not touch the link: %v", c.argv)
		}
	}
}

// TestApplyEgressPolicyFailsClosed: a rejected policy must leave no tap behind, exactly
// as the composed call does, because a VM must never run half-filtered.
func TestApplyEgressPolicyFailsClosed(t *testing.T) {
	cfg := NetfilterConfig{Tap: "tap0", GuestIP: net.ParseIP("10.200.0.2"), HostIP: net.ParseIP("10.200.0.1"), Egress: v1.EgressDeny}
	rr := &failingRunner{failAt: 0, err: errors.New("nft: syntax error")}

	if err := applyEgressPolicy(context.Background(), rr.run, cfg); err == nil {
		t.Fatal("applyEgressPolicy accepted a rejected nft transaction")
	}
	if got := countTapDelete(rr.calls, "tap0"); got == 0 {
		t.Error("a rejected policy left the tap behind; it must be torn down")
	}
}

// TestEnsureEgressLinkInstallsNoPolicy: the dormant half must not install a ruleset. A
// tap with no policy carries no traffic (nothing is loaded behind it and the shared
// forward table defaults to a drop); a tap with the WRONG policy would.
func TestEnsureEgressLinkInstallsNoPolicy(t *testing.T) {
	cfg := NetfilterConfig{Tap: "tap0", GuestIP: net.ParseIP("10.200.0.2"), HostIP: net.ParseIP("10.200.0.1"), Egress: v1.EgressDeny}

	var rr recordingRunner
	if err := ensureEgressLink(context.Background(), rr.run, func() error { return nil }, cfg); err != nil {
		t.Fatalf("ensureEgressLink: %v", err)
	}
	for _, c := range rr.calls {
		if isNftApply(c.argv) {
			t.Errorf("ensureEgressLink applied a policy: %v", c.argv)
		}
	}
}

// TestApplyEgressFilterRejectsABadAllowlistBeforeTouchingTheLink: a malformed policy
// must cost nothing. It used to be rejected before any command ran; splitting the call
// must not start creating taps for a policy that can never be installed.
func TestApplyEgressFilterRejectsABadAllowlistBeforeTouchingTheLink(t *testing.T) {
	cfg := NetfilterConfig{
		Tap: "tap0", GuestIP: net.ParseIP("10.200.0.2"), HostIP: net.ParseIP("10.200.0.1"),
		Egress: v1.EgressDeny, AllowCIDRs: []string{"not-a-cidr"},
	}
	var rr recordingRunner
	if err := applyEgressFilter(context.Background(), rr.run, func() error { return nil }, cfg); err == nil {
		t.Fatal("applyEgressFilter accepted a malformed CIDR allowlist")
	}
	if len(rr.calls) != 0 {
		t.Errorf("a malformed allowlist ran %d commands, want none: %v", len(rr.calls), rr.calls)
	}
}
