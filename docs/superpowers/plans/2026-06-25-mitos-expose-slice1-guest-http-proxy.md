# Mitos Expose Slice 1: Authenticated SSE-safe guest HTTP proxy

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an authenticated HTTP endpoint that reverse-proxies a request to a port inside a running guest over the existing vsock PortForward tunnel, streaming responses immediately so a Server-Sent-Events session against an in-guest coding-agent daemon works end to end.

**Architecture:** A new `net.Conn` adapter wraps a `sandboxrpc.PortForwardStream` so any `http` client can speak HTTP over the vsock tunnel. A `SandboxAPI.ProxyHTTP` builder returns a `httputil.ReverseProxy` whose transport dials the guest port through that adapter with `FlushInterval = -1` (immediate flush, no buffering). The handler is mounted two ways: on forkd behind a per-sandbox bearer gate, and on the standalone `sandbox-server` on its existing tokenless loopback trust model. No TLS, subdomain, or Kubernetes wiring is in this slice; those land in slice 2 (the edge proxy and controller route sync).

**Tech Stack:** Go standard library (`net/http`, `net/http/httputil`, `crypto/subtle`), the existing `internal/daemon` SandboxAPI, the existing `internal/sandboxrpc` PortForward stream, the existing `internal/daemon/forward_test.go` fake-tunnel test rig.

## Global Constraints

- Go module is `mitos.run/mitos`; Go 1.24 or newer.
- Punctuation: never use em (U+2014) or en (U+2013) dashes anywhere, including comments, docs, and commit messages. Use only `.` `,` `;` `:` and the ASCII hyphen-minus for compounds and ranges.
- Error wrapping: `fmt.Errorf("context: %w", err)`. Octal literals as `0o644`.
- Secret values (bearer tokens) are never logged, never in error messages, never in condition or event text, never on a host path. Log keys and counts only.
- TDD: the failing test lands in the same commit as the behavior change. Each task ends with a passing test and a commit.
- DCO: every commit carries a `Signed-off-by` trailer; use `git commit -s`.
- Stage explicit paths only; never `git add -A`.
- Conventional commits: `feat`, `fix`, `docs`, `test`, `refactor`.
- The guest agent dials `127.0.0.1` only; the host never derives the dial target from request input. This invariant is preserved: the guest port is taken from the URL path and range-checked, and the guest side already forces loopback.
- Lint must pass both `golangci-lint run --timeout=5m` and `GOOS=linux golangci-lint run --timeout=5m`.
- A change to `internal/daemon` is a security-sensitive path and needs a named human reviewer before merge; this plan adds the tests and threat-model delta that review depends on.

---

### Task 1: net.Conn adapter over the PortForward stream

A `pfConn` wraps a `sandboxrpc.PortForwardStream` as a `net.Conn`, so a standard `http.Transport` can speak HTTP through the vsock tunnel. Read pulls frames via `Recv` (returning `io.EOF` on the terminal Close frame and buffering any leftover bytes between reads); Write sends a copy via `Send`; Close calls the stream's Close. Deadlines are no-ops (the tunnel lifetime is governed by Close and context cancellation), which is acceptable for a reverse-proxy transport.

**Files:**
- Create: `internal/daemon/expose_conn.go`
- Test: `internal/daemon/expose_conn_test.go`

**Interfaces:**
- Consumes: `sandboxrpc.PortForwardStream` (methods `Recv() (*sandboxrpc.PortForwardFrame, error)`, `Send(data []byte) error`, `Close() error`) from `internal/sandboxrpc/portforward.go`.
- Produces: `func newPFConn(stream sandboxrpc.PortForwardStream) net.Conn`. The returned value satisfies `net.Conn`. Reads return guest-to-client bytes; the terminal Close frame surfaces as `io.EOF`.

- [ ] **Step 1: Write the failing test**

