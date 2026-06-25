package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	"mitos.run/mitos/internal/vsock"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// fakePtyGuestSandbox is an in-process Sandbox gRPC server for PTY tests. Its
// Exec handler reads the ExecOpen (PTY mode), then echoes each stdin chunk as
// stdout, and terminates cleanly when stdin "exit\n" is received or when the
// stream closes.
type fakePtyGuestSandbox struct {
	sandboxv1.UnimplementedSandboxServer
}

// Exec implements the PTY path: reads ExecOpen (ignoring argv since the
// client sends no command), then echoes stdin bytes as stdout frames until
// "exit\n" or stream close.
func (s *fakePtyGuestSandbox) Exec(stream sandboxv1.Sandbox_ExecServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	open := first.GetOpen()
	if open == nil {
		return io.ErrUnexpectedEOF
	}
	// Echo each stdin chunk as stdout; exit cleanly on "exit\n" or stream close.
	for {
		msg, rerr := stream.Recv()
		if rerr == io.EOF {
			return stream.Send(&sandboxv1.ExecResponse{
				Msg: &sandboxv1.ExecResponse_Exit{Exit: &sandboxv1.ExecExit{ExitCode: 0}},
			})
		}
		if rerr != nil {
			return rerr
		}
		data := msg.GetStdin()
		if len(data) == 0 {
			continue
		}
		if string(data) == "exit\n" {
			return stream.Send(&sandboxv1.ExecResponse{
				Msg: &sandboxv1.ExecResponse_Exit{Exit: &sandboxv1.ExecExit{ExitCode: 0}},
			})
		}
		if err := stream.Send(&sandboxv1.ExecResponse{
			Msg: &sandboxv1.ExecResponse_Stdout{Stdout: data},
		}); err != nil {
			return err
		}
	}
}

// startFakePtyGRPCUDS starts an in-process gRPC server with a PTY-capable Exec
// handler on sockPath, suitable for replacing startFakePtyUDS in PTY tests.
func startFakePtyGRPCUDS(t *testing.T, sockPath string) {
	t.Helper()
	startFakeGuestGRPCUDS(t, sockPath, &fakePtyGuestSandbox{})
}

func newPtyAPI(t *testing.T, token string) (*SandboxAPI, *httptest.Server) {
	t.Helper()
	dir := shortVsockDir(t)
	sock := filepath.Join(dir, "sb1", "vsock.sock")
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		t.Fatal(err)
	}
	startFakePtyGRPCUDS(t, sock)
	api := NewSandboxAPI(dir)
	if token != "" {
		api.RegisterToken("sb1", token)
	} else {
		api.AllowTokenless()
	}
	if err := api.RegisterSandbox("sb1", sock); err != nil {
		t.Fatal(err)
	}
	api.RegisterStreamPath("sb1", sock)
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return api, srv
}

func wsURL(httpURL, sandbox string) string {
	s := httpURL
	if len(s) > 7 && s[:7] == "http://" {
		s = "ws://" + s[7:]
	}
	return s + "/v1/pty?sandbox=" + sandbox
}

func TestPtyWebSocketEchoExit(t *testing.T) {
	_, srv := newPtyAPI(t, "sekret")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, wsURL(srv.URL, "sb1"), &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": {"Bearer sekret"}},
		Subprotocols: []string{"mitos.pty.v1"},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	in, _ := json.Marshal(vsock.PtyFrame{Kind: vsock.PtyInput, Data: []byte("hello-pty\n")})
	if err := c.Write(ctx, websocket.MessageText, in); err != nil {
		t.Fatalf("write input: %v", err)
	}
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	var out vsock.PtyFrame
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Kind != vsock.PtyOutput || string(out.Data) != "hello-pty\n" {
		t.Fatalf("output = %+v", out)
	}

	exitFrame, _ := json.Marshal(vsock.PtyFrame{Kind: vsock.PtyInput, Data: []byte("exit\n")})
	_ = c.Write(ctx, websocket.MessageText, exitFrame)
	_, data, err = c.Read(ctx)
	if err != nil {
		t.Fatalf("read exit: %v", err)
	}
	var ex vsock.PtyFrame
	_ = json.Unmarshal(data, &ex)
	if ex.Kind != vsock.PtyExit {
		t.Fatalf("expected exit frame, got %+v", ex)
	}
}

func TestPtyWebSocketRejectsBadToken(t *testing.T) {
	_, srv := newPtyAPI(t, "sekret")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, wsURL(srv.URL, "sb1"), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer wrong"}},
	})
	if err == nil {
		t.Fatal("expected dial to fail on bad token")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v", resp)
	}
}

func TestPtyWebSocketRejectsMissingToken(t *testing.T) {
	_, srv := newPtyAPI(t, "sekret")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, wsURL(srv.URL, "sb1"), nil)
	if err == nil {
		t.Fatal("expected dial to fail without a token")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v", resp)
	}
}

// TestPtyWebSocketRejectsCrossSandboxToken registers two sandboxes A and B with
// distinct tokens, then attempts to open B's PTY using A's token. The auth gate
// compares the presented token against the token of the ?sandbox= id (B), so
// A's token must not drive B's PTY: the upgrade must be rejected with 401.
func TestPtyWebSocketRejectsCrossSandboxToken(t *testing.T) {
	dir := shortVsockDir(t)
	api := NewSandboxAPI(dir)
	for _, sb := range []struct{ id, token string }{{"sbA", "tokenA"}, {"sbB", "tokenB"}} {
		sock := filepath.Join(dir, sb.id, "vsock.sock")
		if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
			t.Fatal(err)
		}
		startFakePtyGRPCUDS(t, sock)
		api.RegisterToken(sb.id, sb.token)
		if err := api.RegisterSandbox(sb.id, sock); err != nil {
			t.Fatal(err)
		}
		api.RegisterStreamPath(sb.id, sock)
	}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Present A's token while targeting B's PTY.
	_, resp, err := websocket.Dial(ctx, wsURL(srv.URL, "sbB"), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer tokenA"}},
	})
	if err == nil {
		t.Fatal("expected dial to fail: A's token must not drive B's pty")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v", resp)
	}
}

