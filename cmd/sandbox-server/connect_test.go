package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"mitos.run/mitos/internal/vsock"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
	"mitos.run/mitos/proto/sandbox/v1/sandboxv1connect"
)

// startFakeStreamUDS serves the Firecracker vsock UDS preamble then writes the
// given exec stream frames for any request, with an optional per-frame gate so a
// test can prove the server forwarded the first chunk before the rest were
// produced. It mirrors the daemon exec_stream fake-agent harness so the Connect
// Exec bridge is exercised against the SAME wire shape the HTTP path uses, with
// no KVM.
func startFakeStreamUDS(t *testing.T, sockPath string, frames []vsock.ExecStreamFrame, gate <-chan struct{}) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lis.Close() })
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				sc := bufio.NewScanner(c)
				sc.Buffer(make([]byte, 1<<20), vsock.MaxMessageBytes)
				if !sc.Scan() {
					return
				}
				if strings.HasPrefix(sc.Text(), "CONNECT ") {
					if _, err := c.Write([]byte("OK 52\n")); err != nil {
						return
					}
				}
				if !sc.Scan() {
					return
				}
				for i, f := range frames {
					b, _ := json.Marshal(f)
					if _, err := c.Write(append(b, '\n')); err != nil {
						return
					}
					// Gate after the first frame so the test can observe the first
					// chunk arriving incrementally before the rest are sent.
					if i == 0 && gate != nil {
						<-gate
					}
				}
			}(conn)
		}
	}()
}

// connectClientFor mounts the server's Connect handler on an unencrypted-HTTP/2
// test server (stdlib h2c, Go 1.26) and returns a matching client. Unencrypted
// HTTP/2 lets the bidi Exec stream work without TLS, exactly as main() serves
// it. The server is closed via t.Cleanup.
func connectClientFor(t *testing.T, s *server) (sandboxv1connect.SandboxClient, func()) {
	t.Helper()
	mux := http.NewServeMux()
	s.mountConnect(mux)
	srv := httptest.NewUnstartedServer(mux)
	var p http.Protocols
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	srv.Config.Protocols = &p
	srv.Start()
	t.Cleanup(srv.Close)

	var cp http.Protocols
	cp.SetUnencryptedHTTP2(true)
	httpClient := &http.Client{Transport: &http.Transport{Protocols: &cp}}
	client := sandboxv1connect.NewSandboxClient(httpClient, srv.URL, connect.WithGRPC())
	return client, func() {}
}

// TestConnectExecStreamsStdoutIncrementally is the #24 acceptance core on the
// sandbox-server transport: an Exec call over Connect runs a command in the
// target sandbox and streams stdout chunks INCREMENTALLY through the real
// SandboxAPI -> vsock exec path, then a terminal ExecExit with the guest's exit
// code. The fake stream UDS gates after the first frame so the test proves the
// first chunk reached the client before the rest were produced.
func TestConnectExecStreamsStdoutIncrementally(t *testing.T) {
	const id = "sb-connect"
	dir, err := os.MkdirTemp("/tmp", "sbcon")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	sock := filepath.Join(dir, id, "vsock.sock")
	gate := make(chan struct{})
	startFakeStreamUDS(t, sock, []vsock.ExecStreamFrame{
		{Kind: vsock.FrameChunk, Stream: vsock.StreamStdout, Data: []byte("first ")},
		{Kind: vsock.FrameChunk, Stream: vsock.StreamStdout, Data: []byte("second\n")},
		{Kind: vsock.FrameChunk, Stream: vsock.StreamStderr, Data: []byte("warn\n")},
		{Kind: vsock.FrameExit, ExitCode: 3, ExecTimeMs: 4.0},
	}, gate)

	s := newServer(dir, "", false, 16, 86400) // real mode, no engine: dials the stream UDS directly
	if err := s.sandboxAPI.RegisterSandbox(id, sock); err != nil {
		t.Fatal(err)
	}
	s.sandboxAPI.RegisterStreamPath(id, sock)

	client, closeSrv := connectClientFor(t, s)
	defer closeSrv()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := client.Exec(ctx)
	stream.RequestHeader().Set(connectSandboxHeader, id)
	if err := stream.Send(&sandboxv1.ExecRequest{
		Msg: &sandboxv1.ExecRequest_Open{Open: &sandboxv1.ExecOpen{Command: "echo first second"}},
	}); err != nil {
		t.Fatalf("send open: %v", err)
	}
	if err := stream.CloseRequest(); err != nil {
		t.Fatalf("close request: %v", err)
	}

	// First response must be the first stdout chunk, received BEFORE we release
	// the fake guest to emit the rest. This is the incremental-streaming proof.
	first, err := stream.Receive()
	if err != nil {
		t.Fatalf("receive first: %v", err)
	}
	if got := string(first.GetStdout()); got != "first " {
		t.Fatalf("first chunk = %q, want %q", got, "first ")
	}
	close(gate)

	var stdout, stderr []byte
	var exit *sandboxv1.ExecExit
	for {
		resp, rerr := stream.Receive()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			t.Fatalf("receive: %v", rerr)
		}
		switch m := resp.Msg.(type) {
		case *sandboxv1.ExecResponse_Stdout:
			stdout = append(stdout, m.Stdout...)
		case *sandboxv1.ExecResponse_Stderr:
			stderr = append(stderr, m.Stderr...)
		case *sandboxv1.ExecResponse_Exit:
			exit = m.Exit
		}
	}
	if exit == nil {
		t.Fatal("no terminal ExecExit frame")
	}
	if exit.GetExitCode() != 3 {
		t.Fatalf("exit code = %d, want 3", exit.GetExitCode())
	}
	if string(stdout) != "second\n" {
		t.Fatalf("remaining stdout = %q, want %q", string(stdout), "second\n")
	}
	if string(stderr) != "warn\n" {
		t.Fatalf("stderr = %q, want %q", string(stderr), "warn\n")
	}
}

// TestConnectExecRequiresSandboxHeader proves the foundation slice resolves the
// target sandbox from the Mitos-Sandbox header and rejects a call that omits it
// with an LLM-legible error rather than running against an unknown sandbox.
func TestConnectExecRequiresSandboxHeader(t *testing.T) {
	dir := t.TempDir()
	s := newServer(dir, "", false, 16, 86400)
	client, closeSrv := connectClientFor(t, s)
	defer closeSrv()

	stream := client.Exec(context.Background())
	if err := stream.Send(&sandboxv1.ExecRequest{
		Msg: &sandboxv1.ExecRequest_Open{Open: &sandboxv1.ExecOpen{Command: "echo hi"}},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	_ = stream.CloseRequest()
	_, err := stream.Receive()
	if err == nil {
		t.Fatal("expected error when Mitos-Sandbox header is absent")
	}
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("code = %v, want PermissionDenied", connect.CodeOf(err))
	}
}
