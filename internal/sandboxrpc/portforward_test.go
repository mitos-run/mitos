package sandboxrpc

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"connectrpc.com/connect"

	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
	"mitos.run/mitos/proto/sandbox/v1/sandboxv1connect"
)

// portForwardGuest extends fakeGuest for PortForward operations. It holds
// scripted PortForwardFrames emitted by the fake guest port and records the
// bytes the client sent toward the guest.
type portForwardGuest struct {
	fakeGuest

	// gotPort records the port number PortForward was called with.
	gotPort uint32

	// guestFrames is the scripted sequence the fake guest emits toward the client.
	guestFrames []*PortForwardFrame

	// sentData accumulates all bytes the client sent toward the guest via Send.
	sentData [][]byte

	// openErr, when non-nil, is returned by PortForward instead of a stream.
	openErr error
}

// fakePortForwardStream backs portForwardGuest.PortForward.
// The stream only emits guestFrames after at least one Send has been received,
// so the test can verify both directions in a realistic sequence.
type fakePortForwardStream struct {
	frames   []*PortForwardFrame
	pos      int
	sentData *[][]byte
	// ready is closed when the first Send arrives, unblocking subsequent Recv.
	ready chan struct{}
}

func (s *fakePortForwardStream) Recv() (*PortForwardFrame, error) {
	// Block until the client sends at least one data byte.
	<-s.ready
	if s.pos >= len(s.frames) {
		return nil, io.EOF
	}
	f := s.frames[s.pos]
	s.pos++
	return f, nil
}

func (s *fakePortForwardStream) Send(data []byte) error {
	buf := make([]byte, len(data))
	copy(buf, data)
	*s.sentData = append(*s.sentData, buf)
	// Unblock Recv on the first Send.
	select {
	case <-s.ready:
		// already unblocked
	default:
		close(s.ready)
	}
	return nil
}

func (s *fakePortForwardStream) Close() error { return nil }

func (g *portForwardGuest) PortForward(_ context.Context, port uint32) (PortForwardStream, error) {
	g.gotPort = port
	if g.openErr != nil {
		return nil, g.openErr
	}
	return &fakePortForwardStream{
		frames:   g.guestFrames,
		sentData: &g.sentData,
		ready:    make(chan struct{}),
	}, nil
}

// newPortForwardTestServer builds a Service wired with the portForwardGuest
// and returns the Connect client.
func newPortForwardTestServer(t *testing.T, g *portForwardGuest) sandboxv1connect.SandboxClient {
	t.Helper()
	svc := &Service{Guest: func(string) (GuestConn, error) { return g, nil }}
	client, _ := newTestServer(t, svc)
	return client
}

// TestPortForwardProxiesBytesInBothDirections is the Task 2.4 acceptance test:
// bytes written by the client appear at the fake guest (client-to-guest),
// and bytes emitted by the fake guest appear at the client (guest-to-client).
func TestPortForwardProxiesBytesInBothDirections(t *testing.T) {
	clientPayload := []byte("GET / HTTP/1.1\r\n")
	guestPayload := []byte("HTTP/1.1 200 OK\r\n")

	g := &portForwardGuest{
		guestFrames: []*PortForwardFrame{
			{Data: guestPayload},
			{Close: true},
		},
	}
	client := newPortForwardTestServer(t, g)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream := client.PortForward(ctx)

	// Send the open frame to select port 8080.
	if err := stream.Send(&sandboxv1.Frame{
		Msg: &sandboxv1.Frame_Open{Open: &sandboxv1.PortForwardOpen{Port: 8080}},
	}); err != nil {
		t.Fatalf("send open: %v", err)
	}

	// Send client data toward the guest.
	if err := stream.Send(&sandboxv1.Frame{
		Msg: &sandboxv1.Frame_Data{Data: clientPayload},
	}); err != nil {
		t.Fatalf("send data: %v", err)
	}

	// Close the client side.
	if err := stream.CloseRequest(); err != nil {
		t.Fatalf("close request: %v", err)
	}

	// Collect frames from the server. Connect wraps the stream-end as a
	// connect.Error with code Unknown, not a bare io.EOF, so check for both.
	var received []byte
	for {
		frame, err := stream.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			var connErr *connect.Error
			if errors.As(err, &connErr) {
				// Normal stream end from the server side.
				break
			}
			t.Fatalf("receive: %v", err)
		}
		received = append(received, frame.GetData()...)
	}

	// Assert client received the guest payload.
	if string(received) != string(guestPayload) {
		t.Fatalf("received = %q, want %q", received, guestPayload)
	}

	// Assert the correct port was forwarded.
	if g.gotPort != 8080 {
		t.Fatalf("gotPort = %d, want 8080", g.gotPort)
	}

	// Assert the client payload reached the fake guest.
	var allSent []byte
	for _, d := range g.sentData {
		allSent = append(allSent, d...)
	}
	if string(allSent) != string(clientPayload) {
		t.Fatalf("guest received = %q, want %q", allSent, clientPayload)
	}
}

