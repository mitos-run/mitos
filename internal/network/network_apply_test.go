package network

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/paperclipinc/sandbox/api/v1alpha1"
	"github.com/paperclipinc/sandbox/internal/netconf"
)

type recordedCall struct {
	argv  []string
	stdin string
}

type recordingRunner struct {
	calls   []recordedCall
	failOn  string // substring of argv[0..] that should error
	failErr error
}

func (r *recordingRunner) run(_ context.Context, argv []string, stdin string) error {
	r.calls = append(r.calls, recordedCall{argv: argv, stdin: stdin})
	if r.failOn != "" && strings.Contains(strings.Join(argv, " "), r.failOn) {
		return r.failErr
	}
	return nil
}

func testIdentity() netconf.Identity {
	return netconf.Identity{
		TapName:  "sbtap0",
		GuestMAC: "02:11:22:33:44:55",
		HostIP:   net.ParseIP("10.200.0.1").To4(),
		GuestIP:  net.ParseIP("10.200.0.2").To4(),
	}
}

func TestSetupCommandOrder(t *testing.T) {
	rr := &recordingRunner{}
	id := testIdentity()
	allow := []netconf.HostPort{{IP: net.ParseIP("10.0.0.5"), Port: 443}}
	resolver := net.ParseIP("10.200.0.1")

	err := setup(context.Background(), rr.run, func() error { return nil },
		id, v1alpha1.EgressDeny, allow, resolver, applyOptions{})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	if len(rr.calls) != 4 {
		t.Fatalf("expected 4 commands, got %d: %+v", len(rr.calls), rr.calls)
	}
	// tap create, addr add, link up, nft apply in order.
	wantArgv := [][]string{
		netconf.TapAddArgs(id.TapName),
		netconf.AddrAddArgs(id.HostIP, id.TapName),
		netconf.LinkUpArgs(id.TapName),
		netconf.NftApplyArgs(),
	}
	for i, w := range wantArgv {
		if !reflect.DeepEqual(rr.calls[i].argv, w) {
			t.Errorf("call %d argv = %v, want %v", i, rr.calls[i].argv, w)
		}
	}
	// The nft apply call carries the rendered ruleset on stdin.
	wantRuleset := netconf.RenderEgressRuleset(id.TapName, id.GuestIP, v1alpha1.EgressDeny, allow, resolver)
	if rr.calls[3].stdin != wantRuleset {
		t.Errorf("nft stdin mismatch\ngot:\n%s\nwant:\n%s", rr.calls[3].stdin, wantRuleset)
	}
	// Earlier commands carry no stdin.
	for i := 0; i < 3; i++ {
		if rr.calls[i].stdin != "" {
			t.Errorf("call %d unexpectedly has stdin %q", i, rr.calls[i].stdin)
		}
	}
}

func TestSetupWithForwardingAndMasquerade(t *testing.T) {
	rr := &recordingRunner{}
	id := testIdentity()
	forwardCalled := false

	err := setup(context.Background(), rr.run, func() error { forwardCalled = true; return nil },
		id, v1alpha1.EgressDeny, nil, nil,
		applyOptions{subnetCIDR: "10.200.0.0/16", uplink: "eth0", enableForwarding: true})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if !forwardCalled {
		t.Error("expected forwarding enabler to be called")
	}
	// tap, addr, link, nft, masquerade.
	if len(rr.calls) != 5 {
		t.Fatalf("expected 5 commands, got %d", len(rr.calls))
	}
	last := rr.calls[4].argv
	wantMasq := netconf.MasqueradeAddArgs("10.200.0.0/16", "eth0")
	if !reflect.DeepEqual(last, wantMasq) {
		t.Errorf("last call = %v, want masquerade %v", last, wantMasq)
	}
}

func TestSetupStopsOnError(t *testing.T) {
	rr := &recordingRunner{failOn: "addr add", failErr: errors.New("boom")}
	id := testIdentity()
	err := setup(context.Background(), rr.run, func() error { return nil },
		id, v1alpha1.EgressDeny, nil, nil, applyOptions{})
	if err == nil {
		t.Fatal("expected error from addr add")
	}
	// tap create then the failing addr add; nothing after.
	if len(rr.calls) != 2 {
		t.Errorf("expected 2 calls before stop, got %d", len(rr.calls))
	}
}

func TestTeardownCommandOrder(t *testing.T) {
	rr := &recordingRunner{}
	id := testIdentity()
	err := teardown(context.Background(), rr.run, id, applyOptions{})
	if err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if len(rr.calls) != 2 {
		t.Fatalf("expected 2 commands, got %d: %+v", len(rr.calls), rr.calls)
	}
	wantArgv := [][]string{
		netconf.LinkDelArgs(id.TapName),
		netconf.NftDeleteTableArgs(id.TapName),
	}
	for i, w := range wantArgv {
		if !reflect.DeepEqual(rr.calls[i].argv, w) {
			t.Errorf("call %d argv = %v, want %v", i, rr.calls[i].argv, w)
		}
	}
}

func TestTeardownBestEffort(t *testing.T) {
	// link del fails but table delete still runs; the first error is returned.
	rr := &recordingRunner{failOn: "link del", failErr: errors.New("no such device")}
	id := testIdentity()
	err := teardown(context.Background(), rr.run, id, applyOptions{})
	if err == nil {
		t.Fatal("expected error from link del")
	}
	if len(rr.calls) != 2 {
		t.Errorf("expected both teardown commands to run, got %d", len(rr.calls))
	}
}

func TestTeardownWithMasquerade(t *testing.T) {
	rr := &recordingRunner{}
	id := testIdentity()
	err := teardown(context.Background(), rr.run, id,
		applyOptions{subnetCIDR: "10.200.0.0/16", uplink: "eth0"})
	if err != nil {
		t.Fatalf("teardown: %v", err)
	}
	// masquerade del, link del, table del.
	if len(rr.calls) != 3 {
		t.Fatalf("expected 3 commands, got %d", len(rr.calls))
	}
	if !reflect.DeepEqual(rr.calls[0].argv, netconf.MasqueradeDelArgs("10.200.0.0/16", "eth0")) {
		t.Errorf("first teardown call = %v, want masquerade del", rr.calls[0].argv)
	}
}

func TestFakeManagerRecords(t *testing.T) {
	fm := &FakeManager{}
	id := testIdentity()
	allow := []netconf.HostPort{{IP: net.ParseIP("10.0.0.5"), Port: 443}}
	if err := fm.Setup(context.Background(), id, v1alpha1.EgressDeny, allow, net.ParseIP("10.200.0.1")); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := fm.Teardown(context.Background(), id); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if len(fm.SetupLog) != 1 || fm.SetupLog[0].Policy != v1alpha1.EgressDeny {
		t.Errorf("SetupLog not recorded: %+v", fm.SetupLog)
	}
	if len(fm.Teardowns) != 1 || fm.Teardowns[0].TapName != "sbtap0" {
		t.Errorf("Teardowns not recorded: %+v", fm.Teardowns)
	}
}
