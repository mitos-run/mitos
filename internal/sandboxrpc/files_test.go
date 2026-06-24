package sandboxrpc

import (
	"bytes"
	"context"
	"testing"

	"connectrpc.com/connect"

	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
	"mitos.run/mitos/proto/sandbox/v1/sandboxv1connect"
)

// fileGuest extends fakeGuest for file operations. It holds scripted responses
// for each GuestConn file method and records the arguments it was called with.
type fileGuest struct {
	fakeGuest

	// ReadFile scripted response.
	readData [][]byte
	readErr  error

	// WriteFile: recorded args and scripted result.
	writeGotPath   string
	writeGotMode   uint32
	writeGotChunks [][]byte
	writeResult    *WriteFileResult
	writeErr       error

	// List: recorded args and scripted result.
	listGotPath      string
	listGotPageSize  int32
	listGotPageToken string
	listGotFilter    string
	listResult       *ListResult
	listErr          error

	// Stat: recorded arg and scripted result.
	statGotPath string
	statResult  *FileInfo
	statErr     error

	// Mkdir: recorded args.
	mkdirGotPath      string
	mkdirGotRecursive bool
	mkdirErr          error

	// Remove: recorded args.
	removeGotPath      string
	removeGotRecursive bool
	removeErr          error
}

func (g *fileGuest) ReadFile(_ context.Context, _ string, _, _ int64) ([][]byte, error) {
	return g.readData, g.readErr
}

func (g *fileGuest) WriteFile(_ context.Context, path string, mode uint32, chunks [][]byte) (*WriteFileResult, error) {
	g.writeGotPath = path
	g.writeGotMode = mode
	g.writeGotChunks = chunks
	return g.writeResult, g.writeErr
}

func (g *fileGuest) List(_ context.Context, path string, pageSize int32, pageToken string, filter string) (*ListResult, error) {
	g.listGotPath = path
	g.listGotPageSize = pageSize
	g.listGotPageToken = pageToken
	g.listGotFilter = filter
	return g.listResult, g.listErr
}

func (g *fileGuest) Stat(_ context.Context, path string) (*FileInfo, error) {
	g.statGotPath = path
	return g.statResult, g.statErr
}

func (g *fileGuest) Mkdir(_ context.Context, path string, recursive bool) error {
	g.mkdirGotPath = path
	g.mkdirGotRecursive = recursive
	return g.mkdirErr
}

func (g *fileGuest) Remove(_ context.Context, path string, recursive bool) error {
	g.removeGotPath = path
	g.removeGotRecursive = recursive
	return g.removeErr
}

// newFileTestServer builds a Service wired with the fileGuest and returns the
// Connect client.
func newFileTestServer(t *testing.T, g *fileGuest) sandboxv1connect.SandboxClient {
	t.Helper()
	svc := &Service{Guest: func(string) (GuestConn, error) { return g, nil }}
	client, _ := newTestServer(t, svc)
	return client
}

// TestWriteThenReadRoundTrips sends two data chunks over WriteFile then reads
// them back via ReadFile, asserting the bytes are preserved end to end.
func TestWriteThenReadRoundTrips(t *testing.T) {
	payload := []byte("hello from the round-trip test")
	g := &fileGuest{
		writeResult: &WriteFileResult{BytesWritten: int64(len(payload))},
		readData:    [][]byte{payload},
	}
	client := newFileTestServer(t, g)
	ctx := context.Background()

	// Write phase.
	ws := client.WriteFile(ctx)
	if err := ws.Send(&sandboxv1.WriteFileRequest{
		Msg: &sandboxv1.WriteFileRequest_Open{Open: &sandboxv1.WriteFileOpen{
			Path: "/workspace/test.txt",
			Mode: 0o644,
		}},
	}); err != nil {
		t.Fatalf("send open: %v", err)
	}
	if err := ws.Send(&sandboxv1.WriteFileRequest{
		Msg: &sandboxv1.WriteFileRequest_Data{Data: payload},
	}); err != nil {
		t.Fatalf("send data: %v", err)
	}
	wres, err := ws.CloseAndReceive()
	if err != nil {
		t.Fatalf("write close: %v", err)
	}
	if wres.Msg.GetBytesWritten() != int64(len(payload)) {
		t.Fatalf("bytes_written = %d, want %d", wres.Msg.GetBytesWritten(), len(payload))
	}
	if g.writeGotPath != "/workspace/test.txt" {
		t.Fatalf("write path = %q, want /workspace/test.txt", g.writeGotPath)
	}
	if g.writeGotMode != 0o644 {
		t.Fatalf("write mode = %o, want 0644", g.writeGotMode)
	}

	// Verify data bytes reached the fake guest.
	var gotData []byte
	for _, c := range g.writeGotChunks {
		gotData = append(gotData, c...)
	}
	if !bytes.Equal(gotData, payload) {
		t.Fatalf("write chunks = %q, want %q", gotData, payload)
	}

	// Read phase: ServerStreamForClient uses Receive() bool + Msg() + Err().
	rstream, rerr := client.ReadFile(ctx, connect.NewRequest(&sandboxv1.ReadFileRequest{
		Path: "/workspace/test.txt",
	}))
	if rerr != nil {
		t.Fatalf("read file: %v", rerr)
	}
	var received []byte
	for rstream.Receive() {
		received = append(received, rstream.Msg().GetData()...)
	}
	if err := rstream.Err(); err != nil {
		t.Fatalf("read stream error: %v", err)
	}
	if err := rstream.Close(); err != nil {
		t.Fatalf("read stream close: %v", err)
	}
	if !bytes.Equal(received, payload) {
		t.Fatalf("read back = %q, want %q", received, payload)
	}
}

