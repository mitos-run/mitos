package husk

// ws_grpc_transport.go: gRPC-backed workspace.VsockTransport for the husk stub.
//
// grpcWorkspaceTransport replaces the legacy JSON vsock.Client.TarDir/UntarDir
// path (AgentPort 52) with the gRPC Sandbox service Archive + Upload RPCs on
// AgentGRPCPort 53. This is the last JSON path in internal/husk; after this
// migration workspace dehydrate/hydrate works against the gRPC-only Rust agent.
//
// Contract match vs the JSON path:
//
//   - TarDir(path): JSON TarDir sent the path to the guest and received the raw
//     tar bytes back (bounded to MaxTarBytes = 64 MiB by the JSON line buffer).
//     Archive(ArchiveRequest{Path: path, Direction: DOWNLOAD}) does the same:
//     the guest tars the subtree at path and streams the bytes back as Chunk
//     frames ending with eof=true. The host concatenates them into the same
//     []byte the caller receives. The host-side cap (vsock.MaxTarBytes) is
//     re-applied as a running byte counter over the incoming chunks as
//     defense-in-depth, matching the old JSON path behavior.
//
//   - UntarDir(path, tar): JSON UntarDir sent the tar bytes to the guest and
//     the guest extracted them into path, sanitizing every member against
//     traversal. Upload does the same: the first UploadRequest carries the
//     open frame (dest=path), subsequent requests carry the tar bytes as chunks.
//     Traversal sanitization is enforced guest-side in the Upload handler,
//     providing the same guarantee as the JSON path. The host-side guard from
//     the old JSON path (MaxTarBytes check before sending) is re-applied so the
//     host refuses to stream an oversized tar.
//
// Secret hygiene: workspace tar bytes may contain user files. The transport
// never logs file contents or byte counts beyond sizes and durations. Error
// messages carry only the operation name and the underlying transport error.

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"mitos.run/mitos/internal/guestgrpc"
	"mitos.run/mitos/internal/vsock"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// wsUploadChunkSize is the maximum number of tar bytes per Upload chunk. 32 KiB
// keeps gRPC messages small while amortizing framing overhead over a workspace
// that is typically a few MiB. This mirrors the chunk size used by the sandbox
// service Upload handler.
const wsUploadChunkSize = 32 * 1024

// wsDialFunc is the injectable dial seam for grpcWorkspaceTransport. The
// production seam is guestgrpc.Dial (vsock); tests inject dialUnixSandbox so no
// real VM is needed.
type wsDialFunc func(vsockPath string) (*guestgrpc.Client, error)

// grpcWorkspaceTransport implements workspace.VsockTransport by forwarding
// TarDir to the guest Sandbox Archive RPC and UntarDir to the Upload RPC over
// gRPC on AgentGRPCPort 53. It dials a fresh connection per call so the
// lifecycle is trivially correct.
type grpcWorkspaceTransport struct {
	// vsockPath is the Firecracker vsock UDS host path for the active VM's
	// guest agent. It is a host filesystem path, not a secret.
	vsockPath string
	// dial opens a gRPC client to the guest Sandbox service. Nil uses
	// guestgrpc.Dial (production vsock); tests inject dialUnixSandbox.
	dial wsDialFunc
	// maxTarBytes is the host-side cap on workspace tar payloads. Zero means
	// use vsock.MaxTarBytes (64 MiB). Tests may set a small value to avoid
	// allocating large buffers.
	maxTarBytes int
}

// effectiveMaxTarBytes returns the active cap: the injected value when non-zero,
// or vsock.MaxTarBytes for production.
func (t *grpcWorkspaceTransport) effectiveMaxTarBytes() int {
	if t.maxTarBytes > 0 {
		return t.maxTarBytes
	}
	return vsock.MaxTarBytes
}

// dialer returns the effective dial function: the injected one, or the
// production guestgrpc.Dial when none is set.
func (t *grpcWorkspaceTransport) dialer() wsDialFunc {
	if t.dial != nil {
		return t.dial
	}
	return guestgrpc.Dial
}

