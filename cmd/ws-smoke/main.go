// Command ws-smoke drives the bulk workspace hydrate/dehydrate data path against
// real guest VMs over vsock, for the KVM integration phase. It connects to two
// already-booted guest agents (each on its own Firecracker vsock UDS), writes a
// set of known files into the SOURCE guest's /workspace, Dehydrates that
// workspace into a content-addressed manifest in a node CAS store, Hydrates the
// manifest into the DESTINATION guest's /workspace, and asserts every file came
// back byte-identical. A mismatch, a transfer error, or a missing file is a real
// FAILURE (exit 1); a usage error is a SETUP error (exit 2), so the workflow can
// tell a broken harness from a broken transfer.
//
// Usage:
//
//	ws-smoke --cas <dir> --src <src-vsock-uds> --dst <dst-vsock-uds>
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"

	"mitos.run/mitos/internal/cas"
	"mitos.run/mitos/internal/guestgrpc"
	"mitos.run/mitos/internal/workspace"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// grpcTransport adapts a *guestgrpc.Client to the workspace.VsockTransport
// interface (TarDir / UntarDir) using the gRPC Archive and Upload RPCs.
type grpcTransport struct {
	client *guestgrpc.Client
}

// TarDir downloads the directory tree at path from the guest as a tar stream
// via the gRPC Archive (DOWNLOAD) RPC.
func (t *grpcTransport) TarDir(path string) ([]byte, error) {
	ctx := context.Background()
	stream, err := t.client.Sandbox.Archive(ctx, &sandboxv1.ArchiveRequest{
		Path:      path,
		Direction: sandboxv1.ArchiveRequest_DOWNLOAD,
	})
	if err != nil {
		return nil, fmt.Errorf("archive download %s: %w", path, err)
	}
	var buf []byte
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("recv archive chunk: %w", err)
		}
		buf = append(buf, chunk.GetData()...)
		if chunk.GetEof() {
			break
		}
	}
	return buf, nil
}

// UntarDir uploads the tar bytes into the guest at path via the gRPC Upload RPC.
func (t *grpcTransport) UntarDir(path string, data []byte) error {
	ctx := context.Background()
	stream, err := t.client.Sandbox.Upload(ctx)
	if err != nil {
		return fmt.Errorf("upload open %s: %w", path, err)
	}
	if err := stream.Send(&sandboxv1.UploadRequest{
		Msg: &sandboxv1.UploadRequest_Open{Open: &sandboxv1.UploadOpen{Dest: path}},
	}); err != nil {
		return fmt.Errorf("send upload open: %w", err)
	}
	const chunkSize = 64 * 1024
	for len(data) > 0 {
		n := chunkSize
		if n > len(data) {
			n = len(data)
		}
		if err := stream.Send(&sandboxv1.UploadRequest{Msg: &sandboxv1.UploadRequest_Chunk{Chunk: data[:n]}}); err != nil {
			return fmt.Errorf("send upload chunk: %w", err)
		}
		data = data[n:]
	}
	if _, err := stream.CloseAndRecv(); err != nil {
		return fmt.Errorf("close upload stream: %w", err)
	}
	return nil
}

