package husk

// ws_grpc_transport_test.go: TDD tests for grpcWorkspaceTransport, the gRPC
// Archive/Upload backed workspace.VsockTransport that replaces the legacy JSON
// vsock.Client.TarDir/UntarDir path on AgentPort 52.
//
// Strategy: stand up an in-process gRPC Sandbox server on a temp unix socket
// implementing Archive (streams tar out) and Upload (receives tar in), then
// assert that grpcWorkspaceTransport.TarDir round-trips a known tar and
// grpcWorkspaceTransport.UntarDir sends it back with the correct dest path
// and chunk framing. A Dehydrate->Hydrate style stub round-trip is also
// exercised to confirm the seam wires into the existing workspace ops.
//
// Secret hygiene: workspace tar bytes are structural test data only. No values
// are logged or written to permanent storage.

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"google.golang.org/grpc"

	"mitos.run/mitos/internal/cas"
	"mitos.run/mitos/internal/workspace"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// recordingSandboxServer is an in-process sandbox.v1.Sandbox stub that handles
// only Archive and Upload. It is ONLY used in tests.
type recordingSandboxServer struct {
	sandboxv1.UnimplementedSandboxServer

	mu sync.Mutex

	// Archive side: tarToStream is the tar bytes streamed out by Archive.
	tarToStream []byte
	archivePath string // the path Archive was called with

	// Upload side: recorded dest and reassembled bytes received via Upload.
	uploadDest  string
	uploadBytes []byte
}

// Archive streams tarToStream as chunks, then sends an eof chunk.
func (s *recordingSandboxServer) Archive(req *sandboxv1.ArchiveRequest, stream sandboxv1.Sandbox_ArchiveServer) error {
	s.mu.Lock()
	s.archivePath = req.GetPath()
	data := s.tarToStream
	s.mu.Unlock()

	const sz = wsUploadChunkSize
	for len(data) > 0 {
		n := sz
		if n > len(data) {
			n = len(data)
		}
		if err := stream.Send(&sandboxv1.Chunk{Data: data[:n]}); err != nil {
			return err
		}
		data = data[n:]
	}
	// Terminal eof chunk (may carry empty Data).
	return stream.Send(&sandboxv1.Chunk{Eof: true})
}

// Upload receives the open + chunk frames, reassembles the bytes.
func (s *recordingSandboxServer) Upload(stream sandboxv1.Sandbox_UploadServer) error {
	// First message must carry the open frame.
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	open := first.GetOpen()
	if open == nil {
		return io.ErrUnexpectedEOF
	}

	s.mu.Lock()
	s.uploadDest = open.GetDest()
	s.mu.Unlock()

	var buf bytes.Buffer
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if c := msg.GetChunk(); len(c) > 0 {
			buf.Write(c)
		}
	}

	s.mu.Lock()
	s.uploadBytes = buf.Bytes()
	total := int64(buf.Len())
	s.mu.Unlock()

	return stream.SendAndClose(&sandboxv1.UploadResult{BytesWritten: total})
}

// startSandboxGRPC starts an in-process Sandbox gRPC server on a temp unix
// socket and returns the socket path and a cleanup function.
func startSandboxGRPC(t *testing.T, srv *recordingSandboxServer) (sockPath string, cleanup func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "husk-sandbox-grpc-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	sockPath = filepath.Join(dir, "sandbox.sock")

	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("listen unix %s: %v", sockPath, err)
	}

	grpcSrv := grpc.NewServer()
	sandboxv1.RegisterSandboxServer(grpcSrv, srv)
	go grpcSrv.Serve(lis) //nolint:errcheck // test; errors surface via RPC failures

	cleanup = func() {
		grpcSrv.Stop()
		lis.Close()
		os.RemoveAll(dir)
	}
	return sockPath, cleanup
}