func TestPtyWebSocketTokenlessAllowed(t *testing.T) {
	_, srv := newPtyAPI(t, "") // AllowTokenless, like sandbox-server
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL(srv.URL, "sb1"), &websocket.DialOptions{
		Subprotocols: []string{"mitos.pty.v1"},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	exitFrame, _ := json.Marshal(vsock.PtyFrame{Kind: vsock.PtyInput, Data: []byte("exit\n")})
	_ = c.Write(ctx, websocket.MessageText, exitFrame)
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var ex vsock.PtyFrame
	_ = json.Unmarshal(data, &ex)
	if ex.Kind != vsock.PtyExit {
		t.Fatalf("expected exit, got %+v", ex)
	}
}

// TestPtyStreamCapRejected verifies the per-sandbox concurrent-stream cap (3)
// admits streams up to the cap and rejects the N+1th as a clean 429 BEFORE the
// WebSocket upgrade (the client gets a non-101 response, not a close code).
func TestPtyStreamCapRejected(t *testing.T) {
	_, srv := newPtyAPI(t, "")
	// Lower cap to 1 so we only need one held slot to trigger rejection.
	api, _ := newPtyAPI(t, "")
	api.SetMaxStreamsPerSandbox(1)
	srv2 := httptest.NewServer(api.Handler())
	t.Cleanup(srv2.Close)
	_ = srv

	// Pre-saturate the slot.
	rel, ok := api.acquireStream("sb1")
	if !ok {
		t.Fatal("pre-saturate: slot must be acquirable")
	}
	defer rel()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, wsURL(srv2.URL, "sb1"), &websocket.DialOptions{
		Subprotocols: []string{"mitos.pty.v1"},
	})
	if err == nil {
		t.Fatal("expected dial to fail when stream cap is exceeded")
	}
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %v, want 429", resp)
	}
}

// TestPtyResizeForwarded verifies that a resize frame sent over the WebSocket
// reaches the gRPC Exec stream as a WindowSize resize message, by using a fake
// that echoes resize events as a synthetic stdout line.
func TestPtyResizeForwarded(t *testing.T) {
	// Use a custom fake that responds to resize with a confirmation on stdout.
	dir := shortVsockDir(t)
	sock := filepath.Join(dir, "sbr", "vsock.sock")
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		t.Fatal(err)
	}

	startFakeGuestGRPCUDS(t, sock, &fakeResizeSandbox{})
	api := NewSandboxAPI(dir)
	api.AllowTokenless()
	if err := api.RegisterSandbox("sbr", sock); err != nil {
		t.Fatal(err)
	}
	api.RegisterStreamPath("sbr", sock)
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL(srv.URL, "sbr"), &websocket.DialOptions{
		Subprotocols: []string{"mitos.pty.v1"},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	// Send a resize.
	resizeFrame, _ := json.Marshal(vsock.PtyFrame{Kind: vsock.PtyResize, Cols: 120, Rows: 40})
	if err := c.Write(ctx, websocket.MessageText, resizeFrame); err != nil {
		t.Fatalf("write resize: %v", err)
	}
	// The fake echoes back a stdout confirmation then exits.
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var f vsock.PtyFrame
	_ = json.Unmarshal(data, &f)
	if f.Kind != vsock.PtyOutput {
		t.Fatalf("want output frame, got %+v", f)
	}
	if string(f.Data) != "resize:120x40\n" {
		t.Fatalf("resize confirmation = %q, want resize:120x40", f.Data)
	}
}

// fakeResizeSandbox echoes resize events as "resize:CxR\n" stdout and exits after.
type fakeResizeSandbox struct {
	sandboxv1.UnimplementedSandboxServer
}

func (s *fakeResizeSandbox) Exec(stream sandboxv1.Sandbox_ExecServer) error {
	_, err := stream.Recv() // read ExecOpen
	if err != nil {
		return err
	}
	for {
		msg, rerr := stream.Recv()
		if rerr == io.EOF {
			return stream.Send(&sandboxv1.ExecResponse{
				Msg: &sandboxv1.ExecResponse_Exit{Exit: &sandboxv1.ExecExit{ExitCode: 0}},
			})
		}
		if rerr != nil {
			return rerr
		}
		if ws := msg.GetResize(); ws != nil {
			out := []byte("resize:" + uintStr(ws.GetCols()) + "x" + uintStr(ws.GetRows()) + "\n")
			if err := stream.Send(&sandboxv1.ExecResponse{
				Msg: &sandboxv1.ExecResponse_Stdout{Stdout: out},
			}); err != nil {
				return err
			}
			return stream.Send(&sandboxv1.ExecResponse{
				Msg: &sandboxv1.ExecResponse_Exit{Exit: &sandboxv1.ExecExit{ExitCode: 0}},
			})
		}
	}
}

// uintStr converts a uint32 to a decimal string without importing strconv
// (keeping the test file dependency-light).
func uintStr(n uint32) string {
	if n == 0 {
		return "0"
	}
	buf := make([]byte, 0, 10)
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}
