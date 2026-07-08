package husk

// Integration coverage for the in-pod sandbox HTTP API the husk stub serves
// after a successful activate (issue #18, slice 2, Fix B). After Activate the
// stub registers the activated VM and its per-sandbox bearer token with a
// daemon.SandboxAPI and serves the token-gated exec/files API on the sandbox
// port, exactly as forkd does. This test proves:
//
//   - after Activate, an HTTP exec carrying the per-sandbox bearer token reaches
//     the (fake) guest agent over gRPC and returns the guest's reply;
//   - an exec WITHOUT the token (or with the wrong token) is rejected (401);
//   - the bearer token VALUE is never written to the captured stub log.
//
// The activate path runs end to end through the Stub OnActivated hook (the same
// hook cmd/husk-stub wires), with a fake VMM, a fake fork-correctness notifier,
// and a REAL fake gRPC guest agent on a unix socket, so the exec genuinely
// traverses RegisterSandbox -> vsock gRPC -> agent.

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/coder/websocket"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"mitos.run/mitos/internal/daemon"
	"mitos.run/mitos/internal/firecracker"
	"mitos.run/mitos/internal/vsock"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
	"mitos.run/mitos/proto/sandbox/v1/sandboxv1connect"
)

// huskFakeGuest is a minimal sandbox.v1.SandboxServer that returns a fixed
// stdout + exit 0 for every Exec call. All other methods fall through to the
// unimplemented stubs.
type huskFakeGuest struct {
	sandboxv1.UnimplementedSandboxServer
}

func (g *huskFakeGuest) Exec(stream sandboxv1.Sandbox_ExecServer) error {
	_, err := stream.Recv()
	if err != nil {
		return err
	}
	if err := stream.Send(&sandboxv1.ExecResponse{
		Msg: &sandboxv1.ExecResponse_Stdout{Stdout: []byte("husk-exec-ok\n")},
	}); err != nil {
		return err
	}
	return stream.Send(&sandboxv1.ExecResponse{
		Msg: &sandboxv1.ExecResponse_Exit{Exit: &sandboxv1.ExecExit{ExitCode: 0}},
	})
}

func (g *huskFakeGuest) List(_ context.Context, _ *sandboxv1.ListRequest) (*sandboxv1.ListResponse, error) {
	return &sandboxv1.ListResponse{}, nil
}
func (g *huskFakeGuest) Mkdir(_ context.Context, _ *sandboxv1.MkdirRequest) (*sandboxv1.MkdirResponse, error) {
	return &sandboxv1.MkdirResponse{}, nil
}
func (g *huskFakeGuest) Remove(_ context.Context, _ *sandboxv1.RemoveRequest) (*sandboxv1.RemoveResponse, error) {
	return &sandboxv1.RemoveResponse{}, nil
}
func (g *huskFakeGuest) ReadFile(_ *sandboxv1.ReadFileRequest, stream sandboxv1.Sandbox_ReadFileServer) error {
	return stream.Send(&sandboxv1.Chunk{Eof: true})
}
func (g *huskFakeGuest) WriteFile(stream sandboxv1.Sandbox_WriteFileServer) error {
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	return stream.SendAndClose(&sandboxv1.WriteFileResult{})
}

// huskChanConnListener is a net.Listener that yields conns from a channel.
type huskChanConnListener struct {
	conns chan net.Conn
	done  chan struct{}
}

func (l *huskChanConnListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *huskChanConnListener) Close() error {
	select {
	case <-l.done:
	default:
		close(l.done)
	}
	return nil
}

func (l *huskChanConnListener) Addr() net.Addr { return &net.UnixAddr{Name: "fake"} }

// huskReplayConn prepends leftover bytes (buffered past the CONNECT preamble
// newline) before the real conn Read, so the gRPC HTTP/2 preface is not lost.
type huskReplayConn struct {
	net.Conn
	leftover []byte
}

func (c *huskReplayConn) Read(b []byte) (int, error) {
	if len(c.leftover) > 0 {
		n := copy(b, c.leftover)
		c.leftover = c.leftover[n:]
		return n, nil
	}
	return c.Conn.Read(b)
}

