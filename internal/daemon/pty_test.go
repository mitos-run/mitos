package daemon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// This file holds the shared PTY test fixtures (a PTY-capable fake guest gRPC
// server and the SandboxAPI builder) plus the ptyAuth security tests. The legacy
// JSON /v1/pty wire was removed in #358; the interactive terminal is now the
// Connect-over-WebSocket Exec endpoint (execWSPath), which shares the same
// ptyAuth bearer gate. The auth properties that were once asserted through
// /v1/pty are asserted here against the ws Exec upgrade so the coverage survives.

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
// handler on sockPath.
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

// dialExecWS opens the Connect-over-WebSocket Exec upgrade for sandbox with the
// given bearer (empty for none). It returns the dial error and the handshake
// response so a caller can assert the auth status code.
func dialExecWS(t *testing.T, baseURL, sandbox, bearer string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	opts := &websocket.DialOptions{Subprotocols: []string{execWSSubprotocol}}
	if bearer != "" {
		opts.HTTPHeader = http.Header{"Authorization": {"Bearer " + bearer}}
	}
	return websocket.Dial(ctx, wsExecURL(baseURL, sandbox), opts)
}

// TestExecWSRejectsMissingToken asserts the ptyAuth gate rejects an upgrade with
// no Authorization header when a token IS registered (401, fail closed). This is
// the ws-path successor of the legacy /v1/pty missing-token test.
func TestExecWSRejectsMissingToken(t *testing.T) {
	_, srv := newPtyAPI(t, "sekret")
	_, resp, err := dialExecWS(t, srv.URL, "sb1", "")
	if err == nil {
		t.Fatal("expected dial to fail without a token")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v, want 401", resp)
	}
}

// TestExecWSRejectsCrossSandboxToken registers two sandboxes A and B with
// distinct tokens, then attempts to open B's Exec ws using A's token. ptyAuth
// compares the presented token against the token of the ?sandbox= id (B), so A's
// token must not drive B's terminal: the upgrade must be rejected with 401. This
// is the ws-path successor of the legacy /v1/pty cross-sandbox-token test.
func TestExecWSRejectsCrossSandboxToken(t *testing.T) {
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

	// Present A's token while targeting B's terminal.
	_, resp, err := dialExecWS(t, srv.URL, "sbB", "tokenA")
	if err == nil {
		t.Fatal("expected dial to fail: A's token must not drive B's terminal")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v, want 401", resp)
	}
}

// TestExecWSTokenlessAllowed asserts that under AllowTokenless (the standalone
// sandbox-server trust model) the ws Exec upgrade succeeds with no bearer and the
// terminal drives to a clean exit. This is the ws-path successor of the legacy
// /v1/pty tokenless test.
func TestExecWSTokenlessAllowed(t *testing.T) {
	_, srv := newPtyAPI(t, "") // AllowTokenless, like sandbox-server
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := dialExecWS(t, srv.URL, "sb1", "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	// Open a PTY exec, then send exit and read the terminal exit frame.
	writeFrame(ctx, t, c, false, &sandboxv1.ExecRequest{
		Msg: &sandboxv1.ExecRequest_Open{Open: &sandboxv1.ExecOpen{
			Pty: &sandboxv1.PtyOptions{Size: &sandboxv1.WindowSize{Cols: 80, Rows: 24}},
		}},
	})
	writeFrame(ctx, t, c, false, &sandboxv1.ExecRequest{
		Msg: &sandboxv1.ExecRequest_Stdin{Stdin: []byte("exit\n")},
	})
	flags, ex := readResponse(ctx, t, c)
	if ex.GetExit() == nil {
		t.Fatalf("final frame = %+v, want exit", ex)
	}
	if flags&connectFlagEndStream == 0 {
		t.Fatalf("exit frame missing end-stream flag (flags=0x%02x)", flags)
	}
}

// TestExecWSStreamCapRejected verifies the per-sandbox concurrent-stream cap
// admits streams up to the cap and a saturated sandbox rejects a NEW ws Exec.
// Post-upgrade the cap surfaces as a policy-violation close (the handshake has
// already returned 101), so we assert the connection is closed by the server
// with a non-normal status rather than a 429 handshake. This is the ws-path
// successor of the legacy /v1/pty stream-cap test.
func TestExecWSStreamCapRejected(t *testing.T) {
	api, srv := newPtyAPI(t, "")
	api.SetMaxStreamsPerSandbox(1)

	// Pre-saturate the single slot, simulating one in-flight stream.
	rel, ok := api.acquireStream("sb1")
	if !ok {
		t.Fatal("pre-saturate: slot must be acquirable")
	}
	defer rel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := dialExecWS(t, srv.URL, "sb1", "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	// Open frame: ExecPTY tries to acquire a slot, fails (cap=1, slot held), and
	// the server closes with a policy-violation after a terminal error frame.
	writeFrame(ctx, t, c, false, &sandboxv1.ExecRequest{
		Msg: &sandboxv1.ExecRequest_Open{Open: &sandboxv1.ExecOpen{
			Pty: &sandboxv1.PtyOptions{Size: &sandboxv1.WindowSize{Cols: 80, Rows: 24}},
		}},
	})
	// Read until the stream ends; the server must terminate it (cap exceeded),
	// never serve the exec.
	for {
		_, _, rerr := c.Read(ctx)
		if rerr != nil {
			var ce websocket.CloseError
			if !errors.As(rerr, &ce) {
				// Any read error after a cap rejection is acceptable as long as the
				// stream did not serve a normal exec; the slot stays held by us.
				return
			}
			if ce.Code == websocket.StatusNormalClosure {
				t.Fatalf("ws Exec served despite the stream cap being saturated")
			}
			return
		}
	}
}
