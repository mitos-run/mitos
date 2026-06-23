package sandboxrpc

// archive_test.go tests the Archive (server-stream, DOWNLOAD direction) and
// Upload (client-stream) RPCs against the fake guest harness, matching the
// pattern from files_test.go and watch_test.go.

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"

	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
	"mitos.run/mitos/proto/sandbox/v1/sandboxv1connect"
)

// nonDrainingUploadGuest returns from Upload immediately WITHOUT reading the
// chunks channel, simulating a guest that errors early (a full disk, a tar
// extract failure). It exercises the reader-goroutine leak path: the handler
// must cancel the upload context and join the goroutine so it does not block
// forever on the unbuffered chunks send.
type nonDrainingUploadGuest struct {
	fakeGuest
	err error
}

func (g *nonDrainingUploadGuest) Upload(_ context.Context, _ string, _ <-chan []byte) (*UploadResult, error) {
	return nil, g.err
}

// archiveGuest extends fakeGuest for Archive and Upload operations. It holds
// scripted responses and records the arguments it was called with.
type archiveGuest struct {
	fakeGuest

	// Archive: scripted tar chunks and recorded path.
	archiveGotPath string
	archiveChunks  [][]byte
	archiveErr     error

	// Upload: recorded dest and received bytes; scripted result.
	uploadGotDest  string
	uploadGotBytes []byte
	uploadResult   *UploadResult
	uploadErr      error
}

func (g *archiveGuest) Archive(_ context.Context, path string) (ArchiveStream, error) {
	g.archiveGotPath = path
	if g.archiveErr != nil {
		return nil, g.archiveErr
	}
	return &fakeArchiveStream{chunks: g.archiveChunks}, nil
}

func (g *archiveGuest) Upload(_ context.Context, dest string, chunks <-chan []byte) (*UploadResult, error) {
	g.uploadGotDest = dest
	var buf []byte
	for c := range chunks {
		buf = append(buf, c...)
	}
	g.uploadGotBytes = buf
	return g.uploadResult, g.uploadErr
}

// fakeArchiveStream implements ArchiveStream backed by a scripted slice of
// tar chunks. Recv returns each chunk with err == nil; the last chunk carries
// Eof = true. A subsequent call would return io.EOF but the Service never
// makes that call.
type fakeArchiveStream struct {
	chunks [][]byte
	pos    int
}

func (s *fakeArchiveStream) Recv() (*ArchiveChunk, error) {
	if s.pos < len(s.chunks) {
		data := s.chunks[s.pos]
		s.pos++
		isLast := s.pos == len(s.chunks)
		return &ArchiveChunk{Data: data, Eof: isLast}, nil
	}
	// Guard: callers should not call Recv after the terminal frame.
	return &ArchiveChunk{Eof: true}, nil
}

func (s *fakeArchiveStream) Close() error { return nil }

// newArchiveTestServer builds a Service wired with the archiveGuest and
// returns the Connect client.
func newArchiveTestServer(t *testing.T, g *archiveGuest) sandboxv1connect.SandboxClient {
	t.Helper()
	svc := &Service{Guest: func(string) (GuestConn, error) { return g, nil }}
	client, _ := newTestServer(t, svc)
	return client
}