// startHuskGRPCUDS listens on sockPath, reads the Firecracker vsock CONNECT
// preamble, replies "OK <port>", then feeds AgentGRPCPort connections to an
// in-process gRPC server serving huskFakeGuest.
func startHuskGRPCUDS(t *testing.T, sockPath string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lis.Close() })

	grpcSrv := grpc.NewServer()
	sandboxv1.RegisterSandboxServer(grpcSrv, &huskFakeGuest{})

	cl := &huskChanConnListener{conns: make(chan net.Conn), done: make(chan struct{})}
	go grpcSrv.Serve(cl) //nolint:errcheck // test: errors surface via RPC failures
	t.Cleanup(func() {
		grpcSrv.Stop()
		cl.Close()
	})

	grpcPortStr := strconv.Itoa(vsock.AgentGRPCPort)
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				r := bufio.NewReader(c)
				line, err := r.ReadString('\n')
				if err != nil || !strings.HasPrefix(line, "CONNECT ") {
					c.Close()
					return
				}
				port := strings.TrimSpace(strings.TrimPrefix(line, "CONNECT "))
				if _, err := c.Write([]byte("OK " + port + "\n")); err != nil {
					c.Close()
					return
				}
				// Only gRPC-port connections go to the gRPC server.
				// Port 52 (legacy JSON for PTY/forward) is unused here.
				if port != grpcPortStr {
					c.Close()
					return
				}
				var served net.Conn = c
				if n := r.Buffered(); n > 0 {
					buf, _ := r.Peek(n)
					served = &huskReplayConn{Conn: c, leftover: append([]byte(nil), buf...)}
				}
				select {
				case cl.conns <- served:
				case <-cl.done:
					c.Close()
				}
			}(conn)
		}
	}()
}

// pathVMM is a vmm whose VsockHostPath returns a fixed host UDS path (the fake
// agent socket), so the OnActivated hook registers the sandbox against a real
// reachable agent.
type pathVMM struct {
	vsockPath string
}

func (m *pathVMM) LoadSnapshotWithOverrides(_, _ string, _ bool, _ []firecracker.NetworkOverride) error {
	return nil
}
func (m *pathVMM) LoadSnapshotUFFD(_, _ string, _ []firecracker.NetworkOverride) error {
	return nil
}
func (m *pathVMM) VsockHostPath(string) string              { return m.vsockPath }
func (m *pathVMM) PatchDrive(_, _ string) error             { return nil }
func (m *pathVMM) Resume() error                            { return nil }
func (m *pathVMM) Pause() error                             { return nil }
func (m *pathVMM) CreateSnapshot(_, _ string) error         { return nil }
func (m *pathVMM) CreateSnapshotVMStateOnly(_ string) error { return nil }
func (m *pathVMM) Ping() error                              { return nil }
func (m *pathVMM) PID() int                                 { return 0 }
func (m *pathVMM) Close() error                             { return nil }

// huskExec runs "echo hi" over the Connect sandbox.v1.Sandbox/ExecStream RPC
// (the runtime path forkd and the SDK use; the legacy /v1/exec JSON route is
// retired, issue #358). The per-sandbox bearer token and sandbox id ride on the
// Authorization and X-Sandbox-Id headers, the SAME gate requireBearer enforced.
// It folds the streamed stdout and returns the terminal Connect error (nil on
// success). A rejected token surfaces as connect.CodeUnauthenticated, the
// Connect successor to the legacy 401. The bearer token VALUE is never logged.
func huskExec(t *testing.T, url, sandbox, bearer string) (stdout string, connErr error) {
	t.Helper()
	cli := sandboxv1connect.NewSandboxClient(http.DefaultClient, url)
	req := connect.NewRequest(&sandboxv1.ExecStreamRequest{Command: "echo hi"})
	if bearer != "" {
		req.Header().Set("Authorization", "Bearer "+bearer)
	}
	req.Header().Set("X-Sandbox-Id", sandbox)

	stream, err := cli.ExecStream(context.Background(), req)
	if err != nil {
		return "", err
	}
	defer func() { _ = stream.Close() }()

	var out strings.Builder
	for stream.Receive() {
		if b := stream.Msg().GetStdout(); len(b) > 0 {
			out.Write(b)
		}
	}
	if err := stream.Err(); err != nil {
		return out.String(), err
	}
	return out.String(), nil
}

// execWSSubprotocol is the WebSocket subprotocol the Connect Exec transport
// speaks (matches daemon.execWSSubprotocol). It must be offered on the upgrade.
const execWSSubprotocol = "connect.sandbox.v1"

