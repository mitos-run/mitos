package saas

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestGatewayWebSocketBidiStreaming exercises the REAL interactive-PTY traffic
// shape through the gateway ws proxy, which the lock-step echo test does not:
// the backend pushes several "stdout" frames ASYNCHRONOUSLY over time WITHOUT a
// triggering client frame (a shell emitting output), while the client
// independently sends "stdin" frames. This is the exact pattern #535 reports as
// broken end to end; running it against the gateway ReverseProxy isolates whether
// the gateway can carry an async, sustained, bidirectional ws stream (vs the bug
// being downstream in the guest/husk-stub PTY).
func TestGatewayWebSocketBidiStreaming(t *testing.T) {
	const nStdout = 8

	var mu sync.Mutex
	var gotStdin []string
	serverReady := make(chan struct{})

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"connect.sandbox.v1"}})
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		// Reader goroutine: collect every client frame (the "stdin" the shell
		// would receive). coder/websocket allows one concurrent reader + writer.
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				_, data, err := c.Read(ctx)
				if err != nil {
					return
				}
				mu.Lock()
				gotStdin = append(gotStdin, string(data))
				mu.Unlock()
			}
		}()

		// Push stdout frames on the server's own schedule, not in response to a
		// client write: this is what a live shell does and what the echo test
		// never covered.
		close(serverReady)
		for i := 0; i < nStdout; i++ {
			if err := c.Write(ctx, websocket.MessageBinary, []byte(fmt.Sprintf("stdout-%d", i))); err != nil {
				return
			}
			time.Sleep(15 * time.Millisecond)
		}
		<-done
	}))
	defer backend.Close()

	store := NewMemStore()
	newTestOrg(t, store, "org-a")
	keys := NewKeyService(store)
	created, err := keys.CreateKey(context.Background(), CreateKeyRequest{OrgID: "org-a", Scopes: []string{ScopeSandboxes}})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	cp := &fakeRuntimeCP{endpoint: strings.TrimPrefix(backend.URL, "http://"), token: "per-sandbox-secret", id: "sb1"}
	gw := NewGateway(keys, nil, cp, nil)
	srv := httptest.NewServer(gw)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1) + "/sandbox.v1.Sandbox/Exec?sandbox=sb1"
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{"connect.sandbox.v1"},
		HTTPHeader:   http.Header{"Authorization": {"Bearer " + created.RawKey}},
	})
	if err != nil {
		t.Fatalf("ws dial through gateway: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	// Client sends an "open" then a "stdin" frame, like the PTY client does.
	<-serverReady
	if err := c.Write(ctx, websocket.MessageBinary, []byte("open")); err != nil {
		t.Fatalf("write open: %v", err)
	}
	if err := c.Write(ctx, websocket.MessageBinary, []byte("stdin-keystroke")); err != nil {
		t.Fatalf("write stdin: %v", err)
	}

	// The client must receive every server-pushed stdout frame, in order, through
	// the gateway proxy. A hang here (the #535 symptom) means the gateway does not
	// carry async server->client streaming.
	for i := 0; i < nStdout; i++ {
		readCtx, readCancel := context.WithTimeout(ctx, 3*time.Second)
		typ, data, err := c.Read(readCtx)
		readCancel()
		if err != nil {
			t.Fatalf("read stdout-%d through gateway: %v (the async server->client stream did not arrive)", i, err)
		}
		if typ != websocket.MessageBinary || string(data) != fmt.Sprintf("stdout-%d", i) {
			t.Fatalf("frame %d = %q, want stdout-%d", i, string(data), i)
		}
	}

	// And the client's stdin must have reached the backend.
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(gotStdin)
		has := contains(gotStdin, "stdin-keystroke")
		mu.Unlock()
		if has {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("backend never received the client stdin frame (got %d frames)", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