// TestArchiveDownloadStreamsFakeTarChunks verifies that Archive with
// DOWNLOAD direction forwards all tar chunks from the guest and terminates
// with a final Chunk carrying eof = true.
func TestArchiveDownloadStreamsFakeTarChunks(t *testing.T) {
	chunk1 := []byte("tar-header-bytes")
	chunk2 := []byte("tar-data-bytes")
	g := &archiveGuest{
		archiveChunks: [][]byte{chunk1, chunk2},
	}
	client := newArchiveTestServer(t, g)
	ctx := context.Background()

	stream, err := client.Archive(ctx, connect.NewRequest(&sandboxv1.ArchiveRequest{
		Path:      "/workspace/src",
		Direction: sandboxv1.ArchiveRequest_DOWNLOAD,
	}))
	if err != nil {
		t.Fatalf("archive: %v", err)
	}

	var received []byte
	eofSeen := false
	for stream.Receive() {
		msg := stream.Msg()
		received = append(received, msg.GetData()...)
		if msg.GetEof() {
			eofSeen = true
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("stream close: %v", err)
	}

	want := append(chunk1, chunk2...)
	if !bytes.Equal(received, want) {
		t.Fatalf("received = %q, want %q", received, want)
	}
	if !eofSeen {
		t.Fatal("expected final Chunk with eof = true, got none")
	}
	if g.archiveGotPath != "/workspace/src" {
		t.Fatalf("archive path = %q, want /workspace/src", g.archiveGotPath)
	}
}

// TestArchiveEmptyPathSendsEofFrame verifies that when the guest returns no
// chunks, Archive sends a single empty EOF frame so the client always sees the
// terminal frame.
func TestArchiveEmptyPathSendsEofFrame(t *testing.T) {
	g := &archiveGuest{archiveChunks: nil}
	client := newArchiveTestServer(t, g)
	ctx := context.Background()

	stream, err := client.Archive(ctx, connect.NewRequest(&sandboxv1.ArchiveRequest{
		Path:      "/workspace/empty",
		Direction: sandboxv1.ArchiveRequest_DOWNLOAD,
	}))
	if err != nil {
		t.Fatalf("archive: %v", err)
	}

	eofSeen := false
	for stream.Receive() {
		if stream.Msg().GetEof() {
			eofSeen = true
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	_ = stream.Close()

	if !eofSeen {
		t.Fatal("expected empty EOF frame, got none")
	}
}

// TestArchiveUntarDirectionReturnsInvalidArgument verifies that Archive with
// UNTAR direction is rejected with CodeInvalidArgument directing the caller to
// use the Upload RPC.
func TestArchiveUntarDirectionReturnsInvalidArgument(t *testing.T) {
	g := &archiveGuest{}
	client := newArchiveTestServer(t, g)
	ctx := context.Background()

	stream, err := client.Archive(ctx, connect.NewRequest(&sandboxv1.ArchiveRequest{
		Path:      "/workspace/src",
		Direction: sandboxv1.ArchiveRequest_UNTAR,
	}))
	if err != nil {
		// Some connect implementations return the error at the initial call.
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("code = %v, want InvalidArgument", connect.CodeOf(err))
		}
		return
	}
	// Others surface it on Receive.
	stream.Receive()
	if serr := stream.Err(); serr == nil {
		t.Fatal("expected InvalidArgument for UNTAR direction")
	} else if connect.CodeOf(serr) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", connect.CodeOf(serr))
	}
	_ = stream.Close()
}

// TestArchiveUnspecifiedDirectionReturnsInvalidArgument verifies that
// DIRECTION_UNSPECIFIED is also rejected with CodeInvalidArgument.
func TestArchiveUnspecifiedDirectionReturnsInvalidArgument(t *testing.T) {
	g := &archiveGuest{}
	client := newArchiveTestServer(t, g)
	ctx := context.Background()

	stream, err := client.Archive(ctx, connect.NewRequest(&sandboxv1.ArchiveRequest{
		Path:      "/workspace/src",
		Direction: sandboxv1.ArchiveRequest_DIRECTION_UNSPECIFIED,
	}))
	if err != nil {
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("code = %v, want InvalidArgument", connect.CodeOf(err))
		}
		return
	}
	stream.Receive()
	if serr := stream.Err(); serr == nil {
		t.Fatal("expected InvalidArgument for DIRECTION_UNSPECIFIED")
	} else if connect.CodeOf(serr) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", connect.CodeOf(serr))
	}
	_ = stream.Close()
}