```go
// internal/daemon/expose_conn_test.go
package daemon

import (
	"io"
	"net"
	"testing"

	"mitos.run/mitos/internal/sandboxrpc"
)

// fakePFStream is a scripted PortForwardStream: Recv replays recvFrames in
// order, Send appends to sent, Close records closure.
type fakePFStream struct {
	recvFrames []*sandboxrpc.PortForwardFrame
	recvErr    error // returned after recvFrames are exhausted (nil means io.EOF-style end via Close frame)
	sent       [][]byte
	closed     bool
}

func (f *fakePFStream) Recv() (*sandboxrpc.PortForwardFrame, error) {
	if len(f.recvFrames) == 0 {
		if f.recvErr != nil {
			return nil, f.recvErr
		}
		return nil, io.EOF
	}
	fr := f.recvFrames[0]
	f.recvFrames = f.recvFrames[1:]
	return fr, nil
}

func (f *fakePFStream) Send(data []byte) error { f.sent = append(f.sent, append([]byte(nil), data...)); return nil }
func (f *fakePFStream) Close() error           { f.closed = true; return nil }

func TestPFConnReadReassemblesFramesThenEOF(t *testing.T) {
	st := &fakePFStream{recvFrames: []*sandboxrpc.PortForwardFrame{
		{Data: []byte("hel")},
		{Data: []byte("lo")},
		{Close: true},
	}}
	var c net.Conn = newPFConn(st)

	// A small buffer forces Read to return the buffered remainder across calls.
	buf := make([]byte, 4)
	n, err := c.Read(buf)
	if err != nil || string(buf[:n]) != "hel" {
		t.Fatalf("first read: got %q err %v", buf[:n], err)
	}
	n, err = c.Read(buf)
	if err != nil || string(buf[:n]) != "lo" {
		t.Fatalf("second read: got %q err %v", buf[:n], err)
	}
	if _, err := c.Read(buf); err != io.EOF {
		t.Fatalf("expected io.EOF on Close frame, got %v", err)
	}
}

func TestPFConnWriteSendsCopyAndCloseClosesStream(t *testing.T) {
	st := &fakePFStream{}
	c := newPFConn(st)
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(st.sent) != 1 || string(st.sent[0]) != "ping" {
		t.Fatalf("send not forwarded: %v", st.sent)
	}
	if err := c.Close(); err != nil || !st.closed {
		t.Fatalf("close: err %v closed %v", err, st.closed)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/daemon/ -run TestPFConn -v`
Expected: FAIL with `undefined: newPFConn`.

- [ ] **Step 3: Write the minimal implementation**

