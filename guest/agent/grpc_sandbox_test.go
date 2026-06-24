//go:build linux

package main

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// TestGRPCWriteThenReadFileRoundTrip writes a file via the streaming WriteFile
// RPC and reads it back via the streaming ReadFile RPC, proving both reuse the
// JSON handlers and round-trip content byte-for-byte.
func TestGRPCWriteThenReadFileRoundTrip(t *testing.T) {
	client := sandboxv1.NewSandboxClient(dialGuestGRPC(t))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	path := filepath.Join(t.TempDir(), "round.txt")
	want := []byte("hello round trip\nsecond line\n")

	ws, err := client.WriteFile(ctx)
	if err != nil {
		t.Fatalf("WriteFile open: %v", err)
	}
	if err := ws.Send(&sandboxv1.WriteFileRequest{Msg: &sandboxv1.WriteFileRequest_Open{Open: &sandboxv1.WriteFileOpen{Path: path, Mode: 0o644}}}); err != nil {
		t.Fatalf("WriteFile open send: %v", err)
	}
	if err := ws.Send(&sandboxv1.WriteFileRequest{Msg: &sandboxv1.WriteFileRequest_Data{Data: want}}); err != nil {
		t.Fatalf("WriteFile data send: %v", err)
	}
	res, err := ws.CloseAndRecv()
	if err != nil {
		t.Fatalf("WriteFile close: %v", err)
	}
	if res.BytesWritten != int64(len(want)) {
		t.Errorf("bytes_written = %d, want %d", res.BytesWritten, len(want))
	}

	rs, err := client.ReadFile(ctx, &sandboxv1.ReadFileRequest{Path: path})
	if err != nil {
		t.Fatalf("ReadFile open: %v", err)
	}
	var got []byte
	for {
		chunk, err := rs.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("ReadFile recv: %v", err)
		}
		got = append(got, chunk.Data...)
		if chunk.Eof {
			break
		}
	}
	if !bytes.Equal(got, want) {
		t.Errorf("read %q, want %q", got, want)
	}
}

// TestGRPCListAndStat creates entries on disk and asserts List enumerates them
// and Stat returns metadata for one path.
func TestGRPCListAndStat(t *testing.T) {
	client := sandboxv1.NewSandboxClient(dialGuestGRPC(t))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	resp, err := client.List(ctx, &sandboxv1.ListRequest{Parent: dir})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	names := map[string]*sandboxv1.FileInfo{}
	for _, e := range resp.Entries {
		names[e.Name] = e
	}
	if _, ok := names["a.txt"]; !ok {
		t.Errorf("a.txt missing from listing: %+v", resp.Entries)
	}
	if sub, ok := names["sub"]; !ok || !sub.IsDir {
		t.Errorf("sub dir missing or not a dir: %+v", resp.Entries)
	}

	info, err := client.Stat(ctx, &sandboxv1.StatRequest{Path: filepath.Join(dir, "a.txt")})
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != 3 || info.IsDir {
		t.Errorf("stat a.txt = size %d isDir %v, want size 3 isDir false", info.Size, info.IsDir)
	}
}

// TestGRPCMkdirAndRemove creates a directory then removes it, proving both reuse
// the JSON handler semantics.
func TestGRPCMkdirAndRemove(t *testing.T) {
	client := sandboxv1.NewSandboxClient(dialGuestGRPC(t))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	target := filepath.Join(t.TempDir(), "nested", "deep")
	if _, err := client.Mkdir(ctx, &sandboxv1.MkdirRequest{Path: target}); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if fi, err := os.Stat(target); err != nil || !fi.IsDir() {
		t.Fatalf("mkdir did not create dir: %v", err)
	}
	if _, err := client.Remove(ctx, &sandboxv1.RemoveRequest{Path: filepath.Dir(target), Recursive: true}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(target)); !os.IsNotExist(err) {
		t.Fatalf("remove did not delete tree: %v", err)
	}
}

// TestGRPCArchiveDownloadStreamsTar tars a workspace subtree and verifies the
// streamed bytes form a valid tar containing the file. It uses a temp dir as the
// workspace root so the allowlist passes.
func TestGRPCArchiveDownloadStreamsTar(t *testing.T) {
	withWorkspaceRoot(t)
	if err := os.WriteFile(filepath.Join(workspaceRoot, "file.txt"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	client := sandboxv1.NewSandboxClient(dialGuestGRPC(t))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := client.Archive(ctx, &sandboxv1.ArchiveRequest{Path: workspaceRoot, Direction: sandboxv1.ArchiveRequest_DOWNLOAD})
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	var tarBytes []byte
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Archive recv: %v", err)
		}
		tarBytes = append(tarBytes, chunk.Data...)
		if chunk.Eof {
			break
		}
	}
	tr := tar.NewReader(bytes.NewReader(tarBytes))
	found := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		if hdr.Name == "file.txt" {
			found = true
		}
	}
	if !found {
		t.Error("file.txt not present in streamed tar")
	}
}