// TestPortForwardGuestNilReturnsFollowup verifies that a Service without a
// Guest returns the honest #24 follow-up error for PortForward.
func TestPortForwardGuestNilReturnsFollowup(t *testing.T) {
	svc := &Service{}
	client, _ := newTestServer(t, svc)

	ctx := context.Background()
	stream := client.PortForward(ctx)
	// The guest-nil handler terminates the RPC without ever reading the request
	// stream, so this open write races the termination. Per the connect-go
	// contract, a Send that loses that race returns an error wrapping io.EOF
	// and the real server error is surfaced by Receive below (issue #695).
	if err := stream.Send(&sandboxv1.Frame{
		Msg: &sandboxv1.Frame_Open{Open: &sandboxv1.PortForwardOpen{Port: 3000}},
	}); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("send open: %v", err)
	}
	_ = stream.CloseRequest()
	_, err := stream.Receive()
	if err == nil {
		t.Fatal("expected error from nil Guest, got nil")
	}
	var connErr *connect.Error
	if !errors.As(err, &connErr) {
		t.Fatalf("expected connect.Error, got %T: %v", err, err)
	}
	if connErr.Code() != connect.CodeUnimplemented {
		t.Fatalf("code = %v, want CodeUnimplemented", connErr.Code())
	}
}

// TestPortForwardFirstFrameMustBeOpen rejects a stream whose first frame is
// not a PortForwardOpen with an LLM-legible InvalidArgument error.
func TestPortForwardFirstFrameMustBeOpen(t *testing.T) {
	g := &portForwardGuest{}
	client := newPortForwardTestServer(t, g)

	ctx := context.Background()
	stream := client.PortForward(ctx)
	// Send data frame as first message (missing open).
	if err := stream.Send(&sandboxv1.Frame{
		Msg: &sandboxv1.Frame_Data{Data: []byte("bad")},
	}); err != nil {
		t.Fatalf("send data: %v", err)
	}
	_ = stream.CloseRequest()
	_, err := stream.Receive()
	if err == nil {
		t.Fatal("expected error for non-open first frame")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

// blockingPortForwardGuest backs a PortForward whose Recv blocks until Close
// is called. This models a real guest stream: the guest-to-client pump sits
// in Recv waiting for guest bytes, and only the host calling Close (on handler
// return) unblocks it. It is the regression fixture for the defer-order
// deadlock: with wg.Wait() running before pf.Close(), the handler would hang
// on return because the pump goroutine is stuck in Recv and nothing closes it.
type blockingPortForwardGuest struct {
	fakeGuest
}

// blockingPortForwardStream blocks Recv until Close is called.
type blockingPortForwardStream struct {
	closed chan struct{}
}

func (s *blockingPortForwardStream) Recv() (*PortForwardFrame, error) {
	// Block until Close is called, then report the stream as ended.
	<-s.closed
	return nil, errors.New("port-forward stream closed")
}

func (s *blockingPortForwardStream) Send(_ []byte) error { return nil }

func (s *blockingPortForwardStream) Close() error {
	select {
	case <-s.closed:
		// already closed
	default:
		close(s.closed)
	}
	return nil
}

func (g *blockingPortForwardGuest) PortForward(_ context.Context, _ uint32) (PortForwardStream, error) {
	return &blockingPortForwardStream{closed: make(chan struct{})}, nil
}

// TestPortForwardReturnsOnClientCloseWithBlockingGuest is the regression test
// for the defer-order deadlock. The guest Recv blocks until Close; the client
// closes its request side while the handler is pumping. The handler MUST
// return promptly: pf.Close() (deferred to run before wg.Wait) unblocks the
// pump goroutine's Recv, and wg.Wait then joins it. With the reverse defer
// order this test hangs and trips the timeout.
func TestPortForwardReturnsOnClientCloseWithBlockingGuest(t *testing.T) {
	g := &blockingPortForwardGuest{}
	svc := &Service{Guest: func(string) (GuestConn, error) { return g, nil }}
	client, _ := newTestServer(t, svc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream := client.PortForward(ctx)
	if err := stream.Send(&sandboxv1.Frame{
		Msg: &sandboxv1.Frame_Open{Open: &sandboxv1.PortForwardOpen{Port: 8080}},
	}); err != nil {
		t.Fatalf("send open: %v", err)
	}
	// Close the client request side; the server's main loop then waits for the
	// guest pump, which is blocked in Recv until pf.Close runs on return.
	if err := stream.CloseRequest(); err != nil {
		t.Fatalf("close request: %v", err)
	}

	// Drain the response stream in a goroutine; signal when it ends (which only
	// happens once the handler returns and closes the stream).
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, err := stream.Receive()
			if err != nil {
				return
			}
		}
	}()

	select {
	case <-done:
		// Handler returned promptly: no deadlock.
	case <-time.After(5 * time.Second):
		t.Fatal("PortForward handler did not return within 5s: defer-order deadlock (wg.Wait before pf.Close)")
	}
}
