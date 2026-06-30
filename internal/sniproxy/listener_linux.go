//go:build linux

package sniproxy

import (
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// ListenAndServe binds a TCP listener on addr and serves every accepted
// connection through Serve. Each connection is attributed to a sandbox by its
// source IP (the TCP remote address), and its ORIGINAL destination (the address
// the guest actually dialed before the per-node nftables datapath transparently
// redirected it here) is recovered via SO_ORIGINAL_DST and used as the splice
// target. It blocks until the listener fails to accept; a transient per-
// connection error never tears the loop down.
//
// Build-tagged linux to match the rest of the host-side network datapath: the
// listener only runs on the forkd node, never in the darwin dev/test build.
//
// KVM follow-up (guest-side wireup): the per-sandbox nftables rule that REDIRECTS
// the guest's outbound TLS (tcp dport 443) to this listener is not emitted by
// internal/netconf yet; until it lands, no traffic reaches this listener. Only
// IPv4 original-destination recovery is implemented; IPv6 transparent SNI is part
// of the same follow-up. See docs/networking.md.
func (p *Proxy) ListenAndServe(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("sni proxy listen on %s: %w", addr, err)
	}
	// Publish the listener so Close can unblock the Accept loop. If Close already
	// ran (shutdown raced ahead of the bind), close immediately and return clean.
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = ln.Close()
		return nil
	}
	p.ln = ln
	p.mu.Unlock()
	defer ln.Close()

	for {
		conn, err := ln.Accept()
		if err != nil {
			// A deliberate Close turns the Accept error into a clean shutdown.
			p.mu.Lock()
			closed := p.closed
			p.mu.Unlock()
			if closed {
				return nil
			}
			return fmt.Errorf("sni proxy accept on %s: %w", addr, err)
		}
		tcpConn, ok := conn.(*net.TCPConn)
		if !ok {
			conn.Close()
			continue
		}
		srcAddr, ok := conn.RemoteAddr().(*net.TCPAddr)
		if !ok {
			conn.Close()
			continue
		}
		dstIP, dstPort, derr := originalDst(tcpConn)
		if derr != nil {
			// No recoverable original destination: drop fail-closed rather than
			// splice to an unknown target.
			conn.Close()
			continue
		}
		go p.Serve(conn, srcAddr.IP, dstIP, dstPort)
	}
}

// originalDst recovers the pre-redirect destination of a transparently
// redirected IPv4 TCP connection via the SO_ORIGINAL_DST getsockopt. The option
// returns a sockaddr_in whose bytes overlay the IPv6Mreq.Multiaddr buffer: bytes
// 2:4 are the port (big-endian) and bytes 4:8 are the IPv4 address.
func originalDst(conn syscall.Conn) (net.IP, int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return nil, 0, fmt.Errorf("sni proxy syscall conn: %w", err)
	}
	var (
		ip      net.IP
		port    int
		ctrlErr error
	)
	if cerr := raw.Control(func(fd uintptr) {
		mreq, e := unix.GetsockoptIPv6Mreq(int(fd), unix.SOL_IP, unix.SO_ORIGINAL_DST)
		if e != nil {
			ctrlErr = e
			return
		}
		ip = net.IPv4(mreq.Multiaddr[4], mreq.Multiaddr[5], mreq.Multiaddr[6], mreq.Multiaddr[7])
		port = int(mreq.Multiaddr[2])<<8 | int(mreq.Multiaddr[3])
	}); cerr != nil {
		return nil, 0, fmt.Errorf("sni proxy getsockopt control: %w", cerr)
	}
	if ctrlErr != nil {
		return nil, 0, fmt.Errorf("sni proxy SO_ORIGINAL_DST: %w", ctrlErr)
	}
	if port == 0 {
		return nil, 0, fmt.Errorf("sni proxy: no original destination recovered")
	}
	return ip, port, nil
}
