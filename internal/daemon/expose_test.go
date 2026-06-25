// internal/daemon/expose_test.go
package daemon

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestProxyHTTPStreamsSSE(t *testing.T) {
	// A backend SSE server that emits three events with a gap between them, so a
	// buffering proxy would fail this test (the first event must arrive before
	// the backend has written the last).
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sse" {
			http.Error(w, "bad path "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("backend ResponseWriter is not a Flusher")
			return
		}
		for i := 0; i < 3; i++ {
			_, _ = io.WriteString(w, "data: tick\n\n")
			fl.Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer backend.Close()

	api := newExposeTestAPI(t, backend.Listener.Addr().String())

	rp, err := api.ProxyHTTP("sb1", portOf(t, backend), "/v1/sandboxes/sb1/expose/"+itoa(portOf(t, backend)))
	if err != nil {
		t.Fatalf("ProxyHTTP: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/sb1/expose/"+itoa(portOf(t, backend))+"/sse", nil)
	rec := newStreamRecorder()
	defer rec.Close()
	go rp.ServeHTTP(rec, req)

	// The first SSE line must arrive while the backend is still mid-stream.
	br := bufio.NewReader(rec)
	line, err := br.ReadString('\n')
	if err != nil || line != "data: tick\n" {
		t.Fatalf("first SSE line: got %q err %v", line, err)
	}
}

func TestProxyHTTPUnknownSandbox(t *testing.T) {
	api := NewSandboxAPI(t.TempDir())
	if _, err := api.ProxyHTTP("ghost", 8000, "/x"); err == nil {
		t.Fatal("expected error for unregistered sandbox")
	}
}

func TestProxyHTTPBadPort(t *testing.T) {
	api := newExposeTestAPI(t, "127.0.0.1:1")
	for _, p := range []int{0, 70000} {
		if _, err := api.ProxyHTTP("sb1", p, "/x"); err == nil {
			t.Fatalf("expected error for out-of-range port %d", p)
		}
	}
}

// newExposeTestAPI wires a SandboxAPI whose sb1 guest tunnels every PortForward
// to backendAddr (a real loopback HTTP server), so ProxyHTTP can be exercised
// without a VM. It reuses the same fake-guest gRPC rig as forward_test.go
// (fakeTunnelGuestSandbox, startFakeGuestGRPCUDS, shortVsockDir), pointing the
// fake guest's target at backendAddr instead of a raw TCP echo.
func newExposeTestAPI(t *testing.T, backendAddr string) *SandboxAPI {
	t.Helper()
	dir := shortVsockDir(t)
	sock := filepath.Join(dir, "sb1", "vsock.sock")
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := &fakeTunnelGuestSandbox{
		target: func(_ int) (net.Conn, error) { return net.Dial("tcp", backendAddr) },
	}
	startFakeGuestGRPCUDS(t, sock, fake)
	api := NewSandboxAPI(dir)
	api.AllowTokenless()
	if err := api.RegisterSandbox("sb1", sock); err != nil {
		t.Fatal(err)
	}
	api.RegisterStreamPath("sb1", sock)
	return api
}

func itoa(n int) string { return strconv.Itoa(n) }

func portOf(t *testing.T, s *httptest.Server) int {
	t.Helper()
	_, p, err := net.SplitHostPort(s.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	n, _ := strconv.Atoi(p)
	return n
}

// streamRecorder is a streaming http.ResponseWriter (also an http.Flusher) whose
// body can be read incrementally with a bufio.Reader, so a streaming proxy is
// observable mid-flight. Close unblocks any proxy copy goroutine still writing
// after the test has read what it needs.
type streamRecorder struct {
	header http.Header
	pr     *io.PipeReader
	pw     *io.PipeWriter
	code   int
	mu     sync.Mutex
}

func newStreamRecorder() *streamRecorder {
	pr, pw := io.Pipe()
	return &streamRecorder{header: make(http.Header), pr: pr, pw: pw}
}
func (r *streamRecorder) Header() http.Header         { return r.header }
func (r *streamRecorder) Write(b []byte) (int, error) { return r.pw.Write(b) }
func (r *streamRecorder) WriteHeader(code int)        { r.mu.Lock(); r.code = code; r.mu.Unlock() }
func (r *streamRecorder) Flush()                      {}
func (r *streamRecorder) Read(p []byte) (int, error)  { return r.pr.Read(p) }
func (r *streamRecorder) Close() error                { r.pr.Close(); r.pw.Close(); return nil }