```go
// internal/daemon/expose_conn.go
package daemon

import (
	"io"
	"net"
	"time"

	"mitos.run/mitos/internal/sandboxrpc"
)

// pfConn adapts a sandboxrpc.PortForwardStream to net.Conn so a standard
// http.Transport can speak HTTP over the vsock PortForward tunnel. Read pulls
// frames from the stream and buffers any bytes a small caller buffer could not
// take; the terminal Close frame surfaces as io.EOF. Write sends a copy toward
// the guest. Deadlines are no-ops: the tunnel lifetime is governed by Close and
// the request context, not by socket deadlines. Bytes are never logged.
type pfConn struct {
	stream sandboxrpc.PortForwardStream
	buf    []byte // unread bytes from the last frame
	eof    bool
}

func newPFConn(stream sandboxrpc.PortForwardStream) net.Conn {
	return &pfConn{stream: stream}
}

func (c *pfConn) Read(p []byte) (int, error) {
	if len(c.buf) == 0 {
		if c.eof {
			return 0, io.EOF
		}
		for {
			frame, err := c.stream.Recv()
			if err != nil {
				return 0, err
			}
			if frame.Close {
				c.eof = true
				return 0, io.EOF
			}
			if len(frame.Data) > 0 {
				c.buf = frame.Data
				break
			}
			// Empty non-close frame: keep reading.
		}
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}

func (c *pfConn) Write(p []byte) (int, error) {
	chunk := make([]byte, len(p))
	copy(chunk, p)
	if err := c.stream.Send(chunk); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *pfConn) Close() error { return c.stream.Close() }

// pfAddr is a placeholder net.Addr for the loopback-only tunnel endpoints.
type pfAddr struct{}

func (pfAddr) Network() string { return "vsock-portforward" }
func (pfAddr) String() string  { return "guest:loopback" }

func (c *pfConn) LocalAddr() net.Addr                { return pfAddr{} }
func (c *pfConn) RemoteAddr() net.Addr               { return pfAddr{} }
func (c *pfConn) SetDeadline(t time.Time) error      { return nil }
func (c *pfConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *pfConn) SetWriteDeadline(t time.Time) error { return nil }
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/daemon/ -run TestPFConn -v`
Expected: PASS (both tests).

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/expose_conn.go internal/daemon/expose_conn_test.go
git commit -s -m "feat(daemon): add net.Conn adapter over the PortForward tunnel"
```

---

### Task 2: ProxyHTTP reverse-proxy builder

`SandboxAPI.ProxyHTTP` returns a `httputil.ReverseProxy` that dials the guest port through a fresh PortForward stream per connection (keep-alives disabled, matching the per-connection tunnel model), strips the route prefix so the guest daemon sees the sub-path, and flushes immediately so SSE streams token by token. It fails fast for an unregistered sandbox or an out-of-range port, reusing the same guards as `ForwardPort`.

**Files:**
- Create: `internal/daemon/expose.go`
- Test: `internal/daemon/expose_test.go`

**Interfaces:**
- Consumes: `newPFConn` (Task 1); `newVsockGuestConn(api, sandboxID) sandboxrpc.GuestConn` and its `PortForward(ctx, port uint32) (sandboxrpc.PortForwardStream, error)`; `api.checkSandboxRegistered(sandboxID) error`; `api.streamPaths`.
- Produces: `func (api *SandboxAPI) ProxyHTTP(sandboxID string, guestPort int, prefix string) (*httputil.ReverseProxy, error)`. `prefix` is the URL path prefix to strip before forwarding (for example `/v1/sandboxes/sb1/expose/8000`).

- [ ] **Step 1: Write the failing test**

This test reuses the real fake-guest gRPC rig from `internal/daemon/forward_test.go` (same package): the existing symbols `fakeTunnelGuestSandbox`, `startFakeGuestGRPCUDS`, and `shortVsockDir`. `newForwardAPI` there wires those to a raw TCP echo server; we need the tunnel to reach a real HTTP backend instead, so this task adds one helper, `newExposeTestAPI(t, backendAddr)`, that points the fake guest's `target` at `backendAddr`. Its full code is in the helpers block at the end of this step; it references only symbols that already exist in `forward_test.go`.

```go
// internal/daemon/expose_test.go
package daemon

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProxyHTTPStreamsSSE(t *testing.T) {
	// A backend SSE server that emits three events with a gap between them, so a
	// buffering proxy would fail this test (the first event must arrive before
	// the backend has written the last).
	flushed := make(chan struct{}, 3)
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
			flushed <- struct{}{}
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
```

Add the helpers `newExposeTestAPI`, `portOf`, `itoa`, and `newStreamRecorder` at the bottom of `expose_test.go`. The import additions over the existing `forward_test.go` set are `os` and `path/filepath` (for the helper) and `strconv`, `sync`, `net/http/httptest` (for the recorder); `net`, `io`, `http`, and `testing` are already imported by the test file.

```go
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
}

func newStreamRecorder() *streamRecorder {
	pr, pw := io.Pipe()
	return &streamRecorder{header: make(http.Header), pr: pr, pw: pw}
}
func (r *streamRecorder) Header() http.Header        { return r.header }
func (r *streamRecorder) Write(b []byte) (int, error) { return r.pw.Write(b) }
func (r *streamRecorder) WriteHeader(code int)        { r.code = code }
func (r *streamRecorder) Flush()                      {}
func (r *streamRecorder) Read(p []byte) (int, error)  { return r.pr.Read(p) }
func (r *streamRecorder) Close() error                { r.pr.Close(); r.pw.Close(); return nil }
```

In `TestProxyHTTPStreamsSSE` and `TestHandleExposeStreamsSSEEndToEnd`, add `defer rec.Close()` immediately after `rec := newStreamRecorder()` so the backend goroutine unblocks when the test returns.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/daemon/ -run TestProxyHTTP -v`
Expected: FAIL with `undefined: api.ProxyHTTP` (and, if absent, `undefined: newExposeTestAPI`; if the helper name differs, wire to the real one before continuing).

