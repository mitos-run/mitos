//go:build linux

package guestnet

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// recvTimeoutSec bounds each netlink read so a missing kernel reply cannot hang
// the guest agent (PID 1). A timeout surfaces as an error; the caller treats a
// configuration failure as "no network", which fails closed (no egress).
const recvTimeoutSec = 5

// Configure sets iface's hardware address (when mac is non-empty), brings it up,
// flushes its IPv4 addresses, assigns guestIP/prefix, and replaces the default
// route via gatewayIP, all over a single rtnetlink socket. The MAC is applied
// before the link comes up and the address is assigned, using the standard
// down/set/up sequence the kernel requires for a hardware-address change. An
// empty mac leaves the snapshot-baked address untouched (just link up + addr +
// route). It depends on no in-rootfs binary, so it works on arbitrary OCI-image
// templates that ship no `ip`.
func Configure(iface, mac, guestIP, gatewayIP string, prefixLen int) error {
	ip := net.ParseIP(guestIP)
	if ip == nil || ip.To4() == nil {
		return fmt.Errorf("guestnet: invalid guest ip %q", guestIP)
	}
	gw := net.ParseIP(gatewayIP)
	if gw == nil || gw.To4() == nil {
		return fmt.Errorf("guestnet: invalid gateway ip %q", gatewayIP)
	}
	if prefixLen < 0 || prefixLen > 32 {
		return fmt.Errorf("guestnet: invalid prefix len %d", prefixLen)
	}
	var hw net.HardwareAddr
	if mac != "" {
		parsed, err := net.ParseMAC(mac)
		if err != nil {
			return fmt.Errorf("guestnet: invalid mac %q: %w", mac, err)
		}
		hw = parsed
	}

	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return fmt.Errorf("guestnet: lookup %s: %w", iface, err)
	}
	idx := int32(ifi.Index)

	s, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW, unix.NETLINK_ROUTE)
	if err != nil {
		return fmt.Errorf("guestnet: netlink socket: %w", err)
	}
	defer unix.Close(s)
	if err := unix.Bind(s, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fmt.Errorf("guestnet: bind: %w", err)
	}
	tv := unix.Timeval{Sec: recvTimeoutSec}
	if err := unix.SetsockoptTimeval(s, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		return fmt.Errorf("guestnet: set recv timeout: %w", err)
	}

	var seq uint32
	next := func() uint32 { seq++; return seq }

	// Apply the per-fork MAC before bringing the link up: the kernel requires
	// the link down to change the hardware address on most drivers, so the
	// sequence is down, set MAC, then up. When no MAC was delivered, skip the
	// down/set steps and just bring the link up (unchanged legacy behavior).
	if hw != nil {
		if err := doRequest(s, buildSetLinkDown(next(), idx)); err != nil {
			return fmt.Errorf("guestnet: link down: %w", err)
		}
		if err := doRequest(s, buildSetMAC(next(), idx, hw)); err != nil {
			return fmt.Errorf("guestnet: set mac %s: %w", hw, err)
		}
	}

	if err := doRequest(s, buildSetLinkUp(next(), idx)); err != nil {
		return fmt.Errorf("guestnet: link up: %w", err)
	}

	dump, err := doDump(s, buildDumpAddr(next()))
	if err != nil {
		return fmt.Errorf("guestnet: dump addrs: %w", err)
	}
	existing, err := parseAddrDump(dump)
	if err != nil {
		return fmt.Errorf("guestnet: parse addrs: %w", err)
	}
	for _, e := range existing {
		if e.Index != idx {
			continue
		}
		if err := doRequest(s, buildDelAddr(next(), idx, e.IP, e.PrefixLen)); err != nil {
			return fmt.Errorf("guestnet: flush addr %s/%d: %w", e.IP, e.PrefixLen, err)
		}
	}

	if err := doRequest(s, buildAddAddr(next(), idx, ip, uint8(prefixLen))); err != nil {
		return fmt.Errorf("guestnet: add addr %s/%d: %w", ip, prefixLen, err)
	}
	if err := doRequest(s, buildReplaceDefaultRoute(next(), idx, gw)); err != nil {
		return fmt.Errorf("guestnet: default route via %s: %w", gw, err)
	}
	return nil
}

// doRequest sends one ACK-requesting message and consumes its NLMSG_ERROR ACK.
// A zero errno is success; parseAddrDump turns a non-zero errno into an error.
func doRequest(s int, msg []byte) error {
	if err := send(s, msg); err != nil {
		return err
	}
	resp, err := recv(s)
	if err != nil {
		return err
	}
	_, err = parseAddrDump(resp)
	return err
}

// doDump sends a DUMP request and concatenates replies until NLMSG_DONE (or an
// NLMSG_ERROR) terminates the dump.
func doDump(s int, msg []byte) ([]byte, error) {
	if err := send(s, msg); err != nil {
		return nil, err
	}
	var out []byte
	for {
		resp, err := recv(s)
		if err != nil {
			return nil, err
		}
		out = append(out, resp...)
		if terminated(resp) {
			return out, nil
		}
	}
}

func send(s int, msg []byte) error {
	if err := unix.Sendto(s, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fmt.Errorf("guestnet: sendto: %w", err)
	}
	return nil
}

func recv(s int) ([]byte, error) {
	buf := make([]byte, 65536)
	n, _, err := unix.Recvfrom(s, buf, 0)
	if err != nil {
		return nil, fmt.Errorf("guestnet: recvfrom: %w", err)
	}
	return buf[:n], nil
}

// terminated reports whether the batch contains an NLMSG_DONE or NLMSG_ERROR
// message, marking the end of a dump.
func terminated(buf []byte) bool {
	for len(buf) >= nlMsgHdrLen {
		mlen := int(nativeEndian.Uint32(buf[0:4]))
		mtype := nativeEndian.Uint16(buf[4:6])
		if mlen < nlMsgHdrLen || mlen > len(buf) {
			return true
		}
		if mtype == nlMsgDone || mtype == nlMsgError {
			return true
		}
		buf = buf[nlAlign(mlen):]
	}
	return false
}
