package daemon

import (
	"sync"
	"testing"
)

// TestStreamCapAcquireRelease covers production-blocker #2, cap 3: a per-sandbox
// ceiling on concurrent OPEN streams (streaming exec, run_code, PTY each hold a
// dedicated vsock connection for the command lifetime). Opening up to N streams
// for a sandbox works; the N+1th is rejected; releasing one frees a slot. The
// counter is per-sandbox, so one sandbox saturating its cap never blocks
// another.
func TestStreamCapAcquireRelease(t *testing.T) {
	api := NewSandboxAPI(t.TempDir())
	api.SetMaxStreamsPerSandbox(2)

	rel1, ok := api.acquireStream("sb-a")
	if !ok {
		t.Fatal("1st stream must be admitted")
	}
	rel2, ok := api.acquireStream("sb-a")
	if !ok {
		t.Fatal("2nd stream must be admitted (at the cap)")
	}
	if _, ok := api.acquireStream("sb-a"); ok {
		t.Fatal("3rd stream must be rejected (over the cap)")
	}

	// A different sandbox is independent: its own cap is untouched.
	relB, ok := api.acquireStream("sb-b")
	if !ok {
		t.Fatal("a different sandbox must have its own cap")
	}
	relB()

	// Releasing one frees a slot for sb-a.
	rel1()
	rel3, ok := api.acquireStream("sb-a")
	if !ok {
		t.Fatal("after releasing one, a new stream must be admitted")
	}
	rel2()
	rel3()

	// Fully drained: the per-sandbox counter is removed so the map does not grow
	// unbounded across sandbox lifetimes.
	api.mu.RLock()
	_, present := api.openStreams["sb-a"]
	api.mu.RUnlock()
	if present {
		t.Error("a fully-drained sandbox must not retain a counter entry")
	}
}

// TestStreamCapDisabledByDefault verifies the cap is off (unbounded) until
// configured, preserving the prior behavior for callers that do not set it.
func TestStreamCapDisabledByDefault(t *testing.T) {
	api := NewSandboxAPI(t.TempDir())
	var releases []func()
	for i := 0; i < 1000; i++ {
		rel, ok := api.acquireStream("sb")
		if !ok {
			t.Fatalf("with the cap disabled, stream %d must be admitted", i)
		}
		releases = append(releases, rel)
	}
	for _, r := range releases {
		r()
	}
}

// TestStreamCapConcurrent exercises the counter under concurrent acquire/release
// to confirm it never exceeds the configured cap.
func TestStreamCapConcurrent(t *testing.T) {
	api := NewSandboxAPI(t.TempDir())
	const cap = 8
	api.SetMaxStreamsPerSandbox(cap)

	var wg sync.WaitGroup
	var mu sync.Mutex
	live := 0
	maxLive := 0
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, ok := api.acquireStream("sb")
			if !ok {
				return
			}
			mu.Lock()
			live++
			if live > maxLive {
				maxLive = live
			}
			mu.Unlock()
			mu.Lock()
			live--
			mu.Unlock()
			rel()
		}()
	}
	wg.Wait()
	if maxLive > cap {
		t.Fatalf("observed %d concurrent streams, cap is %d", maxLive, cap)
	}
}

// The HTTP-surface cap rejection (a NEW interactive Exec over the cap is refused
// without touching existing streams) is covered for the live runtime path by
// TestExecWSStreamCapRejected in pty_test.go: the legacy /v1/exec/stream wire was
// removed in #358, and the ws Exec endpoint shares the same acquireStream gate.