// huskExecWS runs "echo hi" over the Connect-over-WebSocket Exec endpoint
// (/sandbox.v1.Sandbox/Exec), the runtime transport the cluster-mode SDK uses
// against the in-pod husk API: a thin HTTP/1.1 client that reaches the bidi
// sandbox.v1.Sandbox.Exec schema over a WebSocket upgrade. This is the husk
// single-sandbox path: the ?sandbox= id is the SDK-sent pod name, resolved by
// the single-sandbox gate to the one served VM. The per-sandbox bearer token
// rides on the Authorization header. A pre-upgrade auth rejection surfaces as
// the handshake HTTP status (401); a successful upgrade folds the streamed
// stdout. The bearer token VALUE is never logged.
//
// It returns the folded stdout, the pre-upgrade handshake HTTP status (200 once
// the upgrade succeeds, the real status on a pre-upgrade rejection), and any
// dial error without a handshake response.
func huskExecWS(t *testing.T, url, sandbox, bearer string) (stdout string, status int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(url, "http") + "/sandbox.v1.Sandbox/Exec?sandbox=" + sandbox
	opts := &websocket.DialOptions{Subprotocols: []string{execWSSubprotocol}}
	if bearer != "" {
		opts.HTTPHeader = http.Header{"Authorization": {"Bearer " + bearer}}
	}
	c, resp, err := websocket.Dial(ctx, wsURL, opts)
	if err != nil {
		// Pre-upgrade rejection (e.g. 401 auth): the handshake response carries
		// the status; report it so the caller can assert on it.
		if resp == nil {
			t.Fatalf("ws dial failed without a handshake response: %v", err)
		}
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		return "", resp.StatusCode
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	// One open frame, then drain stdout until the terminal exit frame.
	open := &sandboxv1.ExecRequest{Msg: &sandboxv1.ExecRequest_Open{Open: &sandboxv1.ExecOpen{Command: "echo hi"}}}
	if err := c.Write(ctx, websocket.MessageBinary, encodeHuskFrame(t, false, open)); err != nil {
		t.Fatalf("ws write open: %v", err)
	}
	var out strings.Builder
	for {
		typ, data, rerr := c.Read(ctx)
		if rerr != nil {
			var ce websocket.CloseError
			if errors.As(rerr, &ce) && ce.Code == websocket.StatusNormalClosure {
				break
			}
			break
		}
		if typ != websocket.MessageBinary {
			continue
		}
		frame := decodeHuskResponse(t, data)
		if frame.GetExit() != nil {
			break
		}
		out.Write(frame.GetStdout())
	}
	return out.String(), http.StatusOK
}

// encodeHuskFrame frames one ExecRequest as a Connect enveloped WebSocket frame:
// a 5-byte header (1 flags byte, big-endian uint32 length) then the protojson
// payload. It mirrors daemon.encodeConnectFrame on the client side.
func encodeHuskFrame(t *testing.T, end bool, msg *sandboxv1.ExecRequest) []byte {
	t.Helper()
	payload, err := protojson.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal exec frame: %v", err)
	}
	out := make([]byte, 5+len(payload))
	if end {
		out[0] = 0x02 // end-stream flag
	}
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out
}

// decodeHuskResponse splits one Connect enveloped WebSocket frame and decodes
// its payload as an ExecResponse.
func decodeHuskResponse(t *testing.T, b []byte) *sandboxv1.ExecResponse {
	t.Helper()
	if len(b) < 5 {
		t.Fatalf("short connect frame: %d bytes", len(b))
	}
	n := binary.BigEndian.Uint32(b[1:5])
	if int(n) != len(b)-5 {
		t.Fatalf("connect frame length %d does not match payload %d", n, len(b)-5)
	}
	var resp sandboxv1.ExecResponse
	if err := protojson.Unmarshal(b[5:], &resp); err != nil {
		t.Fatalf("decode ExecResponse: %v", err)
	}
	return &resp
}

func TestActivateServesTokenGatedSandboxAPI(t *testing.T) {
	const sandboxID = "husk"
	const token = "per-sandbox-bearer-CANARY-do-not-log"

	// A short /tmp dir: a unix socket path must stay under the OS sun_path limit
	// (104 bytes on darwin), which t.TempDir's long path can exceed.
	dir, err := os.MkdirTemp("/tmp", "husk-sb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "vsock.sock")
	startHuskGRPCUDS(t, sockPath)

	// A real daemon.SandboxAPI; the OnActivated hook registers the activated VM
	// and the delivered token, then we serve its Handler over httptest. This
	// mirrors cmd/husk-stub's makeSandboxServer wiring (register sandbox+token,
	// serve the bearer-gated Handler), without binding the fixed sandbox port.
	api := daemon.NewSandboxAPI(dir)
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)

	logged := captureStderr(t, func() {
		onActivated := func(vsockPath, tok string) error {
			if err := api.RegisterSandbox(sandboxID, vsockPath); err != nil {
				return err
			}
			api.RegisterToken(sandboxID, tok)
			return nil
		}

		vm := &pathVMM{vsockPath: sockPath}
		stub := New(firecracker.VMConfig{ID: sandboxID}, Options{
			Start:       func(firecracker.VMConfig) (vmm, error) { return vm, nil },
			Ready:       func(context.Context, string, time.Duration) error { return nil },
			Notify:      func(string, uint64, []byte, ActivateRequest) error { return nil },
			Verify:      verifyOK,
			OnActivated: onActivated,
		})

		if err := stub.Prepare(context.Background()); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		res, err := stub.Activate(context.Background(), ActivateRequest{
			SnapshotDir: "/data/templates/tmpl/snapshot",
			Token:       token,
		})
		if err != nil {
			t.Fatalf("Activate: %v", err)
		}
		if !res.OK {
			t.Fatalf("activate not OK: %s", res.Error)
		}

		// A tokened exec reaches the guest and returns its reply.
		body, err := huskExec(t, ts.URL, sandboxID, token)
		if err != nil {
			t.Fatalf("tokened exec failed: %v (body = %s)", err, body)
		}
		if !strings.Contains(body, "husk-exec-ok") {
			t.Fatalf("tokened exec did not reach the guest agent: %s", body)
		}

		// An untokened exec is rejected (Connect unauthenticated, the 401 successor).
		_, err = huskExec(t, ts.URL, sandboxID, "")
		if connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Fatalf("untokened exec code = %v, want unauthenticated", connect.CodeOf(err))
		}

		// A wrong-token exec is rejected.
		_, err = huskExec(t, ts.URL, sandboxID, "wrong-token")
		if connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Fatalf("wrong-token exec code = %v, want unauthenticated", connect.CodeOf(err))
		}
	})

	// The per-sandbox bearer token VALUE must never appear in any stub log line.
	if strings.Contains(logged, token) {
		t.Fatalf("bearer token value leaked into stub logs:\n%s", logged)
	}
}