- [ ] **Step 3: Write the minimal implementation**

```go
// internal/daemon/expose.go
package daemon

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
)

// ProxyHTTP returns a reverse proxy that forwards an HTTP request to the guest's
// 127.0.0.1:guestPort over a fresh PortForward tunnel, stripping prefix from the
// request path so the guest daemon sees the sub-path. FlushInterval is -1 so
// responses (including Server-Sent-Events) stream immediately with no buffering;
// keep-alives are disabled so each request uses its own tunnel and guest TCP
// connection, matching the per-connection tunnel model of ForwardPort. It fails
// fast for an unregistered sandbox or an out-of-range port. Bytes are never
// logged (secret hygiene).
func (api *SandboxAPI) ProxyHTTP(sandboxID string, guestPort int, prefix string) (*httputil.ReverseProxy, error) {
	if guestPort < 1 || guestPort > 65535 {
		return nil, fmt.Errorf("guest port %d out of range 1-65535", guestPort)
	}
	if err := api.checkSandboxRegistered(sandboxID); err != nil {
		return nil, err
	}
	api.mu.RLock()
	_, hasPath := api.streamPaths[sandboxID]
	api.mu.RUnlock()
	if !hasPath {
		return nil, fmt.Errorf("sandbox %s has no stream path; cannot proxy HTTP", sandboxID)
	}

	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		g := newVsockGuestConn(api, sandboxID)
		stream, err := g.PortForward(ctx, uint32(guestPort))
		if err != nil {
			return nil, fmt.Errorf("open guest port forward: %w", err)
		}
		return newPFConn(stream), nil
	}

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = "guest" // ignored: DialContext returns the tunnel
			pr.Out.Host = "guest"
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, prefix)
			if pr.Out.URL.Path == "" {
				pr.Out.URL.Path = "/"
			}
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
		},
		Transport: &http.Transport{
			DialContext:       dial,
			DisableKeepAlives: true,
		},
		FlushInterval: -1, // immediate flush: SSE and long-lived streams
	}
	return rp, nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/daemon/ -run TestProxyHTTP -v`
Expected: PASS (all three tests). If `TestProxyHTTPStreamsSSE` hangs, the FlushInterval or the streamRecorder pipe is misconfigured; confirm `FlushInterval: -1` and that the recorder writes go through the pipe.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/expose.go internal/daemon/expose_test.go
git commit -s -m "feat(daemon): add ProxyHTTP guest reverse proxy with immediate flush"
```

---

### Task 3: per-sandbox bearer check helper and the forkd expose route

The expose handler proxies arbitrary HTTP (including long-lived streams), so it cannot sit behind the body-peeking `requireBearer` wrapper. It is mounted on the outer mux with its own constant-time bearer check, mirroring how the Connect and PTY routes authenticate outside `requireBearer`. The route is `GET /v1/sandboxes/{id}/expose/{port}/` plus the catch-all so sub-paths reach the guest.

**Files:**
- Modify: `internal/daemon/sandbox_api.go` (add `checkBearer`; register the route on `outer` in `Handler()` near line 611)
- Create: `internal/daemon/expose_route.go` (the `handleExpose` handler)
- Test: `internal/daemon/expose_route_test.go`

**Interfaces:**
- Consumes: `api.tokens` map; `api.allowTokenless`; `api.resolveSandboxID`; `ProxyHTTP` (Task 2).
- Produces: `func (api *SandboxAPI) checkBearer(sandboxID string, r *http.Request) bool`; `func (api *SandboxAPI) handleExpose(w http.ResponseWriter, r *http.Request)`.

- [ ] **Step 1: Write the failing test**

```go
// internal/daemon/expose_route_test.go
package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleExposeRejectsMissingBearer(t *testing.T) {
	api := newExposeTestAPI(t, "127.0.0.1:1")
	api.RegisterToken("sb1", "secret-token")

	r := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/sb1/expose/8000/", nil)
	r.SetPathValue("id", "sb1")
	r.SetPathValue("port", "8000")
	w := httptest.NewRecorder()
	api.handleExpose(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without bearer, got %d", w.Code)
	}
}

