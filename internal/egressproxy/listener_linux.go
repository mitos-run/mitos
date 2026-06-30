//go:build linux

package egressproxy

import (
	"fmt"
	"net"
)

// ListenAndServe binds a TCP listener on addr and serves every accepted
// connection through Serve, attributing it to a sandbox by the connection's
// source IP. It is the per-node listener: each fork's nftables DNAT redirects
// that fork's sentinel proxy address to its gateway, and all of those land on
// this one process, which resolves the owning sandbox from the source IP. It
// blocks until the listener fails to accept; a transient per-connection error
// never tears the loop down. The source IP comes from the TCP remote address; a
// connection whose remote address is not a *net.TCPAddr is dropped (it carries
// no guest source IP to attribute).
//
// Build-tagged linux to match the rest of the host-side network datapath: the
// proxy listener only runs on the forkd node, never in the darwin dev/test
// build.
func (p *Proxy) ListenAndServe(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("egress proxy listen on %s: %w", addr, err)
	}
	defer ln.Close()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return fmt.Errorf("egress proxy accept on %s: %w", addr, err)
		}
		tcpAddr, ok := conn.RemoteAddr().(*net.TCPAddr)
		if !ok {
			conn.Close()
			continue
		}
		go p.Serve(conn, tcpAddr.IP)
	}
}
