// internal/daemon/expose_conn.go
package daemon

import (
	"io"
	"net"
	"time"

	"mitos.run/mitos/internal/sandboxrpc"
)

// pfConn adapts a sandboxrpc.PortForwardStream to net.Conn so a standard
// http.Transport can speak HTTP over the vsock PortForward tunnel. Read pulls
// frames from the stream and buffers any bytes a small caller buffer could not
// take; the terminal Close frame surfaces as io.EOF. Write sends a copy toward
// the guest. Deadlines are no-ops: the tunnel lifetime is governed by Close and
// the request context, not by socket deadlines. Bytes are never logged.
type pfConn struct {
	stream sandboxrpc.PortForwardStream
	buf    []byte // unread bytes from the last frame
	eof    bool
}

func newPFConn(stream sandboxrpc.PortForwardStream) net.Conn {
	return &pfConn{stream: stream}
}

func (c *pfConn) Read(p []byte) (int, error) {
	if len(c.buf) == 0 {
		if c.eof {
			return 0, io.EOF
		}
		for {
			frame, err := c.stream.Recv()
			if err != nil {
				return 0, err
			}
			if frame.Close {
				c.eof = true
				return 0, io.EOF
			}
			if len(frame.Data) > 0 {
				// Copy so we never alias the frame's slice; safe even if a
				// future Recv reuses its buffer.
				c.buf = append([]byte(nil), frame.Data...)
				break
			}
			// Empty non-close frame: keep reading.
		}
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}

func (c *pfConn) Write(p []byte) (int, error) {
	chunk := make([]byte, len(p))
	copy(chunk, p)
	if err := c.stream.Send(chunk); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *pfConn) Close() error { return c.stream.Close() }

// pfAddr is a placeholder net.Addr for the loopback-only tunnel endpoints.
type pfAddr struct{}

func (pfAddr) Network() string { return "vsock-portforward" }
func (pfAddr) String() string  { return "guest:loopback" }

func (c *pfConn) LocalAddr() net.Addr                { return pfAddr{} }
func (c *pfConn) RemoteAddr() net.Addr               { return pfAddr{} }
func (c *pfConn) SetDeadline(t time.Time) error      { return nil }
func (c *pfConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *pfConn) SetWriteDeadline(t time.Time) error { return nil }