// TestListReturnsFakeEntriesAndNextPageToken verifies that List surfaces both
// the directory entries and the next_page_token when the fake truncates the
// result at one page.
func TestListReturnsFakeEntriesAndNextPageToken(t *testing.T) {
	g := &fileGuest{
		listResult: &ListResult{
			Entries: []*FileInfo{
				{Name: "a.txt", Path: "/workspace/a.txt", IsDir: false, Size: 10, Mode: 0o644},
				{Name: "subdir", Path: "/workspace/subdir", IsDir: true},
			},
			NextPageToken: "page2",
		},
	}
	client := newFileTestServer(t, g)
	ctx := context.Background()

	resp, err := client.List(ctx, connect.NewRequest(&sandboxv1.ListRequest{
		Parent:    "/workspace",
		PageSize:  2,
		PageToken: "page1",
		Filter:    "*.txt",
	}))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if resp.Msg.GetNextPageToken() != "page2" {
		t.Fatalf("next_page_token = %q, want page2", resp.Msg.GetNextPageToken())
	}
	if len(resp.Msg.GetEntries()) != 2 {
		t.Fatalf("entries count = %d, want 2", len(resp.Msg.GetEntries()))
	}
	if resp.Msg.GetEntries()[0].GetName() != "a.txt" {
		t.Fatalf("first entry = %q, want a.txt", resp.Msg.GetEntries()[0].GetName())
	}
	if !resp.Msg.GetEntries()[1].GetIsDir() {
		t.Fatal("second entry IsDir = false, want true")
	}
	// Assert forwarding of all request fields.
	if g.listGotPath != "/workspace" {
		t.Fatalf("list path = %q, want /workspace", g.listGotPath)
	}
	if g.listGotPageSize != 2 {
		t.Fatalf("page_size = %d, want 2", g.listGotPageSize)
	}
	if g.listGotPageToken != "page1" {
		t.Fatalf("page_token = %q, want page1", g.listGotPageToken)
	}
	if g.listGotFilter != "*.txt" {
		t.Fatalf("filter = %q, want *.txt", g.listGotFilter)
	}
}

// TestStatReturnsFileInfo verifies that Stat surfaces the FileInfo returned by
// the fake guest with all fields intact.
func TestStatReturnsFileInfo(t *testing.T) {
	g := &fileGuest{
		statResult: &FileInfo{
			Name:           "hello.go",
			Path:           "/workspace/hello.go",
			IsDir:          false,
			Size:           1024,
			Mode:           0o644,
			ModifiedAtUnix: 1700000000,
		},
	}
	client := newFileTestServer(t, g)
	ctx := context.Background()

	resp, err := client.Stat(ctx, connect.NewRequest(&sandboxv1.StatRequest{
		Path: "/workspace/hello.go",
	}))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if resp.Msg.GetName() != "hello.go" {
		t.Fatalf("name = %q, want hello.go", resp.Msg.GetName())
	}
	if resp.Msg.GetSize() != 1024 {
		t.Fatalf("size = %d, want 1024", resp.Msg.GetSize())
	}
	if resp.Msg.GetMode() != 0o644 {
		t.Fatalf("mode = %o, want 0644", resp.Msg.GetMode())
	}
	if resp.Msg.GetModifiedAtUnix() != 1700000000 {
		t.Fatalf("modified_at_unix = %d, want 1700000000", resp.Msg.GetModifiedAtUnix())
	}
	if g.statGotPath != "/workspace/hello.go" {
		t.Fatalf("stat path = %q, want /workspace/hello.go", g.statGotPath)
	}
}

// TestMkdirForwardsPath verifies that Mkdir forwards the path to the guest and
// returns an empty response on success.
func TestMkdirForwardsPath(t *testing.T) {
	g := &fileGuest{}
	client := newFileTestServer(t, g)
	ctx := context.Background()

	_, err := client.Mkdir(ctx, connect.NewRequest(&sandboxv1.MkdirRequest{
		Path: "/workspace/newdir",
	}))
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if g.mkdirGotPath != "/workspace/newdir" {
		t.Fatalf("mkdir path = %q, want /workspace/newdir", g.mkdirGotPath)
	}
	if !g.mkdirGotRecursive {
		t.Fatal("mkdir recursive = false, want true")
	}
}

// TestRemoveForwardsPathAndRecursive verifies that Remove forwards the path and
// recursive flag to the guest.
func TestRemoveForwardsPathAndRecursive(t *testing.T) {
	g := &fileGuest{}
	client := newFileTestServer(t, g)
	ctx := context.Background()

	_, err := client.Remove(ctx, connect.NewRequest(&sandboxv1.RemoveRequest{
		Path:      "/workspace/olddir",
		Recursive: true,
	}))
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if g.removeGotPath != "/workspace/olddir" {
		t.Fatalf("remove path = %q, want /workspace/olddir", g.removeGotPath)
	}
	if !g.removeGotRecursive {
		t.Fatal("remove recursive = false, want true")
	}
}
