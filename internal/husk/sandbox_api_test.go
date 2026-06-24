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
	"bytes"
	"context"
	"encoding/json"
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

	"google.golang.org/grpc"
	"mitos.run/mitos/internal/daemon"
	"mitos.run/mitos/internal/firecracker"
	"mitos.run/mitos/internal/vsock"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
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
func (m *pathVMM) VsockHostPath(string) string      { return m.vsockPath }
func (m *pathVMM) PatchDrive(_, _ string) error     { return nil }
func (m *pathVMM) Resume() error                    { return nil }
func (m *pathVMM) Pause() error                     { return nil }
func (m *pathVMM) CreateSnapshot(_, _ string) error { return nil }
func (m *pathVMM) Close() error                     { return nil }

func postHuskExec(t *testing.T, url, sandbox, bearer string) (int, string) {
	t.Helper()
	bodyBytes, _ := json.Marshal(map[string]any{"sandbox": sandbox, "command": "echo hi"})
	req, err := http.NewRequest(http.MethodPost, url+"/v1/exec", bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
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
		code, body := postHuskExec(t, ts.URL, sandboxID, token)
		if code != 200 {
			t.Fatalf("tokened exec status = %d, body = %s, want 200", code, body)
		}
		if !strings.Contains(body, "husk-exec-ok") {
			t.Fatalf("tokened exec did not reach the guest agent: %s", body)
		}

		// An untokened exec is rejected.
		code, _ = postHuskExec(t, ts.URL, sandboxID, "")
		if code != 401 {
			t.Fatalf("untokened exec status = %d, want 401", code)
		}

		// A wrong-token exec is rejected.
		code, _ = postHuskExec(t, ts.URL, sandboxID, "wrong-token")
		if code != 401 {
			t.Fatalf("wrong-token exec status = %d, want 401", code)
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

		// The SDK sends the POD NAME, not the local id; the correct token
		// authorizes and the exec reaches the single VM's guest agent.
		code, body := postHuskExec(t, ts.URL, podID, token)
		if code != 200 {
			t.Fatalf("exec with pod id + correct token: status = %d, body = %s, want 200", code, body)
		}
		if !strings.Contains(body, "husk-exec-ok") {
			t.Fatalf("exec did not reach the guest agent: %s", body)
		}

		// Wrong and absent tokens are still rejected, even with the pod id.
		code, _ = postHuskExec(t, ts.URL, podID, "")
		if code != 401 {
			t.Fatalf("exec with pod id + no token: status = %d, want 401", code)
		}
		code, _ = postHuskExec(t, ts.URL, podID, "wrong-token")
		if code != 401 {
			t.Fatalf("exec with pod id + wrong token: status = %d, want 401", code)
		}
	})

	if strings.Contains(logged, token) {
		t.Fatalf("bearer token value leaked into stub logs:\n%s", logged)
	}
}