// TestActivateSingleSandboxAcceptsSDKPodID is the regression test for the
// cluster-e2e auth bug: the husk-stub serves ONE sandbox registered under a
// fixed local id, but the SDK addresses the in-pod API with the claim's
// status.sandboxID (the husk pod name), which never equals that local id. With
// single-sandbox mode set (as cmd/husk-stub does), the per-sandbox bearer token
// authorizes an exec whose request body carries the POD NAME, the wrong/absent
// token is still rejected (401), and the token value never leaks to the log.
func TestActivateSingleSandboxAcceptsSDKPodID(t *testing.T) {
	const localID = "husk"
	const podID = "mitos-py-husk-5gwmh" // the id the SDK sends in cluster mode
	const token = "per-sandbox-bearer-CANARY-do-not-log"

	dir, err := os.MkdirTemp("/tmp", "husk-sb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "vsock.sock")
	startHuskGRPCUDS(t, sockPath)

	api := daemon.NewSandboxAPI(dir)
	// Single-sandbox mode: gate on the one registered token regardless of the
	// request's sandbox id, exactly as cmd/husk-stub configures it.
	api.SetSingleSandbox(localID)
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)

	logged := captureStderr(t, func() {
		onActivated := func(vsockPath, tok string) error {
			if err := api.RegisterSandbox(localID, vsockPath); err != nil {
				return err
			}
			api.RegisterToken(localID, tok)
			return nil
		}

		vm := &pathVMM{vsockPath: sockPath}
		stub := New(firecracker.VMConfig{ID: localID}, Options{
			Start:       func(firecracker.VMConfig) (vmm, error) { return vm, nil },
			Ready:       func(context.Context, string, time.Duration) error { return nil },
			Notify:      func(string, uint64, []byte, ActivateRequest) error { return nil },
			Verify:      verifyOK,
			OnActivated: onActivated,
		})

		if err := stub.Prepare(context.Background()); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		res, err := stub.Activate(context.Background(), ActivateRequest{
			SnapshotDir: "/data/templates/tmpl/snapshot",
			Token:       token,
		})
		if err != nil {
			t.Fatalf("Activate: %v", err)
		}
		if !res.OK {
			t.Fatalf("activate not OK: %s", res.Error)
		}

		// The SDK sends the POD NAME, not the local id, over the Connect-over-
		// WebSocket Exec endpoint (the runtime transport the cluster-mode SDK uses
		// against the in-pod husk API; single-sandbox resolution maps the pod name
		// to the one served VM). The correct token authorizes and the exec reaches
		// the single VM's guest agent.
		body, status := huskExecWS(t, ts.URL, podID, token)
		if status != 200 {
			t.Fatalf("exec with pod id + correct token: handshake status = %d, want 200", status)
		}
		if !strings.Contains(body, "husk-exec-ok") {
			t.Fatalf("exec did not reach the guest agent: %s", body)
		}

		// Wrong and absent tokens are still rejected (pre-upgrade 401), even with
		// the pod id.
		_, status = huskExecWS(t, ts.URL, podID, "")
		if status != 401 {
			t.Fatalf("exec with pod id + no token: handshake status = %d, want 401", status)
		}
		_, status = huskExecWS(t, ts.URL, podID, "wrong-token")
		if status != 401 {
			t.Fatalf("exec with pod id + wrong token: handshake status = %d, want 401", status)
		}
	})

	if strings.Contains(logged, token) {
		t.Fatalf("bearer token value leaked into stub logs:\n%s", logged)
	}
}
