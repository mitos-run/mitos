package husk

import (
	"context"
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
	// Expect: tap add, addr add, link up, resolver addr add, shared table apply,
	// sandbox chain apply, masquerade apply, shared input table apply, sandbox
	// input chain apply.
	if len(rr.calls) != 9 {
		t.Fatalf("got %d calls, want 9: %+v", len(rr.calls), rr.calls)
	}
	// The resolver IP is bound to the tap as a /32 so the per-pod DNS proxy can
	// listen on it and the guest's queries are delivered locally.
	resolverArgv := strings.Join(rr.calls[3].argv, " ")
	if !strings.Contains(resolverArgv, "169.254.1.1/32") || !strings.Contains(resolverArgv, "sbtap0") {
		t.Errorf("resolver IP not bound to tap as /32: %v", rr.calls[3].argv)
	}
	chainStdin := rr.calls[5].stdin
	if !strings.Contains(chainStdin, "ip daddr 169.254.169.254 drop") {
		t.Errorf("chain missing metadata block:\n%s", chainStdin)
	}
	if !strings.Contains(chainStdin, "ip daddr 10.0.0.5 tcp dport 5432 accept") {
		t.Errorf("chain missing static allow:\n%s", chainStdin)
	}
	if !strings.Contains(chainStdin, netconf.SandboxChainName("sbtap0")) {
		t.Errorf("chain not named for tap:\n%s", chainStdin)
	}
	masqStdin := rr.calls[6].stdin
	if !strings.Contains(masqStdin, "ip saddr 10.200.0.2 masquerade") {
		t.Errorf("missing masquerade for guest source:\n%s", masqStdin)
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
	// The last two applies are the shared input table and this tap's input chain.
	inputTable := rr.calls[7].stdin
	if !strings.Contains(inputTable, "type filter hook input") {
		t.Errorf("shared input table not applied:\n%s", inputTable)
	}
	inputChain := rr.calls[8].stdin
	if !strings.Contains(inputChain, "ip saddr 10.200.0.2 ip daddr 169.254.1.1 udp dport 53 accept") {
		t.Errorf("input chain missing resolver allow:\n%s", inputChain)
	}
	if !strings.Contains(inputChain, "ip saddr 10.200.0.2 drop") {
		t.Errorf("input chain missing guest-to-pod-local drop:\n%s", inputChain)
	}
	if !strings.Contains(inputChain, netconf.SandboxInputChainName("sbtap0")) {
		t.Errorf("input chain not named for tap:\n%s", inputChain)
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
