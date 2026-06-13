package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// TestExecStreamRejectedOverCap covers the HTTP surface: when a sandbox is at
// its stream cap, a NEW /v1/exec/stream is rejected with 429 and the LLM-legible
// error envelope, WITHOUT touching the existing streams. The cap is checked at
// stream OPEN, before the dedicated vsock connection is dialed.
func TestExecStreamRejectedOverCap(t *testing.T) {
	api, _ := newStreamAPI(t)
	api.SetMaxStreamsPerSandbox(1)

	// Saturate the sandbox's single slot, simulating one in-flight stream.
	rel, ok := api.acquireStream("sb1")
	if !ok {
		t.Fatal("setup: first slot must be acquirable")
	}
	defer rel()

	body, _ := json.Marshal(execRequest{Sandbox: "sb1", Command: "echo hi"})
	req := httptest.NewRequest(http.MethodPost, "/v1/exec/stream", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	api.handleExecStream(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	var env struct {
		Error struct {
			Code        string `json:"code"`
			Remediation string `json:"remediation"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v (body=%q)", err, rec.Body.String())
	}
	if env.Error.Code != "too_many_streams" {
		t.Errorf("error code = %q, want too_many_streams", env.Error.Code)
	}
	if env.Error.Remediation == "" {
		t.Error("error envelope must carry actionable remediation")
	}
}
