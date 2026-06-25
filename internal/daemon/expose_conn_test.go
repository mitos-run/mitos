// internal/daemon/expose_conn_test.go
package daemon

import (
	"io"
	"net"
	"testing"

	"mitos.run/mitos/internal/sandboxrpc"
)

// fakePFStream is a scripted PortForwardStream: Recv replays recvFrames in
// order, Send appends to sent, Close records closure.
type fakePFStream struct {
	recvFrames []*sandboxrpc.PortForwardFrame
	recvErr    error // returned after recvFrames are exhausted (nil means io.EOF-style end via Close frame)
	sent       [][]byte
	closed     bool
}

func (f *fakePFStream) Recv() (*sandboxrpc.PortForwardFrame, error) {
	if len(f.recvFrames) == 0 {
		if f.recvErr != nil {
			return nil, f.recvErr
		}
		return nil, io.EOF
	}
	fr := f.recvFrames[0]
	f.recvFrames = f.recvFrames[1:]
	return fr, nil
}

func (f *fakePFStream) Send(data []byte) error { f.sent = append(f.sent, append([]byte(nil), data...)); return nil }
func (f *fakePFStream) Close() error           { f.closed = true; return nil }

func TestPFConnReadReassemblesFramesThenEOF(t *testing.T) {
	st := &fakePFStream{recvFrames: []*sandboxrpc.PortForwardFrame{
		{Data: []byte("hel")},
		{Data: []byte("lo")},
		{Close: true},
	}}
	var c net.Conn = newPFConn(st)

	// A small buffer forces Read to return the buffered remainder across calls.
	buf := make([]byte, 4)
	n, err := c.Read(buf)
	if err != nil || string(buf[:n]) != "hel" {
		t.Fatalf("first read: got %q err %v", buf[:n], err)
	}
	n, err = c.Read(buf)
	if err != nil || string(buf[:n]) != "lo" {
		t.Fatalf("second read: got %q err %v", buf[:n], err)
	}
	if _, err := c.Read(buf); err != io.EOF {
		t.Fatalf("expected io.EOF on Close frame, got %v", err)
	}
}

func TestPFConnWriteSendsCopyAndCloseClosesStream(t *testing.T) {
	st := &fakePFStream{}
	c := newPFConn(st)
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(st.sent) != 1 || string(st.sent[0]) != "ping" {
		t.Fatalf("send not forwarded: %v", st.sent)
	}
	if err := c.Close(); err != nil || !st.closed {
		t.Fatalf("close: err %v closed %v", err, st.closed)
	}
}
