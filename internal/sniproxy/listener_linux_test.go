//go:build linux

package sniproxy

import (
	"testing"
	"time"
)

func newClosableProxy() *Proxy {
	return NewProxy(
		staticResolver{ip2id: map[string]string{}},
		stubAllowlist{allow: map[string]int{}},
		&fakeDialer{conn: newFakeConn(nil)},
		&recordingLogger{},
	)
}

// TestListenAndServeClosesGracefully asserts Close unblocks the Accept loop and
// makes ListenAndServe return nil (a clean shutdown, not a fatal error), so forkd
// can shut the SNI proxy down on signal. Mirrors the egressproxy parity.
func TestListenAndServeClosesGracefully(t *testing.T) {
	p := newClosableProxy()

	errCh := make(chan error, 1)
	go func() { errCh <- p.ListenAndServe("127.0.0.1:0") }()

	time.Sleep(50 * time.Millisecond)
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("ListenAndServe must return nil on Close, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ListenAndServe did not return after Close")
	}
}

// TestCloseBeforeListenIsClean asserts Close before ListenAndServe binds (the
// shutdown-races-startup case) still makes ListenAndServe return nil.
func TestCloseBeforeListenIsClean(t *testing.T) {
	p := newClosableProxy()
	if err := p.Close(); err != nil {
		t.Fatalf("Close before listen: %v", err)
	}
	if err := p.ListenAndServe("127.0.0.1:0"); err != nil {
		t.Fatalf("ListenAndServe after pre-close must be nil, got %v", err)
	}
}
