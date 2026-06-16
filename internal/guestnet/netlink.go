// Package guestnet configures the guest VM's single NIC after a snapshot
// restore using rtnetlink syscalls directly, with no dependency on an `ip`
// binary in the rootfs. Templates are built from arbitrary user OCI images
// (internal/ociroot) that may ship no iproute2 and no busybox `ip` applet, so
// the guest agent cannot shell out to configure eth0; it must speak netlink
// itself. This file holds the pure, host-byte-order message builders and the
// dump parser so the wire layout is unit-testable on any platform; the actual
// socket round trip lives in the linux-tagged Configure.
package guestnet

import (
	"encoding/binary"
	"fmt"
	"net"
)

// nativeEndian is the byte order netlink expects: host order. The guest runs on
// amd64/arm64 (both little-endian); binary.NativeEndian keeps it correct if that
// ever changes.
var nativeEndian = binary.NativeEndian

// Netlink wire constants (uapi/linux/{netlink,rtnetlink,if_addr,if_link}.h).
// Hardcoded rather than taken from x/sys/unix so this file compiles and its
// tests run on non-linux build hosts (the darwin lint and `go test`).
const (
	nlMsgHdrLen  = 16 // sizeof(struct nlmsghdr)
	ifInfoMsgLen = 16 // sizeof(struct ifinfomsg)
	ifAddrMsgLen = 8  // sizeof(struct ifaddrmsg)
	rtMsgLen     = 12 // sizeof(struct rtmsg)
	rtAttrHdrLen = 4  // sizeof(struct rtattr)

	nlMsgError = 2
	nlMsgDone  = 3

	nlmFRequest = 0x1
	nlmFAck     = 0x4
	nlmFReplace = 0x100
	nlmFCreate  = 0x400
	nlmFDump    = 0x100 | 0x200 // NLM_F_ROOT | NLM_F_MATCH

	rtmNewLink  = 16
	rtmNewAddr  = 20
	rtmDelAddr  = 21
	rtmGetAddr  = 22
	rtmNewRoute = 24

	afUnspec = 0
	afInet   = 2

	ifaAddress = 1
	ifaLocal   = 2

	rtaGateway = 5
	rtaOIF     = 4

	iffUp = 0x1

	rtTableMain     = 254
	rtProtBoot      = 3
	rtScopeUniverse = 0
	rtnUnicast      = 1
)

// nlMsghdr mirrors struct nlmsghdr for decoding; builders write the same layout.
type nlMsghdr struct {
	Len   uint32
	Type  uint16
	Flags uint16
	Seq   uint32
	Pid   uint32
}

// addrEntry is one address recovered from an RTM_GETADDR dump: enough to issue
// the matching RTM_DELADDR that flushes it.
type addrEntry struct {
	Index     int32
	IP        net.IP
	PrefixLen uint8
}

// nlAlign rounds n up to the 4-byte netlink alignment (NLMSG_ALIGNTO,
// RTA_ALIGNTO).
func nlAlign(n int) int { return (n + 3) &^ 3 }

// appendAttr appends a 4-byte-aligned rtattr (len, type, payload) to b.
func appendAttr(b []byte, typ uint16, data []byte) []byte {
	attrLen := rtAttrHdrLen + len(data)
	hdr := make([]byte, rtAttrHdrLen)
	nativeEndian.PutUint16(hdr[0:2], uint16(attrLen))
	nativeEndian.PutUint16(hdr[2:4], typ)
	b = append(b, hdr...)
	b = append(b, data...)
	for pad := nlAlign(attrLen) - attrLen; pad > 0; pad-- {
		b = append(b, 0)
	}
	return b
}

// finalize patches the leading nlmsghdr length field to the assembled size.
func finalize(b []byte) []byte {
	nativeEndian.PutUint32(b[0:4], uint32(len(b)))
	return b
}

// msgHeader writes a leading nlmsghdr with the given type/flags/seq.
func msgHeader(typ, flags uint16, seq uint32) []byte {
	b := make([]byte, nlMsgHdrLen)
	nativeEndian.PutUint16(b[4:6], typ)
	nativeEndian.PutUint16(b[6:8], flags)
	nativeEndian.PutUint32(b[8:12], seq)
	return b
}

// buildSetLinkUp builds an RTM_NEWLINK request that sets IFF_UP on ifindex. Only
// the up bit is in the change mask, so other link flags are untouched.
func buildSetLinkUp(seq uint32, ifindex int32) []byte {
	b := msgHeader(rtmNewLink, nlmFRequest|nlmFAck, seq)
	info := make([]byte, ifInfoMsgLen)
	info[0] = afUnspec
	nativeEndian.PutUint32(info[4:8], uint32(ifindex))
	nativeEndian.PutUint32(info[8:12], iffUp)  // flags
	nativeEndian.PutUint32(info[12:16], iffUp) // change mask
	b = append(b, info...)
	return finalize(b)
}

