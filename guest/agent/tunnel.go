//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"mitos.run/mitos/internal/vsock"
)

// tunnelDialTimeout bounds the in-guest dial to 127.0.0.1:<port>. A loopback
// dial to a listening port returns immediately; this ceiling bounds a dial to a
// port whose backlog is wedged so the handler returns a clean refused ack rather
// than hanging the host caller.
const tunnelDialTimeout = 5 * time.Second

// handleTunnel implements the host TunnelRequest on a DEDICATED vsock
// connection (issue #228): it dials 127.0.0.1:<port> INSIDE the guest, writes a
// single TunnelAck line back, and on success pipes bytes bidirectionally between
// that guest TCP socket and conn until either side closes. The target is forced
// to loopback (the host only carries a port, never an address), so the tunnel
// can never reach the guest's other interfaces or the host network.
//
// conn is the raw dedicated connection. The dispatcher has already consumed the
// request line; the host sends NO payload bytes before it has read this ack
// (the host-side proxy blocks on the ack in vsock.StreamConn.Tunnel before it
// writes anything), so no application bytes can be coalesced with the request
// line and lost. On a refused dial the ack carries OK=false with an LLM-legible
// cause and the connection is closed.
func handleTunnel(conn net.Conn, req *vsock.TunnelRequest) {
	if req.Port < 1 || req.Port > 65535 {
		// Refusal path: a failed ack write just means the host hung up; there is
		// nothing left to splice, so the error is intentionally discarded.
		_ = writeTunnelAck(conn, vsock.TunnelAck{OK: false, Error: fmt.Sprintf("invalid guest port %d: must be 1-65535", req.Port)})
		return
	}

	// Loopback ONLY. The host carries a bare port; the guest always dials
	// 127.0.0.1 so a tunnel cannot be steered to another interface.
	target, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", req.Port), tunnelDialTimeout)
	if err != nil {
		// The dial error names the loopback target and the OS cause (connection
		// refused when nothing is listening); it carries no secret value. A failed
		// ack write just means the host hung up, so the error is discarded.
		_ = writeTunnelAck(conn, vsock.TunnelAck{OK: false, Error: fmt.Sprintf("dial 127.0.0.1:%d in guest: %v", req.Port, err)})
		return
	}

	if err := writeTunnelAck(conn, vsock.TunnelAck{OK: true}); err != nil {
		// The host hung up before the pipe started; drop the guest socket.
		target.Close()
		return
	}

	// Splice bytes both directions. Each copy closes BOTH ends when it finishes
	// so a half-close on one side promptly tears the other down: no goroutine or
	// fd leaks once either peer closes. The application bytes are never logged.
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			conn.Close()
			target.Close()
		})
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(target, conn)
		closeBoth()
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(conn, target)
		closeBoth()
	}()
	wg.Wait()
}

// writeTunnelAck marshals one ack and writes it as a single newline-delimited
// line. It returns the write error so the caller can drop the guest socket if
// the host already hung up.
func writeTunnelAck(conn net.Conn, ack vsock.TunnelAck) error {
	b, err := json.Marshal(ack)
	if err != nil {
		return err
	}
	_, werr := conn.Write(append(b, '\n'))
	return werr
}