// TarDir opens the guest Archive RPC with DOWNLOAD direction, drains the Chunk
// stream, and returns the concatenated tar bytes. This is the gRPC equivalent
// of vsock.Client.TarDir: the guest tars the subtree at path and streams it
// back. Workspace content bytes are never logged; errors carry only the path
// and transport error.
func (t *grpcWorkspaceTransport) TarDir(path string) ([]byte, error) {
	client, err := t.dialer()(t.vsockPath)
	if err != nil {
		return nil, fmt.Errorf("connect guest gRPC for workspace tar: %w", err)
	}
	defer client.Close() //nolint:errcheck // best-effort on close

	ctx := context.Background()
	stream, err := client.Sandbox.Archive(ctx, &sandboxv1.ArchiveRequest{
		Path:      path,
		Direction: sandboxv1.ArchiveRequest_DOWNLOAD,
	})
	if err != nil {
		return nil, fmt.Errorf("open guest Archive for workspace tar %q: %w", path, err)
	}

	cap := t.effectiveMaxTarBytes()
	var buf bytes.Buffer
	var received int
	for {
		chunk, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				// io.EOF before an eof chunk means the stream ended unexpectedly.
				return nil, fmt.Errorf("guest Archive stream ended before eof chunk while tarring %q", path)
			}
			return nil, fmt.Errorf("recv archive chunk for workspace tar %q: %w", path, err)
		}
		if data := chunk.GetData(); len(data) > 0 {
			received += len(data)
			if received > cap {
				return nil, fmt.Errorf("workspace tar from guest exceeds %d bytes", cap)
			}
			buf.Write(data)
		}
		if chunk.GetEof() {
			break
		}
	}

	return buf.Bytes(), nil
}

// UntarDir opens the guest Upload RPC, sends the open frame (dest=path), then
// streams the tar bytes as chunk UploadRequests, and closes for the UploadResult.
// This is the gRPC equivalent of vsock.Client.UntarDir: the guest extracts the
// tar into path, sanitizing every member against traversal guest-side. Workspace
// content bytes are never logged; errors carry only the path and transport error.
func (t *grpcWorkspaceTransport) UntarDir(path string, tarBytes []byte) error {
	// Host-side cap: refuse to stream a tar that exceeds the bound. This mirrors
	// the old JSON client.go:297-299 check (vsock.Client.UntarDir) as defense-in-depth.
	if len(tarBytes) > t.effectiveMaxTarBytes() {
		return fmt.Errorf("workspace tar for guest exceeds %d bytes", t.effectiveMaxTarBytes())
	}

	client, err := t.dialer()(t.vsockPath)
	if err != nil {
		return fmt.Errorf("connect guest gRPC for workspace untar: %w", err)
	}
	defer client.Close() //nolint:errcheck // best-effort on close

	ctx := context.Background()
	stream, err := client.Sandbox.Upload(ctx)
	if err != nil {
		return fmt.Errorf("open guest Upload for workspace untar %q: %w", path, err)
	}

	// First message: open frame with the destination path.
	if err := stream.Send(&sandboxv1.UploadRequest{
		Msg: &sandboxv1.UploadRequest_Open{Open: &sandboxv1.UploadOpen{Dest: path}},
	}); err != nil {
		return fmt.Errorf("send workspace untar open frame for %q: %w", path, err)
	}

	// Stream the tar bytes in wsUploadChunkSize blocks. Slice directly into
	// tarBytes (owned, not mutated during send) to avoid a per-chunk allocation.
	for len(tarBytes) > 0 {
		n := wsUploadChunkSize
		if n > len(tarBytes) {
			n = len(tarBytes)
		}
		if err := stream.Send(&sandboxv1.UploadRequest{
			Msg: &sandboxv1.UploadRequest_Chunk{Chunk: tarBytes[:n]},
		}); err != nil {
			return fmt.Errorf("send workspace untar chunk for %q: %w", path, err)
		}
		tarBytes = tarBytes[n:]
	}

	if _, err := stream.CloseAndRecv(); err != nil {
		return fmt.Errorf("close workspace untar upload stream for %q: %w", path, err)
	}
	return nil
}

// dialUnixSandbox dials the guest Sandbox gRPC service over a plain unix socket
// (no Firecracker CONNECT preamble). Used by tests that run an in-process gRPC
// Sandbox server on a temp unix socket instead of a real VM.
func dialUnixSandbox(sockPath string) (*guestgrpc.Client, error) {
	return guestgrpc.DialUnix(sockPath)
}