// TestUploadSendsOpenThenChunksAndReturnsResult verifies that Upload forwards
// the dest from the open frame, the concatenated chunk bytes to the guest, and
// returns the bytes_written from UploadResult.
func TestUploadSendsOpenThenChunksAndReturnsResult(t *testing.T) {
	data1 := []byte("first-tar-bytes")
	data2 := []byte("second-tar-bytes")
	g := &archiveGuest{
		uploadResult: &UploadResult{BytesWritten: int64(len(data1) + len(data2))},
	}
	client := newArchiveTestServer(t, g)
	ctx := context.Background()

	us := client.Upload(ctx)

	if err := us.Send(&sandboxv1.UploadRequest{
		Msg: &sandboxv1.UploadRequest_Open{Open: &sandboxv1.UploadOpen{
			Dest: "/workspace/target",
		}},
	}); err != nil {
		t.Fatalf("send open: %v", err)
	}
	if err := us.Send(&sandboxv1.UploadRequest{
		Msg: &sandboxv1.UploadRequest_Chunk{Chunk: data1},
	}); err != nil {
		t.Fatalf("send chunk 1: %v", err)
	}
	if err := us.Send(&sandboxv1.UploadRequest{
		Msg: &sandboxv1.UploadRequest_Chunk{Chunk: data2},
	}); err != nil {
		t.Fatalf("send chunk 2: %v", err)
	}

	res, err := us.CloseAndReceive()
	if err != nil {
		t.Fatalf("close and receive: %v", err)
	}

	want := int64(len(data1) + len(data2))
	if res.Msg.GetBytesWritten() != want {
		t.Fatalf("bytes_written = %d, want %d", res.Msg.GetBytesWritten(), want)
	}
	if g.uploadGotDest != "/workspace/target" {
		t.Fatalf("upload dest = %q, want /workspace/target", g.uploadGotDest)
	}
	wantBytes := append(data1, data2...)
	if !bytes.Equal(g.uploadGotBytes, wantBytes) {
		t.Fatalf("upload bytes = %q, want %q", g.uploadGotBytes, wantBytes)
	}
}

// TestUploadMissingOpenFrameReturnsInvalidArgument verifies that Upload
// rejects a stream whose first message is not an UploadOpen with
// CodeInvalidArgument.
func TestUploadMissingOpenFrameReturnsInvalidArgument(t *testing.T) {
	g := &archiveGuest{}
	client := newArchiveTestServer(t, g)
	ctx := context.Background()

	us := client.Upload(ctx)
	if err := us.Send(&sandboxv1.UploadRequest{
		Msg: &sandboxv1.UploadRequest_Chunk{Chunk: []byte("data")},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	_, err := us.CloseAndReceive()
	if err == nil {
		t.Fatal("expected error for missing open frame")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

// TestUploadEarlyGuestErrorDoesNotHang sends an open frame and a chunk, then has
// the guest's Upload return an error WITHOUT draining the chunks channel. The
// handler must cancel the upload context and join the reader goroutine, so it
// returns the guest error promptly instead of hanging on wg.Wait (which would
// happen if the cancel were dropped while the goroutine is blocked on the send).
func TestUploadEarlyGuestErrorDoesNotHang(t *testing.T) {
	g := &nonDrainingUploadGuest{err: errors.New("tar extract failed: no space left on device")}
	svc := &Service{Guest: func(string) (GuestConn, error) { return g, nil }}
	client, _ := newTestServer(t, svc)

	stream := client.Upload(context.Background())
	if err := stream.Send(&sandboxv1.UploadRequest{
		Msg: &sandboxv1.UploadRequest_Open{Open: &sandboxv1.UploadOpen{Dest: "/workspace/out"}},
	}); err != nil {
		t.Fatalf("send open: %v", err)
	}
	if err := stream.Send(&sandboxv1.UploadRequest{
		Msg: &sandboxv1.UploadRequest_Chunk{Chunk: []byte("some tar bytes the guest never reads")},
	}); err != nil {
		t.Fatalf("send chunk: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := stream.CloseAndReceive()
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error from the early guest Upload failure, got nil")
		}
		if connect.CodeOf(err) != connect.CodeInternal {
			t.Fatalf("code = %v, want Internal", connect.CodeOf(err))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Upload handler hung: the reader goroutine likely leaked (cancelUpload missing before wg.Wait)")
	}
}