func main() {
	casDir := flag.String("cas", "", "directory for the node CAS store")
	srcUDS := flag.String("src", "", "source guest vsock UDS path")
	dstUDS := flag.String("dst", "", "destination guest vsock UDS path")
	flag.Parse()

	if *casDir == "" || *srcUDS == "" || *dstUDS == "" {
		fmt.Fprintln(os.Stderr, "SETUP: ws-smoke requires --cas, --src, and --dst")
		os.Exit(2)
	}

	store, err := cas.New(*casDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "SETUP: open CAS store: %v\n", err)
		os.Exit(2)
	}

	srcClient := connect(*srcUDS, "source")
	defer srcClient.Close() //nolint:errcheck // best-effort close
	dstClient := connect(*dstUDS, "destination")
	defer dstClient.Close() //nolint:errcheck // best-effort close

	src := &grpcTransport{client: srcClient}
	dst := &grpcTransport{client: dstClient}

	// Known workspace content, including a nested path and binary bytes, so the
	// proof covers directory structure and non-text content.
	want := map[string][]byte{
		"/workspace/main.go":           []byte("package main\n\nfunc main() {}\n"),
		"/workspace/sub/nested.txt":    []byte("nested workspace content\n"),
		"/workspace/sub/deep/data.bin": {0x00, 0x01, 0x02, 0xff, 0xfe},
		"/workspace/notes.md":          []byte("# notes\nhydrate/dehydrate round trip\n"),
	}
	// A file that should NOT survive a revision: it sits at a secret path the
	// dehydrate exclude list strips. The destination must never see it.
	secretPath := "/workspace/.netrc"
	if err := grpcWriteFile(srcClient, secretPath, []byte("machine secret password hunter2\n"), 0o600); err != nil {
		fail("write secret file into source workspace: %v", err)
	}
	for path, content := range want {
		if err := grpcWriteFile(srcClient, path, content, 0o644); err != nil {
			fail("write %s into source workspace: %v", path, err)
		}
	}
	fmt.Printf("WS_SMOKE wrote %d files into source /workspace\n", len(want))

	ctx := context.Background()
	excludes := []string{"/workspace/.netrc"}
	digest, err := workspace.Dehydrate(ctx, src, store, excludes, nil)
	if err != nil {
		fail("dehydrate source workspace: %v", err)
	}
	fmt.Printf("WS_SMOKE dehydrated to digest %s\n", digest)

	if err := workspace.Hydrate(ctx, dst, store, digest); err != nil {
		fail("hydrate digest into destination workspace: %v", err)
	}
	fmt.Println("WS_SMOKE hydrated digest into destination /workspace")

	// Assert every known file came back byte-identical on the destination.
	paths := make([]string, 0, len(want))
	for p := range want {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, path := range paths {
		got, err := grpcReadFile(dstClient, path)
		if err != nil {
			fail("read %s from destination workspace: %v", path, err)
		}
		if !bytes.Equal(got, want[path]) {
			fail("MISMATCH %s: destination has %d bytes, want %d", path, len(got), len(want[path]))
		}
		fmt.Printf("WS_SMOKE OK %s (%d bytes identical)\n", path, len(got))
	}

	// The excluded secret must NOT have crossed into the revision.
	if _, err := grpcReadFile(dstClient, secretPath); err == nil {
		fail("SECRET LEAK: %s reached the destination workspace; dehydrate exclude failed", secretPath)
	}
	fmt.Printf("WS_SMOKE OK secret %s excluded from the revision\n", secretPath)

	fmt.Println("WS_SMOKE PASS: workspace round trip byte-identical, secret excluded")
}

// connect dials a guest agent over the Firecracker vsock UDS via gRPC.
// A dial failure is a SETUP error (the VM did not boot or the agent is not
// listening), distinct from a transfer FAILURE.
func connect(udsPath, role string) *guestgrpc.Client {
	client, err := guestgrpc.Dial(udsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "SETUP: connect to %s guest agent at %s: %v\n", role, udsPath, err)
		os.Exit(2)
	}
	return client
}

// grpcReadFile reads a file from the guest via the gRPC ReadFile streaming RPC.
func grpcReadFile(client *guestgrpc.Client, path string) ([]byte, error) {
	ctx := context.Background()
	stream, err := client.Sandbox.ReadFile(ctx, &sandboxv1.ReadFileRequest{Path: path})
	if err != nil {
		return nil, fmt.Errorf("read file stream: %w", err)
	}
	var buf []byte
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("recv read_file chunk: %w", err)
		}
		buf = append(buf, chunk.GetData()...)
		if chunk.GetEof() {
			break
		}
	}
	return buf, nil
}

// grpcWriteFile writes bytes into a file in the guest via the gRPC WriteFile streaming RPC.
func grpcWriteFile(client *guestgrpc.Client, path string, content []byte, mode uint32) error {
	ctx := context.Background()
	stream, err := client.Sandbox.WriteFile(ctx)
	if err != nil {
		return fmt.Errorf("write file stream: %w", err)
	}
	if err := stream.Send(&sandboxv1.WriteFileRequest{
		Msg: &sandboxv1.WriteFileRequest_Open{Open: &sandboxv1.WriteFileOpen{Path: path, Mode: mode}},
	}); err != nil {
		return fmt.Errorf("send write_file open: %w", err)
	}
	if len(content) > 0 {
		if err := stream.Send(&sandboxv1.WriteFileRequest{Msg: &sandboxv1.WriteFileRequest_Data{Data: content}}); err != nil {
			return fmt.Errorf("send write_file data: %w", err)
		}
	}
	if _, err := stream.CloseAndRecv(); err != nil {
		return fmt.Errorf("close write_file stream: %w", err)
	}
	return nil
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "FAILURE: "+format+"\n", args...)
	os.Exit(1)
}