// buildAddAddr builds an RTM_NEWADDR request that assigns ip/prefixLen to
// ifindex. CREATE|REPLACE makes a re-delivery of the same address idempotent.
func buildAddAddr(seq uint32, ifindex int32, ip net.IP, prefixLen uint8) []byte {
	return buildAddrMsg(rtmNewAddr, nlmFRequest|nlmFAck|nlmFCreate|nlmFReplace, seq, ifindex, ip, prefixLen)
}

// buildDelAddr builds an RTM_DELADDR request removing ip/prefixLen from ifindex.
func buildDelAddr(seq uint32, ifindex int32, ip net.IP, prefixLen uint8) []byte {
	return buildAddrMsg(rtmDelAddr, nlmFRequest|nlmFAck, seq, ifindex, ip, prefixLen)
}

func buildAddrMsg(typ, flags uint16, seq uint32, ifindex int32, ip net.IP, prefixLen uint8) []byte {
	b := msgHeader(typ, flags, seq)
	addr := make([]byte, ifAddrMsgLen)
	addr[0] = afInet
	addr[1] = prefixLen
	addr[2] = 0 // flags
	addr[3] = rtScopeUniverse
	nativeEndian.PutUint32(addr[4:8], uint32(ifindex))
	b = append(b, addr...)
	v4 := ip.To4()
	b = appendAttr(b, ifaLocal, v4)
	b = appendAttr(b, ifaAddress, v4)
	return finalize(b)
}

// buildDumpAddr builds the RTM_GETADDR dump request used to enumerate every
// address on every interface (the caller filters by ifindex).
func buildDumpAddr(seq uint32) []byte {
	b := msgHeader(rtmGetAddr, nlmFRequest|nlmFDump, seq)
	addr := make([]byte, ifAddrMsgLen)
	addr[0] = afInet
	b = append(b, addr...)
	return finalize(b)
}

// buildReplaceDefaultRoute builds an RTM_NEWROUTE request installing a default
// route (0.0.0.0/0) via gw out ifindex. CREATE|REPLACE overwrites the
// snapshot-baked default route in place.
func buildReplaceDefaultRoute(seq uint32, ifindex int32, gw net.IP) []byte {
	b := msgHeader(rtmNewRoute, nlmFRequest|nlmFAck|nlmFCreate|nlmFReplace, seq)
	rt := make([]byte, rtMsgLen)
	rt[0] = afInet
	rt[1] = 0 // dst_len: default route
	rt[4] = rtTableMain
	rt[5] = rtProtBoot
	rt[6] = rtScopeUniverse
	rt[7] = rtnUnicast
	b = append(b, rt...)
	b = appendAttr(b, rtaGateway, gw.To4())
	oif := make([]byte, 4)
	nativeEndian.PutUint32(oif, uint32(ifindex))
	b = appendAttr(b, rtaOIF, oif)
	return finalize(b)
}

// walkAttrs splits a 4-byte-aligned rtattr stream into a type->payload map. A
// malformed (too-short or out-of-bounds) attribute stops the walk.
func walkAttrs(b []byte) map[uint16][]byte {
	out := map[uint16][]byte{}
	for len(b) >= rtAttrHdrLen {
		alen := int(nativeEndian.Uint16(b[0:2]))
		typ := nativeEndian.Uint16(b[2:4])
		if alen < rtAttrHdrLen || alen > len(b) {
			break
		}
		out[typ] = b[rtAttrHdrLen:alen]
		b = b[nlAlign(alen):]
	}
	return out
}

// parseAddrDump walks a (possibly multipart) RTM_GETADDR dump buffer and
// returns every IPv4 address it carries, stopping at NLMSG_DONE. An
// NLMSG_ERROR with a non-zero errno is surfaced so a failed dump fails closed.
func parseAddrDump(buf []byte) ([]addrEntry, error) {
	var entries []addrEntry
	for len(buf) >= nlMsgHdrLen {
		mlen := int(nativeEndian.Uint32(buf[0:4]))
		mtype := nativeEndian.Uint16(buf[4:6])
		if mlen < nlMsgHdrLen || mlen > len(buf) {
			return nil, fmt.Errorf("guestnet: truncated netlink message (len=%d, have=%d)", mlen, len(buf))
		}
		msg := buf[:mlen]
		switch mtype {
		case nlMsgDone:
			return entries, nil
		case nlMsgError:
			if len(msg) >= nlMsgHdrLen+4 {
				if errno := int32(nativeEndian.Uint32(msg[nlMsgHdrLen : nlMsgHdrLen+4])); errno != 0 {
					return nil, fmt.Errorf("guestnet: netlink error %d", errno)
				}
			}
		case rtmNewAddr:
			body := msg[nlMsgHdrLen:]
			if len(body) >= ifAddrMsgLen {
				prefix := body[1]
				index := int32(nativeEndian.Uint32(body[4:8]))
				attrs := walkAttrs(body[ifAddrMsgLen:])
				raw, ok := attrs[ifaLocal]
				if !ok {
					raw, ok = attrs[ifaAddress]
				}
				if ok && len(raw) == 4 {
					ip := make(net.IP, 4)
					copy(ip, raw)
					entries = append(entries, addrEntry{Index: index, IP: ip, PrefixLen: prefix})
				}
			}
		}
		buf = buf[nlAlign(mlen):]
	}
	return entries, nil
}