// TestGRPCArchiveRejectsOutsideWorkspace proves the reused allowlist refuses a
// path outside the workspace transfer root.
func TestGRPCArchiveRejectsOutsideWorkspace(t *testing.T) {
	withWorkspaceRoot(t)
	client := sandboxv1.NewSandboxClient(dialGuestGRPC(t))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := client.Archive(ctx, &sandboxv1.ArchiveRequest{Path: "/etc", Direction: sandboxv1.ArchiveRequest_DOWNLOAD})
	if err != nil {
		t.Fatalf("Archive open: %v", err)
	}
	_, err = stream.Recv()
	if err == nil {
		t.Fatal("expected error for out-of-workspace path, got nil")
	}
}

// TestGRPCUploadUntars uploads a tar over the Upload stream and verifies the
// member is extracted under the workspace root.
func TestGRPCUploadUntars(t *testing.T) {
	withWorkspaceRoot(t)
	client := sandboxv1.NewSandboxClient(dialGuestGRPC(t))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	content := []byte("uploaded content")
	if err := tw.WriteHeader(&tar.Header{Name: "up.txt", Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	us, err := client.Upload(ctx)
	if err != nil {
		t.Fatalf("Upload open: %v", err)
	}
	if err := us.Send(&sandboxv1.UploadRequest{Msg: &sandboxv1.UploadRequest_Open{Open: &sandboxv1.UploadOpen{Dest: workspaceRoot}}}); err != nil {
		t.Fatalf("Upload open send: %v", err)
	}
	if err := us.Send(&sandboxv1.UploadRequest{Msg: &sandboxv1.UploadRequest_Chunk{Chunk: buf.Bytes()}}); err != nil {
		t.Fatalf("Upload chunk send: %v", err)
	}
	if _, err := us.CloseAndRecv(); err != nil {
		t.Fatalf("Upload close: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(workspaceRoot, "up.txt"))
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("extracted %q, want %q", got, content)
	}
}

// TestGRPCRunCodeEmitsResult drives RunCode against the fake kernel driver and
// asserts a result frame and a terminal exit_code arrive, proving the kernel
// reuse and the frame mapping.
func TestGRPCRunCodeEmitsResult(t *testing.T) {
	dir := t.TempDir()
	driver := writeFakeDriver(t, dir)
	guestKernel = newKernelManager(kernelConfig{python: "/bin/sh", driverPath: driver})
	t.Cleanup(func() { guestKernel.shutdown() })

	client := sandboxv1.NewSandboxClient(dialGuestGRPC(t))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := client.RunCode(ctx)
	if err != nil {
		t.Fatalf("RunCode open: %v", err)
	}
	if err := stream.Send(&sandboxv1.RunCodeRequest{Msg: &sandboxv1.RunCodeRequest_Open{Open: &sandboxv1.RunCodeOpen{Code: "print('hi')", Language: "python"}}}); err != nil {
		t.Fatalf("RunCode send: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("RunCode CloseSend: %v", err)
	}

	var gotResult, gotExit bool
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("RunCode recv: %v", err)
		}
		switch m := resp.Msg.(type) {
		case *sandboxv1.RunCodeResponse_Result:
			gotResult = true
			if string(m.Result.Data["image/png"]) == "" {
				t.Errorf("rich result data not mapped: %+v", m.Result.Data)
			}
		case *sandboxv1.RunCodeResponse_ExitCode:
			gotExit = true
		}
	}
	if !gotResult {
		t.Error("no result frame received")
	}
	if !gotExit {
		t.Error("no exit_code frame received")
	}
}

// TestGRPCVitalsEmitsSample asserts a single Vitals sample arrives when the
// interval is 0, reusing the real /proc collector.
func TestGRPCVitalsEmitsSample(t *testing.T) {
	client := sandboxv1.NewSandboxClient(dialGuestGRPC(t))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := client.Vitals(ctx, &sandboxv1.VitalsRequest{IntervalSeconds: 0})
	if err != nil {
		t.Fatalf("Vitals: %v", err)
	}
	sample, err := stream.Recv()
	if err != nil {
		t.Fatalf("Vitals recv: %v", err)
	}
	if sample.MemTotalBytes <= 0 {
		t.Errorf("mem_total_bytes = %d, want > 0", sample.MemTotalBytes)
	}
	if sample.ProcessCount <= 0 {
		t.Errorf("process_count = %d, want > 0", sample.ProcessCount)
	}
	// Interval 0 closes after one sample.
	if _, err := stream.Recv(); err != io.EOF {
		t.Errorf("expected EOF after one sample, got %v", err)
	}
}

// TestGRPCExecPtyEchoAndResize drives an interactive PTY exec over Exec: it
// writes a command to the shell via stdin and reads the echoed output, then
// sends a resize and a second command, proving the PTY path reuses runPTY and
// the resize frame is honored.
func TestGRPCExecPtyEchoAndResize(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("no /bin/sh: %v", err)
	}
	client := sandboxv1.NewSandboxClient(dialGuestGRPC(t))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	stream, err := client.Exec(ctx)
	if err != nil {
		t.Fatalf("Exec open: %v", err)
	}
	if err := stream.Send(&sandboxv1.ExecRequest{Msg: &sandboxv1.ExecRequest_Open{Open: &sandboxv1.ExecOpen{
		Command: "/bin/sh",
		Cwd:     t.TempDir(),
		Pty:     &sandboxv1.PtyOptions{Size: &sandboxv1.WindowSize{Cols: 80, Rows: 24}},
	}}}); err != nil {
		t.Fatalf("Exec open send: %v", err)
	}

	// Feed a command, a resize, then exit. The shell echoes input and prints the
	// marker; on exit the master closes and the exit frame arrives.
	if err := stream.Send(&sandboxv1.ExecRequest{Msg: &sandboxv1.ExecRequest_Stdin{Stdin: []byte("echo PTYMARK\n")}}); err != nil {
		t.Fatalf("stdin send: %v", err)
	}
	if err := stream.Send(&sandboxv1.ExecRequest{Msg: &sandboxv1.ExecRequest_Resize{Resize: &sandboxv1.WindowSize{Cols: 120, Rows: 40}}}); err != nil {
		t.Fatalf("resize send: %v", err)
	}
	if err := stream.Send(&sandboxv1.ExecRequest{Msg: &sandboxv1.ExecRequest_Stdin{Stdin: []byte("exit\n")}}); err != nil {
		t.Fatalf("exit send: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	var out []byte
	var gotExit bool
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Exec recv: %v", err)
		}
		switch m := resp.Msg.(type) {
		case *sandboxv1.ExecResponse_Stdout:
			out = append(out, m.Stdout...)
		case *sandboxv1.ExecResponse_Exit:
			gotExit = true
		}
	}
	if !bytes.Contains(out, []byte("PTYMARK")) {
		t.Errorf("PTY output %q does not contain echoed marker", out)
	}
	if !gotExit {
		t.Error("no exit frame from PTY exec")
	}
}

// TestGRPCPortForwardProxiesBytes starts a local echo listener and proves
// PortForward dials 127.0.0.1:<port> and proxies bytes both directions.
func TestGRPCPortForwardProxiesBytes(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(c, c) // echo
	}()

	client := sandboxv1.NewSandboxClient(dialGuestGRPC(t))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := client.PortForward(ctx)
	if err != nil {
		t.Fatalf("PortForward open: %v", err)
	}
	if err := stream.Send(&sandboxv1.Frame{Msg: &sandboxv1.Frame_Open{Open: &sandboxv1.PortForwardOpen{Port: uint32(port)}}}); err != nil {
		t.Fatalf("open send: %v", err)
	}
	payload := []byte("ping-through-tunnel")
	if err := stream.Send(&sandboxv1.Frame{Msg: &sandboxv1.Frame_Data{Data: payload}}); err != nil {
		t.Fatalf("data send: %v", err)
	}

	var got []byte
	for len(got) < len(payload) {
		resp, err := stream.Recv()
		if err != nil {
			t.Fatalf("PortForward recv: %v", err)
		}
		if d, ok := resp.Msg.(*sandboxv1.Frame_Data); ok {
			got = append(got, d.Data...)
		}
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("echoed %q, want %q", got, payload)
	}
	// Closing the send side tears the proxy down; the RPC ends cleanly.
	_ = stream.CloseSend()
}

// TestGRPCPortForwardRejectsBadPort proves the loopback-only guard refuses an
// invalid port without dialing.
func TestGRPCPortForwardRejectsBadPort(t *testing.T) {
	client := sandboxv1.NewSandboxClient(dialGuestGRPC(t))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.PortForward(ctx)
	if err != nil {
		t.Fatalf("PortForward open: %v", err)
	}
	if err := stream.Send(&sandboxv1.Frame{Msg: &sandboxv1.Frame_Open{Open: &sandboxv1.PortForwardOpen{Port: 0}}}); err != nil {
		t.Fatalf("open send: %v", err)
	}
	_ = stream.CloseSend()
	if _, err := stream.Recv(); err == nil {
		t.Fatal("expected error for invalid port, got nil")
	}
}

// withWorkspaceRoot points the package-level workspaceRoot at a fresh temp dir
// for the duration of one test so the Archive/Upload allowlist can be exercised
// without writing to the real /workspace. It restores the original after.
func withWorkspaceRoot(t *testing.T) {
	t.Helper()
	orig := workspaceRoot
	dir := t.TempDir()
	workspaceRoot = dir
	t.Cleanup(func() { workspaceRoot = orig })
}
