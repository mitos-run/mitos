package husk

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/netconf"
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
		Egress:     v1alpha1.EgressDeny,
		Allow:      []string{"10.0.0.5:5432"},
		ResolverIP: net.ParseIP("169.254.1.1"),
	}
	if err := applyEgressFilter(context.Background(), rr.run, cfg); err != nil {
		t.Fatal(err)
	}
	// Expect: tap add, addr add, link up, shared table apply, sandbox chain apply.
	if len(rr.calls) != 5 {
		t.Fatalf("got %d calls, want 5: %+v", len(rr.calls), rr.calls)
	}
	chainStdin := rr.calls[4].stdin
	if !strings.Contains(chainStdin, "ip daddr 169.254.169.254 drop") {
		t.Errorf("chain missing metadata block:\n%s", chainStdin)
	}
	if !strings.Contains(chainStdin, "ip daddr 10.0.0.5 tcp dport 5432 accept") {
		t.Errorf("chain missing static allow:\n%s", chainStdin)
	}
	if !strings.Contains(chainStdin, netconf.SandboxChainName("sbtap0")) {
		t.Errorf("chain not named for tap:\n%s", chainStdin)
	}
}

func TestApplyEgressFilterRejectsMalformedAllow(t *testing.T) {
	rr := &recordingRunner{}
	cfg := NetfilterConfig{
		Tap:     "sbtap0",
		GuestIP: net.ParseIP("10.200.0.2"),
		HostIP:  net.ParseIP("10.200.0.1"),
		Egress:  v1alpha1.EgressDeny,
		Allow:   []string{"not-a-valid-entry"},
	}
	if err := applyEgressFilter(context.Background(), rr.run, cfg); err == nil {
		t.Fatal("expected error on malformed allow entry, got nil")
	}
}
