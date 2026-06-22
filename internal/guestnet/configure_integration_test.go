//go:build linux && nettest

// These tests drive the real rtnetlink path against a live kernel and need
// CAP_NET_ADMIN, so they are gated behind the `nettest` build tag and excluded
// from the normal suite. Run them in a privileged Linux container:
//
//	go test -tags nettest ./internal/guestnet/ -run Integration -v
//
// They configure the loopback interface (always present, no setup binary
// needed); a nil return proves the kernel ACKed every netlink message.
package guestnet

import (
	"net"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func requireNetAdmin(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root / CAP_NET_ADMIN")
	}
}

// dumpLoAddrs returns the IPv4 addresses currently on lo via our own netlink
// path, so the test verifies through the same wire format it is exercising.
func dumpLoAddrs(t *testing.T) []addrEntry {
	t.Helper()
	lo, err := net.InterfaceByName("lo")
	if err != nil {
		t.Fatalf("lookup lo: %v", err)
	}
	s, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW, unix.NETLINK_ROUTE)
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	defer unix.Close(s)
	if err := unix.Bind(s, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	raw, err := doDump(s, buildDumpAddr(1))
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	all, err := parseAddrDump(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var out []addrEntry
	for _, e := range all {
		if e.Index == int32(lo.Index) {
			out = append(out, e)
		}
	}
	return out
}

// TestIntegrationConfigureAppliesAddressAndRoute proves Configure runs end to
// end against a real kernel: a nil return means link-up, flush, address-add,
// and default-route-add were all ACKed, and the address is then visible in a
// fresh dump.
func TestIntegrationConfigureAppliesAddressAndRoute(t *testing.T) {
	requireNetAdmin(t)
	const guestIP = "10.123.0.2"
	if err := Configure("lo", "", guestIP, "10.123.0.1", 30); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	found := false
	for _, e := range dumpLoAddrs(t) {
		if e.IP.String() == guestIP && e.PrefixLen == 30 {
			found = true
		}
	}
	if !found {
		t.Errorf("address %s/30 not present on lo after Configure", guestIP)
	}
}

// TestIntegrationConfigureIsIdempotent proves a second Configure with a
// different address flushes the first: lo ends with exactly one address in the
// test's 10.123.0.0/16 range, not an accumulation across runs.
func TestIntegrationConfigureIsIdempotent(t *testing.T) {
	requireNetAdmin(t)
	if err := Configure("lo", "", "10.123.0.2", "10.123.0.1", 30); err != nil {
		t.Fatalf("Configure first: %v", err)
	}
	if err := Configure("lo", "", "10.123.0.6", "10.123.0.5", 30); err != nil {
		t.Fatalf("Configure second: %v", err)
	}
	count := 0
	for _, e := range dumpLoAddrs(t) {
		if ip := e.IP.To4(); ip != nil && ip[0] == 10 && ip[1] == 123 {
			count++
		}
	}
	if count != 1 {
		t.Errorf("got %d addresses in 10.123.0.0/16 on lo, want exactly 1 (flush failed)", count)
	}
}