func TestHandleExposeRejectsWrongBearer(t *testing.T) {
	api := newExposeTestAPI(t, "127.0.0.1:1")
	api.RegisterToken("sb1", "secret-token")

	r := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/sb1/expose/8000/", nil)
	r.SetPathValue("id", "sb1")
	r.SetPathValue("port", "8000")
	r.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	api.handleExpose(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong bearer, got %d", w.Code)
	}
}

func TestHandleExposeRejectsBadPort(t *testing.T) {
	api := newExposeTestAPI(t, "127.0.0.1:1")
	api.RegisterToken("sb1", "secret-token")

	r := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/sb1/expose/notaport/", nil)
	r.SetPathValue("id", "sb1")
	r.SetPathValue("port", "notaport")
	r.Header.Set("Authorization", "Bearer secret-token")
	w := httptest.NewRecorder()
	api.handleExpose(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad port, got %d", w.Code)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/daemon/ -run TestHandleExpose -v`
Expected: FAIL with `undefined: api.handleExpose`.

- [ ] **Step 3: Write the minimal implementation**

```go
// internal/daemon/expose_route.go
package daemon

import (
	"crypto/subtle"
	"net/http"
	"strconv"
	"strings"
)

// checkBearer validates the per-sandbox bearer token on a request that is
// mounted outside the body-peeking requireBearer wrapper (the expose proxy and
// other streaming routes). It mirrors requireBearer's contract: fail closed when
// a token is registered and the presented bearer does not match in constant
// time; allow when AllowTokenless was set and no token is registered (standalone
// loopback). Token values are never logged.
func (api *SandboxAPI) checkBearer(sandboxID string, r *http.Request) bool {
	id := api.resolveSandboxID(sandboxID)
	api.mu.RLock()
	token, hasToken := api.tokens[id]
	api.mu.RUnlock()

	if !hasToken {
		return api.allowTokenless
	}
	presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(token)) == 1
}

// handleExpose reverse-proxies GET/POST traffic to a guest port over the vsock
// tunnel. The path is /v1/sandboxes/{id}/expose/{port}/...; everything after the
// port is forwarded to the guest daemon. Auth is the per-sandbox bearer
// (checkBearer); the body-peeking requireBearer wrapper cannot front this route
// because it proxies arbitrary and streaming HTTP.
func (api *SandboxAPI) handleExpose(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	portStr := r.PathValue("port")
	if id == "" || portStr == "" {
		http.Error(w, "expose path must be /v1/sandboxes/{id}/expose/{port}/", http.StatusBadRequest)
		return
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		http.Error(w, "guest port must be an integer in 1-65535", http.StatusBadRequest)
		return
	}
	if !api.checkBearer(id, r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	prefix := "/v1/sandboxes/" + id + "/expose/" + portStr
	rp, err := api.ProxyHTTP(id, port, prefix)
	if err != nil {
		http.Error(w, "no route to guest port", http.StatusBadGateway)
		return
	}
	rp.ServeHTTP(w, r)
}
```

Then register the route on the outer mux. Modify `internal/daemon/sandbox_api.go` in `Handler()` (the block around line 611), adding the two lines before `outer.Handle("/", api.requireBearer(jsonMux))`:

```go
	// Authenticated guest HTTP proxy (Mitos Expose slice 1). Mounted OUTSIDE
	// requireBearer because it proxies arbitrary and streaming HTTP and cannot be
	// body-peeked; checkBearer enforces the same per-sandbox token gate. The
	// trailing-slash pattern captures every sub-path under the port.
	outer.HandleFunc("/v1/sandboxes/{id}/expose/{port}/", api.handleExpose)
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/daemon/ -run TestHandleExpose -v`
Expected: PASS (all three tests).

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/expose_route.go internal/daemon/sandbox_api.go internal/daemon/expose_route_test.go
git commit -s -m "feat(daemon): mount authenticated guest HTTP expose route on forkd"
```

---

### Task 4: standalone sandbox-server expose route

The standalone `sandbox-server` registers HTTP routes on its own mux and delegates to `SandboxAPI`. Mount the expose route the same way the existing `/forward` route is mounted (`cmd/sandbox-server/main.go:338`), reusing `api.handleExpose`. On the standalone server the loopback trust model applies (AllowTokenless), so `checkBearer` returns true when no token is registered, matching the existing forward behavior.

**Files:**
- Modify: `internal/daemon/expose_route.go` (add the exported `HandleExpose` shim)
- Modify: `cmd/sandbox-server/main.go` (register the route near line 338, next to the `/forward` registration)
- Test: `cmd/sandbox-server/expose_test.go`

**Interfaces:**
- Consumes: `s.sandboxAPI` (a `*daemon.SandboxAPI`); `daemon.SandboxAPI.handleExpose` is unexported, so the standalone server reaches it through the mounted `s.sandboxAPI.Handler()` OR via a thin exported shim. Use the exported shim: add `func (api *SandboxAPI) HandleExpose(w http.ResponseWriter, r *http.Request)` in `internal/daemon/expose_route.go` that calls `api.handleExpose`, so `cmd/sandbox-server` (a different package) can register it directly.
- Produces: the route `GET /v1/sandboxes/{id}/expose/{port}/` on the standalone mux.

- [ ] **Step 1: Write the failing test**

The standalone tests call handlers directly (see `cmd/sandbox-server/forward_test.go`: `TestForwardEndpointMockModeUnsupported` calls `s.handleForward(w, r)` with `r.SetPathValue(...)`, never through the mux). Follow that convention: construct a real-mode server with `newServer(dataDir, "", false, 16, 86400)` and call the exported `s.sandboxAPI.HandleExpose` directly. With no sandbox registered, the standalone loopback trust model passes the (tokenless) auth and the request fails at route resolution, so the handler returns 502 (no route to guest), proving the standalone server exposes the handler. The one-line mux registration in Step 3 follows the identical pattern as the adjacent `/forward` registration.

```go
// cmd/sandbox-server/expose_test.go
package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStandaloneExposeHandlerReachable(t *testing.T) {
	s := newServer(t.TempDir(), "", false, 16, 86400) // real mode, no engine
	r := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/ghost/expose/8000/", nil)
	r.SetPathValue("id", "ghost")
	r.SetPathValue("port", "8000")
	w := httptest.NewRecorder()
	s.sandboxAPI.HandleExpose(w, r)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 for unknown sandbox through the standalone server, got %d (body %q)", w.Code, w.Body.String())
	}
}
```

If `newServer` does not set the loopback tokenless mode on its `sandboxAPI` (check whether it calls `AllowTokenless`), the unknown-sandbox path may return 401 instead; in that case assert `w.Code == http.StatusUnauthorized || w.Code == http.StatusBadGateway` and confirm it is never 404, which would mean the handler is not wired.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/sandbox-server/ -run TestStandaloneExposeHandlerReachable -v`
Expected: FAIL to compile with `s.sandboxAPI.HandleExpose undefined` (the exported shim does not exist yet).

- [ ] **Step 3: Add the exported shim and register the route**

Add the exported shim to `internal/daemon/expose_route.go` so a different package can mount the handler:

```go
// HandleExpose is the exported entry point so the standalone sandbox-server
// (a separate package) can mount the guest HTTP proxy route. It is identical to
// the route forkd mounts internally via handleExpose.
func (api *SandboxAPI) HandleExpose(w http.ResponseWriter, r *http.Request) {
	api.handleExpose(w, r)
}
```

Then register the route in `cmd/sandbox-server/main.go`, immediately after the existing line 338 `mux.HandleFunc("POST /v1/sandboxes/{id}/forward", s.handleForward)`:

```go
	// Authenticated guest HTTP proxy (Mitos Expose slice 1): reverse-proxy to a
	// guest port over the vsock tunnel, streaming responses (SSE-safe). On the
	// standalone server this inherits the loopback tokenless trust model.
	mux.HandleFunc("/v1/sandboxes/{id}/expose/{port}/", s.sandboxAPI.HandleExpose)
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./cmd/sandbox-server/ -run TestStandaloneExposeHandlerReachable -v`
Expected: PASS (status is 502, not 404).

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/expose_route.go cmd/sandbox-server/main.go cmd/sandbox-server/expose_test.go
git commit -s -m "feat(sandbox-server): mount the guest HTTP expose route on the standalone mux"
```

---

### Task 5: end-to-end SSE streaming test through the mounted route

Tasks 2 and 3 tested the proxy builder and the auth gate in isolation. This task proves the full mounted path: an authenticated request to the forkd route streams an SSE response from a real in-guest-style backend incrementally. It is the concrete #230 acceptance criterion ("stream an SSE session against a coding agent running in the guest end to end") at the unit-integration layer; the KVM end-to-end against a real guest daemon is a follow-up CI phase noted at the end.

**Files:**
- Modify: `internal/daemon/expose_test.go` (add the end-to-end case)

**Interfaces:**
- Consumes: `newExposeTestAPI`, `handleExpose`, `streamRecorder`, `portOf`, `itoa` (Tasks 2 and 3).

- [ ] **Step 1: Write the failing test**

```go
// add to internal/daemon/expose_test.go
func TestHandleExposeStreamsSSEEndToEnd(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			_, _ = io.WriteString(w, "data: ev\n\n")
			fl.Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer backend.Close()

	api := newExposeTestAPI(t, backend.Listener.Addr().String())
	api.RegisterToken("sb1", "tok")

	p := portOf(t, backend)
	r := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/sb1/expose/"+itoa(p)+"/", nil)
	r.SetPathValue("id", "sb1")
	r.SetPathValue("port", itoa(p))
	r.Header.Set("Authorization", "Bearer tok")
	rec := newStreamRecorder()
	go api.handleExpose(rec, r)

	br := bufio.NewReader(rec)
	line, err := br.ReadString('\n')
	if err != nil || line != "data: ev\n" {
		t.Fatalf("first streamed SSE line: got %q err %v", line, err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails, then passes**

Run: `go test ./internal/daemon/ -run TestHandleExposeStreamsSSEEndToEnd -v`
Expected: PASS once Tasks 1-4 are in place (this test adds no new production code; if it fails, the failure localizes a regression in the tunnel-to-proxy wiring). If it FAILS because `newExposeTestAPI` does not bridge to the backend for the chosen port, extend the rig so the fake guest dials the backend addr for the requested port.

- [ ] **Step 3: Run the full daemon package and lint**

Run: `go test ./internal/daemon/ -v`
Run: `golangci-lint run --timeout=5m ./internal/daemon/... ./cmd/sandbox-server/...`
Run: `GOOS=linux golangci-lint run --timeout=5m ./internal/daemon/... ./cmd/sandbox-server/...`
Expected: all PASS, lint clean.

- [ ] **Step 4: Commit**

```bash
git add internal/daemon/expose_test.go
git commit -s -m "test(daemon): end-to-end SSE streaming through the expose route"
```

---

### Task 6: docs and threat-model delta

Document the authenticated guest HTTP proxy in `docs/ports.md`, update the harness recipe to use it instead of the raw socket tunnel for the HTTP case, and add the threat-model row. Per the operating principles, the threat-model delta lands in the same change as the surface.

**Files:**
- Modify: `docs/ports.md`
- Modify: `docs/recipes/agent-harness.md`
- Modify: `docs/threat-model.md` (section 7c area, the preview / port-exposure rows)

**Interfaces:** none (documentation).

- [ ] **Step 1: Document the endpoint in docs/ports.md**

Add a section describing the route, its auth, and its streaming behavior:

```markdown
## Authenticated guest HTTP proxy (Mitos Expose)

`GET|POST /v1/sandboxes/{id}/expose/{port}/<sub-path>` reverse-proxies an HTTP
request to the guest's `127.0.0.1:<port>` over the vsock PortForward tunnel.
Unlike the raw `/forward` socket tunnel, this path speaks HTTP, streams the
response immediately (Server-Sent-Events safe, no buffering), and is gated by
the per-sandbox bearer token on forkd. On the standalone sandbox-server it
inherits the loopback tokenless trust model. The guest dial is forced to
loopback by the guest agent; the host never derives the dial target from request
input. Each request uses its own tunnel and guest TCP connection.
```

- [ ] **Step 2: Update the harness recipe**

In `docs/recipes/agent-harness.md`, replace the "follow-up: SSE / long-lived streaming" bullet with a worked step that starts the daemon, then drives it over `/v1/sandboxes/{id}/expose/{port}/` with a streamed SSE example, noting the bearer header for the forkd path.

- [ ] **Step 3: Add the threat-model row**

In `docs/threat-model.md`, near the preview/port-exposure rows, add a row for the authenticated guest HTTP proxy: status mitigated; the per-sandbox bearer gate (constant-time compare, never logged); mounted outside the body-peeking requireBearer so streaming is not buffered; loopback-only guest dial preserved (SSRF allowlist-of-one); the standalone path is loopback tokenless by design; note that the internet-facing edge proxy, TLS, and subdomain routing are slice 2 and not yet part of this surface.

- [ ] **Step 4: Verify no forbidden dashes and commit**

Run: `grep -nP "[\x{2014}\x{2013}]" docs/ports.md docs/recipes/agent-harness.md docs/threat-model.md` (expect no new matches in the edited regions).

```bash
git add docs/ports.md docs/recipes/agent-harness.md docs/threat-model.md
git commit -s -m "docs(expose): document the authenticated guest HTTP proxy and its threat-model delta"
```

---

## Self-review notes

- Spec coverage for this slice: #230 "reach its HTTP port from an external client through forkd, with auth on the path" (Tasks 3, 4); "stream an SSE session end to end" (Tasks 2, 5). The remaining #230 criteria (Kubernetes Service/Ingress routing) and #312 (claim to URL) are explicitly later slices and out of scope here.
- Deferred to slice 2: the edge `expose-proxy`, the `<label>.<expose-domain>` subdomain scheme, the Host allowlist and reserved names, the controller route sync, and the wildcard plus post-quantum TLS. The `internal/preview` to `internal/expose` package rename is deferred to that slice so this slice adds no cross-package churn.
- Type consistency: `newPFConn` (Task 1) is consumed by `ProxyHTTP` (Task 2); `ProxyHTTP(sandboxID string, guestPort int, prefix string)` is consumed by `handleExpose` (Task 3) and the standalone shim `HandleExpose` (Task 4); `checkBearer` (Task 3) is the single auth gate. `newExposeTestAPI`, `streamRecorder`, `portOf`, and `itoa` are test helpers introduced in Task 2 and reused in Tasks 3 and 5.
- Open follow-up for the implementer: confirm the exact name and signature of the existing fake-tunnel test rig in `internal/daemon/forward_test.go` and the standalone-server test constructor in `cmd/sandbox-server/forward_test.go`, and wire the new tests to those rather than duplicating them. If `httputil.ProxyRequest.Rewrite` is not preferred in this codebase (check whether existing reverse proxies use `Director`), match the local convention.
- KVM end-to-end: a follow-up adds a `kvm-test.yaml` phase that starts a real in-guest SSE daemon and streams through the expose route on a KVM runner, the same way the existing firecracker-test phase exercises guest exec over vsock.