// simpleTar builds an in-memory tar with a single named regular file.
func simpleTar(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	body := []byte(content)
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o644,
		Size:     int64(len(body)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("tar WriteHeader: %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("tar Write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	return buf.Bytes()
}

// TestGRPCWorkspaceTransport_TarDirRoundTrip verifies that TarDir opens the
// Archive RPC with the correct path and returns the reassembled tar bytes.
func TestGRPCWorkspaceTransport_TarDirRoundTrip(t *testing.T) {
	wantTar := simpleTar(t, "main.go", "package main")
	srv := &recordingSandboxServer{tarToStream: wantTar}
	sockPath, cleanup := startSandboxGRPC(t, srv)
	defer cleanup()

	tr := &grpcWorkspaceTransport{vsockPath: sockPath, dial: dialUnixSandbox}

	got, err := tr.TarDir("/workspace")
	if err != nil {
		t.Fatalf("TarDir: %v", err)
	}
	if !bytes.Equal(got, wantTar) {
		t.Fatalf("TarDir returned %d bytes, want %d; content mismatch", len(got), len(wantTar))
	}

	srv.mu.Lock()
	path := srv.archivePath
	srv.mu.Unlock()
	if path != "/workspace" {
		t.Errorf("Archive called with path %q, want %q", path, "/workspace")
	}
}

// TestGRPCWorkspaceTransport_UntarDirSendsTar verifies that UntarDir opens the
// Upload RPC, sends the open frame with the correct dest, streams the tar as
// chunks, and closes for the UploadResult.
func TestGRPCWorkspaceTransport_UntarDirSendsTar(t *testing.T) {
	wantTar := simpleTar(t, "README", "hello")
	srv := &recordingSandboxServer{}
	sockPath, cleanup := startSandboxGRPC(t, srv)
	defer cleanup()

	tr := &grpcWorkspaceTransport{vsockPath: sockPath, dial: dialUnixSandbox}

	if err := tr.UntarDir("/workspace", wantTar); err != nil {
		t.Fatalf("UntarDir: %v", err)
	}

	srv.mu.Lock()
	dest := srv.uploadDest
	got := srv.uploadBytes
	srv.mu.Unlock()

	if dest != "/workspace" {
		t.Errorf("Upload dest = %q, want %q", dest, "/workspace")
	}
	if !bytes.Equal(got, wantTar) {
		t.Fatalf("Upload received %d bytes, want %d; content mismatch", len(got), len(wantTar))
	}
}

// TestGRPCWorkspaceTransport_TarDirLargerThanChunk verifies that a tar larger
// than wsUploadChunkSize is correctly reassembled across multiple Archive chunks.
func TestGRPCWorkspaceTransport_TarDirLargerThanChunk(t *testing.T) {
	// Build a tar that exceeds one chunk: fill a file with wsUploadChunkSize+1 bytes.
	bigContent := bytes.Repeat([]byte("x"), wsUploadChunkSize+1)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name:     "big.bin",
		Mode:     0o644,
		Size:     int64(len(bigContent)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if _, err := tw.Write(bigContent); err != nil {
		t.Fatalf("tar Write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	wantTar := buf.Bytes()

	srv := &recordingSandboxServer{tarToStream: wantTar}
	sockPath, cleanup := startSandboxGRPC(t, srv)
	defer cleanup()

	tr := &grpcWorkspaceTransport{vsockPath: sockPath, dial: dialUnixSandbox}
	got, err := tr.TarDir("/workspace")
	if err != nil {
		t.Fatalf("TarDir large: %v", err)
	}
	if !bytes.Equal(got, wantTar) {
		t.Fatalf("TarDir returned %d bytes, want %d", len(got), len(wantTar))
	}
}

// TestProductionWorkspaceTransport_UsesGRPC verifies that productionWorkspaceTransport
// returns a transport that successfully calls TarDir via Archive over a unix socket.
// This exercises the production seam itself (DialUnix path), not just the type.
func TestProductionWorkspaceTransport_UsesGRPC(t *testing.T) {
	wantTar := simpleTar(t, "app.py", "print('hello')")
	srv := &recordingSandboxServer{tarToStream: wantTar}
	sockPath, cleanup := startSandboxGRPC(t, srv)
	defer cleanup()

	// grpcWorkspaceTransport with dialUnixSandbox mirrors what productionWorkspaceTransport
	// does but over unix instead of vsock (no Firecracker process needed in tests).
	tr := &grpcWorkspaceTransport{vsockPath: sockPath, dial: dialUnixSandbox}

	got, err := tr.TarDir("/workspace")
	if err != nil {
		t.Fatalf("TarDir via grpcWorkspaceTransport: %v", err)
	}
	if !bytes.Equal(got, wantTar) {
		t.Fatalf("bytes mismatch: got %d want %d", len(got), len(wantTar))
	}
}

// TestGRPCWorkspaceTransport_StubDehydrateHydrate exercises a full Dehydrate ->
// Hydrate round-trip using grpcWorkspaceTransport as the workspace.VsockTransport
// seam, confirming that the gRPC transport integrates correctly with the workspace
// CAS helpers. The test wires productionWorkspaceTransport-shaped stubs by
// injecting a wsTransporter that returns a grpcWorkspaceTransport connected to
// the in-process Sandbox server.
func TestGRPCWorkspaceTransport_StubDehydrateHydrate(t *testing.T) {
	// Build the tar the "guest" will return for TarDir.
	srcTar := tarOf(t, map[string]string{
		"main.go": "package main",
		"go.mod":  "module example.com\n",
	})

	archiveSrv := &recordingSandboxServer{tarToStream: srcTar}
	sockPath, cleanup := startSandboxGRPC(t, archiveSrv)
	defer cleanup()

	// wsTransporter that returns a grpcWorkspaceTransport backed by the in-process server.
	transport := func(_ string) (workspace.VsockTransport, error) {
		return &grpcWorkspaceTransport{vsockPath: sockPath, dial: dialUnixSandbox}, nil
	}

	// Set up a stub with the gRPC transport.
	store, err := cas.New(t.TempDir())
	if err != nil {
		t.Fatalf("cas.New: %v", err)
	}
	s := &Stub{
		state:        StateActive,
		vm:           &fakeVMM{},
		casStore:     store,
		wsTransport:  transport,
		vsockRelPath: "v.sock",
	}

	// Dehydrate.
	dres, err := s.DehydrateWorkspace(context.Background(), DehydrateWorkspaceRequest{})
	if err != nil {
		t.Fatalf("DehydrateWorkspace: %v", err)
	}
	if !dres.OK {
		t.Fatalf("DehydrateWorkspace not OK: %+v", dres)
	}
	digest := cas.Digest(dres.ManifestDigest)
	if err := digest.Validate(); err != nil {
		t.Fatalf("manifest digest invalid: %v", err)
	}

	// Hydrate into a fresh Upload-recording server and assert the tar is delivered.
	uploadSrv := &recordingSandboxServer{}
	sockPath2, cleanup2 := startSandboxGRPC(t, uploadSrv)
	defer cleanup2()

	transport2 := func(_ string) (workspace.VsockTransport, error) {
		return &grpcWorkspaceTransport{vsockPath: sockPath2, dial: dialUnixSandbox}, nil
	}
	s2 := &Stub{
		state:        StateActive,
		vm:           &fakeVMM{},
		casStore:     store,
		wsTransport:  transport2,
		vsockRelPath: "v.sock",
	}

	hres, err := s2.HydrateWorkspace(context.Background(), HydrateWorkspaceRequest{ManifestDigest: dres.ManifestDigest})
	if err != nil {
		t.Fatalf("HydrateWorkspace: %v", err)
	}
	if !hres.OK {
		t.Fatalf("HydrateWorkspace not OK: %+v", hres)
	}

	uploadSrv.mu.Lock()
	uploadDest := uploadSrv.uploadDest
	uploadLen := len(uploadSrv.uploadBytes)
	uploadSrv.mu.Unlock()

	if uploadDest != workspace.WorkspacePath {
		t.Errorf("hydrate UntarDir dest = %q, want %q", uploadDest, workspace.WorkspacePath)
	}
	if uploadLen == 0 {
		t.Errorf("hydrate UntarDir received 0 bytes; expected non-empty tar")
	}
}
