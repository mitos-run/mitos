// internal/daemon/expose_cap_test.go
package daemon

// Tests for the per-sandbox expose concurrency cap (acquireExpose / SetMaxExposePerSandbox)
// and force-close-on-terminate (CloseExpose / UnregisterSandbox integration).
//
// TDD: these tests were written BEFORE the production code and must fail first.

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestExposeConcurrencyCapRejects asserts that when the cap is 1 a second
// concurrent expose request to the same sandbox is rejected with 429, and that
// after the first request completes a new one is admitted. The discriminator is
// structural (channel synchronization), not a sleep.
func TestExposeConcurrencyCapRejects(t *testing.T) {
	// A backend that blocks after the first flush until <proceed> is closed so we
	// can keep one request open while racing a second.
	proceed := make(chan struct{})
	var once sync.Once
	closeProceed := func() { once.Do(func() { close(proceed) }) }
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: tick\n\n")
		fl.Flush()
		<-proceed
		_, _ = io.WriteString(w, "data: tock\n\n")
		fl.Flush()
	}))
	defer backend.Close()
	defer closeProceed()

	api := newExposeTestAPI(t, backend.Listener.Addr().String())
	api.SetMaxExposePerSandbox(1)
	api.RegisterToken("sb1", "tok")

	port := portOf(t, backend)
	portStr := itoa(port)

	// Helper to build a request for handleExpose.
	makeReq := func() (*http.Request, *streamRecorder) {
		r := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/sb1/expose/"+portStr+"/", nil)
		r.SetPathValue("id", "sb1")
		r.SetPathValue("port", portStr)
		r.Header.Set("Authorization", "Bearer tok")
		return r, newStreamRecorder()
	}

	// First request: start it and wait until the first SSE event arrives (i.e. the
	// tunnel is open and holding the slot).
	r1, rec1 := makeReq()
	defer rec1.Close()
	firstLineDone := make(chan struct{})
	go func() {
		defer close(firstLineDone)
		api.handleExpose(rec1, r1)
	}()

	// Wait until the first line is readable, proving the tunnel is live and holding
	// the concurrency slot. Keep draining rec1 in the background so the reverse
	// proxy is never blocked on a full pipe while the slot is still held.
	br1 := bufio.NewReader(rec1)
	lineC := make(chan string, 1)
	go func() {
		line, _ := br1.ReadString('\n')
		lineC <- line
		// Drain the rest of rec1 so the proxy's copy goroutine can complete once
		// the backend unblocks; without this the pipe fills and handleExpose blocks.
		io.Copy(io.Discard, rec1) //nolint:errcheck // drain until pipe closed
	}()
	select {
	case line := <-lineC:
		if line != "data: tick\n" {
			t.Fatalf("first SSE line: got %q", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first request did not deliver SSE line in time")
	}

	// Second request: must be rejected with 429 while the first is still open.
	// Use a standard httptest.Recorder (not streamRecorder) for the 429 case
	// because http.Error writes a response body and the pipe-backed streamRecorder
	// would block if nobody reads; a regular Recorder buffers the body fine.
	r2, _ := makeReq()
	rec2std := httptest.NewRecorder()
	api.handleExpose(rec2std, r2)
	if rec2std.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 for second concurrent expose, got %d", rec2std.Code)
	}

	// Unblock the backend so the first request's response completes, then wait for
	// handleExpose to return (which releases the slot via defer release()).
	closeProceed()
	select {
	case <-firstLineDone:
	case <-time.After(2 * time.Second):
		t.Fatal("first request did not finish after unblocking the backend")
	}

	// Third request: the slot was released so it must be admitted (not 429).
	r3, rec3 := makeReq()
	defer rec3.Close()
	done3 := make(chan struct{})
	go func() {
		defer close(done3)
		api.handleExpose(rec3, r3)
	}()
	// Read the SSE line to confirm the request went through.
	br3 := bufio.NewReader(rec3)
	line3C := make(chan string, 1)
	go func() { line, _ := br3.ReadString('\n'); line3C <- line }()
	select {
	case line := <-line3C:
		// proceed is already closed so the backend sends both events immediately;
		// the first readable line is "data: tick\n".
		if line != "data: tick\n" {
			t.Fatalf("third request SSE first line: got %q, want data: tick\\n", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("third request (after slot released) did not deliver SSE line in time")
	}
	rec3.Close()
	<-done3
}

// TestExposeCapPerSandbox asserts the cap is per-sandbox: saturating sb1 does
// not block a request to a different sandbox, and the counter for sb1 decrements
// correctly after release so a sequential second request succeeds.
func TestExposeCapPerSandbox(t *testing.T) {
	api := NewSandboxAPI(t.TempDir())
	api.SetMaxExposePerSandbox(1)

	// Acquire for sb1 and hold the slot.
	rel1, ok := api.acquireExpose("sb1")
	if !ok {
		t.Fatal("first acquire for sb1 must be admitted")
	}

	// A second acquire for sb1 must be rejected.
	if _, ok2 := api.acquireExpose("sb1"); ok2 {
		t.Fatal("second concurrent acquire for sb1 must be rejected (over cap)")
	}

	// A different sandbox must be unaffected.
	relB, okB := api.acquireExpose("sb2")
	if !okB {
		t.Fatal("first acquire for sb2 must be admitted (different sandbox)")
	}
	relB()

	// After releasing sb1's slot a new acquire must succeed.
	rel1()
	rel2, ok2 := api.acquireExpose("sb1")
	if !ok2 {
		t.Fatal("after release, a new acquire for sb1 must be admitted")
	}
	rel2()

	// Fully drained: counter entry must be deleted so the map does not grow.
	api.mu.RLock()
	_, present := api.openExpose["sb1"]
	api.mu.RUnlock()
	if present {
		t.Error("a fully-drained sandbox must not retain an expose counter entry")
	}
}

// TestExposeCapDisabled asserts n<=0 disables the cap (unlimited concurrency).
func TestExposeCapDisabled(t *testing.T) {
	api := NewSandboxAPI(t.TempDir())
	api.SetMaxExposePerSandbox(0) // explicitly disable
	var rels []func()
	for i := 0; i < 50; i++ {
		rel, ok := api.acquireExpose("sb1")
		if !ok {
			t.Fatalf("with cap disabled, acquire %d must be admitted", i)
		}
		rels = append(rels, rel)
	}
	for _, r := range rels {
		r()
	}
}

// TestCloseExposeOnUnregister asserts that an in-flight expose conn tracked under
// sandboxID is closed when UnregisterSandbox is called, and the tracking set is
// emptied. It uses a fake net.Conn so no real network is needed.
func TestCloseExposeOnUnregister(t *testing.T) {
	api := NewSandboxAPI(t.TempDir())

	// Create a pair of in-process pipes that behave as a net.Conn.
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	// Register a real sandbox path so UnregisterSandbox has something to delete.
	if err := api.RegisterSandbox("sb1", t.TempDir()+"/fake.sock"); err != nil {
		t.Fatal(err)
	}

	// Track the server side as an expose conn.
	api.trackExposeConn("sb1", serverConn)

	// Verify it is tracked.
	api.mu.RLock()
	_, exists := api.exposeConns["sb1"]
	api.mu.RUnlock()
	if !exists {
		t.Fatal("conn must be tracked after trackExposeConn")
	}

	// UnregisterSandbox must call CloseExpose which closes the conn.
	api.UnregisterSandbox("sb1")

	// serverConn must be closed: a read on clientConn returns an error quickly.
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		_, err := clientConn.Read(buf)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected clientConn to be closed (read error) after UnregisterSandbox")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("clientConn was not closed within 2s after UnregisterSandbox")
	}

	// Tracking set must be cleared.
	api.mu.RLock()
	_, stillExists := api.exposeConns["sb1"]
	api.mu.RUnlock()
	if stillExists {
		t.Error("exposeConns entry must be cleared after CloseExpose")
	}
}

// TestCloseExposeDirectly asserts CloseExpose closes tracked conns and clears
// the entry, leaving other sandboxes unaffected.
func TestCloseExposeDirectly(t *testing.T) {
	api := NewSandboxAPI(t.TempDir())

	c1a, c1b := net.Pipe()
	c2a, c2b := net.Pipe()
	defer c1a.Close()
	defer c2a.Close()

	api.trackExposeConn("sb1", c1b)
	api.trackExposeConn("sb2", c2b)

	api.CloseExpose("sb1")

	// c1b must be closed: read on c1a returns error.
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		_, err := c1a.Read(buf)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected c1a read to error after CloseExpose(sb1)")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("c1a was not closed within 2s after CloseExpose(sb1)")
	}

	// sb2 must be unaffected.
	api.mu.RLock()
	_, sb2exists := api.exposeConns["sb2"]
	api.mu.RUnlock()
	if !sb2exists {
		t.Error("CloseExpose(sb1) must not affect sb2's tracking entry")
	}

	// Cleanup.
	api.CloseExpose("sb2")
	c2a.Close()
}
