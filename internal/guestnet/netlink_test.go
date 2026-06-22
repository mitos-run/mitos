package guestnet

import (
	"net"
	"testing"
)

// TestBuildAddAddrRoundTripsThroughParse proves the RTM_NEWADDR builder lays out
// the nlmsghdr + ifaddrmsg + IFA_LOCAL attribute exactly as the kernel emits
// them in an address dump, by parsing the builder's own output back. If the
// header sizing, struct field order, or attribute alignment were wrong the
// round trip would not recover the index, prefix, and address.
func TestBuildAddAddrRoundTripsThroughParse(t *testing.T) {
	ip := net.ParseIP("10.200.0.2").To4()
	msg := buildAddAddr(1, 7, ip, 30)

	entries, err := parseAddrDump(msg)
	if err != nil {
		t.Fatalf("parseAddrDump: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.Index != 7 {
		t.Errorf("index = %d, want 7", e.Index)
	}
	if e.PrefixLen != 30 {
		t.Errorf("prefixlen = %d, want 30", e.PrefixLen)
	}
	if !e.IP.Equal(ip) {
		t.Errorf("ip = %v, want %v", e.IP, ip)
	}
}

// TestParseAddrDumpHandlesMultipartAndStopsAtDone proves the parser walks a
// multi-message dump buffer (the kernel concatenates one nlmsghdr per address)
// and halts at NLMSG_DONE, ignoring anything after the terminator. This is the
// behavior the flush step relies on to enumerate every baked address before
// deleting it.
func TestParseAddrDumpHandlesMultipartAndStopsAtDone(t *testing.T) {
	a := buildAddAddr(1, 7, net.ParseIP("10.0.0.2").To4(), 30)
	b := buildAddAddr(2, 7, net.ParseIP("10.0.0.6").To4(), 24)
	done := buildDone(3)
	after := buildAddAddr(4, 9, net.ParseIP("10.0.0.9").To4(), 32)

	buf := concat(a, b, done, after)
	entries, err := parseAddrDump(buf)
	if err != nil {
		t.Fatalf("parseAddrDump: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2 (must stop at NLMSG_DONE)", len(entries))
	}
	if !entries[0].IP.Equal(net.ParseIP("10.0.0.2").To4()) || !entries[1].IP.Equal(net.ParseIP("10.0.0.6").To4()) {
		t.Errorf("entries = %v, want the two addresses before DONE", entries)
	}
}

// TestParseAddrDumpSurfacesKernelError proves an NLMSG_ERROR with a non-zero
// errno in the buffer is returned as an error rather than silently parsed as
// an address, so a failed dump fails closed instead of flushing nothing.
func TestParseAddrDumpSurfacesKernelError(t *testing.T) {
	if _, err := parseAddrDump(buildError(1, -1)); err == nil {
		t.Fatal("parseAddrDump on NLMSG_ERROR returned nil error, want non-nil")
	}
}

// TestBuildReplaceDefaultRouteLayout proves the default-route builder emits an
// RTM_NEWROUTE with create+replace flags, a zero-length destination (the
// default route), and the gateway and output-interface attributes carrying the
// values passed in. Replace semantics are what let a restored fork overwrite
// the snapshot-baked default route in place.
func TestBuildReplaceDefaultRouteLayout(t *testing.T) {
	gw := net.ParseIP("10.200.0.1").To4()
	msg := buildReplaceDefaultRoute(1, 7, gw)

	hdr, body := splitMsg(t, msg)
	if hdr.Type != rtmNewRoute {
		t.Errorf("nlmsg type = %d, want RTM_NEWROUTE(%d)", hdr.Type, rtmNewRoute)
	}
	if hdr.Flags&nlmFCreate == 0 || hdr.Flags&nlmFReplace == 0 {
		t.Errorf("flags = %#x, want CREATE|REPLACE set", hdr.Flags)
	}
	if dstLen := body[1]; dstLen != 0 {
		t.Errorf("rtmsg dst_len = %d, want 0 (default route)", dstLen)
	}

	attrs := walkAttrs(body[rtMsgLen:])
	gotGW, ok := attrs[rtaGateway]
	if !ok || !net.IP(gotGW).Equal(gw) {
		t.Errorf("RTA_GATEWAY = %v (present=%v), want %v", net.IP(gotGW), ok, gw)
	}
	gotOIF, ok := attrs[rtaOIF]
	if !ok || len(gotOIF) != 4 || nativeEndian.Uint32(gotOIF) != 7 {
		t.Errorf("RTA_OIF = %v (present=%v), want 7", gotOIF, ok)
	}
}

// TestBuildDelAddrTargetsAddress proves the flush builder emits an RTM_DELADDR
// carrying the same index/prefix/address it is asked to remove, so each baked
// address recovered from the dump is deleted precisely.
func TestBuildDelAddrTargetsAddress(t *testing.T) {
	ip := net.ParseIP("10.0.0.2").To4()
	msg := buildDelAddr(1, 7, ip, 30)

	hdr, body := splitMsg(t, msg)
	if hdr.Type != rtmDelAddr {
		t.Errorf("nlmsg type = %d, want RTM_DELADDR(%d)", hdr.Type, rtmDelAddr)
	}
	if body[1] != 30 {
		t.Errorf("ifaddrmsg prefixlen = %d, want 30", body[1])
	}
	if idx := int32(nativeEndian.Uint32(body[4:8])); idx != 7 {
		t.Errorf("ifaddrmsg index = %d, want 7", idx)
	}
	attrs := walkAttrs(body[ifAddrMsgLen:])
	if got, ok := attrs[ifaLocal]; !ok || !net.IP(got).Equal(ip) {
		t.Errorf("IFA_LOCAL = %v (present=%v), want %v", net.IP(got), ok, ip)
	}
}

// TestBuildDumpAddrIsDumpRequest proves the enumerate builder asks for a dump
// (NLM_F_DUMP) of RTM_GETADDR, which is what makes the kernel return every
// address terminated by NLMSG_DONE.
func TestBuildDumpAddrIsDumpRequest(t *testing.T) {
	hdr, _ := splitMsg(t, buildDumpAddr(1))
	if hdr.Type != rtmGetAddr {
		t.Errorf("nlmsg type = %d, want RTM_GETADDR(%d)", hdr.Type, rtmGetAddr)
	}
	if hdr.Flags&nlmFDump != nlmFDump {
		t.Errorf("flags = %#x, want NLM_F_DUMP(%#x) set", hdr.Flags, nlmFDump)
	}
}

// TestBuildSetLinkDownLayout proves the link-down builder requests RTM_NEWLINK
// with IFF_UP cleared in the flags but set in the change mask, so only the up
// bit is toggled off (a zero change mask would be a no-op). Bringing the link
// down is the first step of changing the hardware address.
func TestBuildSetLinkDownLayout(t *testing.T) {
	msg := buildSetLinkDown(1, 7)
	hdr, body := splitMsg(t, msg)
	if hdr.Type != rtmNewLink {
		t.Errorf("nlmsg type = %d, want RTM_NEWLINK(%d)", hdr.Type, rtmNewLink)
	}
	index := int32(nativeEndian.Uint32(body[4:8]))
	flags := nativeEndian.Uint32(body[8:12])
	change := nativeEndian.Uint32(body[12:16])
	if index != 7 {
		t.Errorf("ifinfomsg index = %d, want 7", index)
	}
	if flags&iffUp != 0 {
		t.Errorf("flags=%#x, want IFF_UP clear (link down)", flags)
	}
	if change&iffUp == 0 {
		t.Errorf("change=%#x, want IFF_UP set in change mask", change)
	}
}

// TestBuildSetMACCarriesAddress proves the MAC builder emits an RTM_NEWLINK for
// the target ifindex carrying an IFLA_ADDRESS attribute whose 6-byte payload is
// exactly the hardware address passed in. This is what gives each restored fork
// its own distinct eth0 MAC instead of the shared snapshot placeholder.
func TestBuildSetMACCarriesAddress(t *testing.T) {
	hw, err := net.ParseMAC("02:11:22:33:44:55")
	if err != nil {
		t.Fatalf("ParseMAC: %v", err)
	}
	msg := buildSetMAC(1, 7, hw)

	hdr, body := splitMsg(t, msg)
	if hdr.Type != rtmNewLink {
		t.Errorf("nlmsg type = %d, want RTM_NEWLINK(%d)", hdr.Type, rtmNewLink)
	}
	if index := int32(nativeEndian.Uint32(body[4:8])); index != 7 {
		t.Errorf("ifinfomsg index = %d, want 7", index)
	}
	attrs := walkAttrs(body[ifInfoMsgLen:])
	got, ok := attrs[iflaAddress]
	if !ok {
		t.Fatalf("IFLA_ADDRESS attribute missing; attrs=%v", attrs)
	}
	if len(got) != len(hw) {
		t.Fatalf("IFLA_ADDRESS len = %d, want %d", len(got), len(hw))
	}
	for i := range hw {
		if got[i] != hw[i] {
			t.Fatalf("IFLA_ADDRESS = %x, want %x", got, []byte(hw))
		}
	}
}

// TestBuildSetLinkUpLayout proves the link-up builder requests RTM_NEWLINK with
// IFF_UP set in both the flags and the change mask, so only the up bit is
// toggled (a zero change mask would be a no-op).
func TestBuildSetLinkUpLayout(t *testing.T) {
	msg := buildSetLinkUp(1, 7)
	hdr, body := splitMsg(t, msg)
	if hdr.Type != rtmNewLink {
		t.Errorf("nlmsg type = %d, want RTM_NEWLINK(%d)", hdr.Type, rtmNewLink)
	}
	// ifinfomsg: family(1) pad(1) type(2) index(int32 @4) flags(uint32 @8) change(uint32 @12)
	index := int32(nativeEndian.Uint32(body[4:8]))
	flags := nativeEndian.Uint32(body[8:12])
	change := nativeEndian.Uint32(body[12:16])
	if index != 7 {
		t.Errorf("ifinfomsg index = %d, want 7", index)
	}
	if flags&iffUp == 0 || change&iffUp == 0 {
		t.Errorf("flags=%#x change=%#x, want IFF_UP set in both", flags, change)
	}
}
