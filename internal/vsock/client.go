package vsock

import (
	"fmt"
	"net"
	"time"
)

// DefaultRequestTimeout bounds the preamble read in DialGRPCConn so a wedged
// guest that connects to the vsock UDS but never completes the "OK <port>"
// acknowledgment cannot hang the caller goroutine indefinitely. It is NOT
// applied to the long-lived gRPC traffic that follows; gRPC manages its own
// per-RPC deadlines via context cancellation.
const DefaultRequestTimeout = 60 * time.Second

// ChunkFunc receives one stream's bytes as they arrive. Returning a non-nil
// error stops the stream early.
type ChunkFunc func(stream StreamName, data []byte) error

// DialGRPCConn dials the guest agent over the Firecracker vsock UDS, performs
// the "CONNECT <port>\n" / "OK <port>" preamble, and returns the RAW net.Conn
// ready for gRPC framing (HTTP/2) to take over. It is the transport the host
// side of the gRPC runtime protocol uses to reach the guest gRPC server on
// AgentGRPCPort.
//
// The preamble "OK" line is read ONE BYTE AT A TIME directly from the conn (not
// through a bufio.Scanner) so the read stops exactly at the preamble newline and
// never over-consumes into the gRPC HTTP/2 client preface or settings frames
// that follow. The guest gRPC server sends nothing before it sees the client's
// first bytes, so in practice there is nothing buffered after the newline; the
// byte-at-a-time read guarantees correctness regardless. The preamble read is
// bounded by maxAckLineBytes and a deadline so a wedged guest cannot hang the
// caller; the deadline is cleared before the conn is returned so it never leaks
// onto the long-lived gRPC traffic.
func DialGRPCConn(udsPath string, guestPort int) (net.Conn, error) {
	conn, err := net.DialTimeout("unix", udsPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial grpc vsock UDS: %w", err)
	}
	if _, err := conn.Write([]byte(fmt.Sprintf("CONNECT %d\n", guestPort))); err != nil {
		conn.Close()
		return nil, fmt.Errorf("vsock CONNECT: %w", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(DefaultRequestTimeout))
	line, leftover, rerr := readAckLine(conn)
	_ = conn.SetReadDeadline(time.Time{})
	if rerr != nil {
		conn.Close()
		return nil, fmt.Errorf("vsock CONNECT: %w", rerr)
	}
	if len(line) < 2 || string(line[:2]) != "OK" {
		conn.Close()
		return nil, fmt.Errorf("vsock CONNECT rejected: %s", string(line))
	}
	if len(leftover) > 0 {
		// The guest sent application bytes coalesced with the preamble newline.
		// Replay them ahead of the conn so gRPC's HTTP/2 reader sees them first.
		return &prefixConn{Conn: conn, leftover: leftover}, nil
	}
	return conn, nil
}

// DialGRPCConnUnix dials a PLAIN unix socket where the guest gRPC server listens
// WITHOUT a CONNECT preamble (the standalone sandbox-server's local-testing
// fallback: the guest binds /tmp/sandbox-agent-<port>.sock via net.Listen and
// serves gRPC directly on it). It returns the raw net.Conn for gRPC framing.
func DialGRPCConnUnix(sockPath string) (net.Conn, error) {
	conn, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial grpc unix: %w", err)
	}
	return conn, nil
}

// prefixConn replays leftover bytes (captured alongside the CONNECT preamble
// newline) before reading from the underlying conn, so no application bytes are
// lost when the preamble read coalesces with early payload. Writes and Close go
// straight to the underlying conn.
type prefixConn struct {
	net.Conn
	leftover []byte
}

func (p *prefixConn) Read(b []byte) (int, error) {
	if len(p.leftover) > 0 {
		n := copy(b, p.leftover)
		p.leftover = p.leftover[n:]
		return n, nil
	}
	return p.Conn.Read(b)
}

// readAckLine reads bytes from conn until the first newline, returning the line
// WITHOUT the trailing newline and any bytes that arrived in the same read after
// the newline (leftover, which belongs to the raw pipe that follows the ack). It
// reads in small chunks so it cannot over-consume more than one OS read past the
// newline, and that single over-read is preserved as leftover. The ack line is
// bounded by maxAckLineBytes so a guest that never sends a newline cannot drive
// an unbounded host allocation.
func readAckLine(conn net.Conn) (line, leftover []byte, err error) {
	var buf []byte
	tmp := make([]byte, 512)
	for {
		n, rerr := conn.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if i := bytesIndexByte(buf, '\n'); i >= 0 {
				return buf[:i], append([]byte(nil), buf[i+1:]...), nil
			}
			if len(buf) > maxAckLineBytes {
				return nil, nil, fmt.Errorf("tunnel ack exceeded %d bytes without a newline", maxAckLineBytes)
			}
		}
		if rerr != nil {
			if len(buf) == 0 {
				return nil, nil, fmt.Errorf("connection closed before ack")
			}
			return nil, nil, fmt.Errorf("connection closed mid-ack")
		}
	}
}

// maxAckLineBytes bounds the host's preamble-ack read so a guest that connects
// but never sends a newline-terminated ack cannot drive an unbounded allocation.
const maxAckLineBytes = 64 << 10

// bytesIndexByte returns the index of the first b in s, or -1. A tiny local
// helper to avoid importing bytes solely for IndexByte in this file.
func bytesIndexByte(s []byte, b byte) int {
	for i := range s {
		if s[i] == b {
			return i
		}
	}
	return -1
}
