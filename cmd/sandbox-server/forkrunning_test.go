package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// liveForkRequest POSTs the live-fork endpoint /v1/sandboxes/{source}/fork with
// the given child id and pause flag, driving the handler through the ServeMux
// path matcher so {id} is populated exactly as production serves it.
func liveForkRequest(t *testing.T, s *server, source, child string, pause bool) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"id": child, "pause_source": pause})
	r := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+source+"/fork", bytes.NewReader(body))
	r.SetPathValue("id", source)
	w := httptest.NewRecorder()
	s.handleForkRunning(w, r)
	return w
}

// TestLiveForkOfRunningSandboxReseedsAndSucceeds proves the standalone server's
// live-fork endpoint forks an already-running sandbox (source resolved from the
// PATH, not a template), runs the same fail-closed reseed handshake a cold fork
// runs, and returns a ready child that inherits the source's template id. With
// no KVM engine wired (engine==nil) the handler takes the vsock unix-fallback
// path, so the reseed handshake still runs against the fake agent; the real
// memory+disk carry is proven by the KVM acceptance test.
func TestLiveForkOfRunningSandboxReseedsAndSucceeds(t *testing.T) {
	const source = "sb-src"
	const child = "sb-child"
	s, notifies := realServerWithAgent(t, child, true)
	// A live fork targets a RUNNING sandbox, so the source must already exist.
	s.sandboxes[source] = &sandboxInfo{ID: source, TemplateID: source + "-tmpl", CreatedAt: time.Now()}

	w := liveForkRequest(t, s, source, child, true)
	if w.Code != http.StatusOK {
		t.Fatalf("live fork of a running sandbox: status %d, body %s", w.Code, w.Body.String())
	}
	if len(*notifies) != 1 {
		t.Fatalf("expected exactly one notify_forked reseed, got %d", len(*notifies))
	}
	if len((*notifies)[0].Entropy) != 32 {
		t.Errorf("reseed entropy length = %d, want 32", len((*notifies)[0].Entropy))
	}
	var got sandboxInfo
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.ID != child {
		t.Errorf("child id = %q, want %q", got.ID, child)
	}
	// The child inherits the SOURCE's template id, not a template named after the
	// child: a live fork descends from the running sandbox, not from a template.
	if got.TemplateID != source+"-tmpl" {
		t.Errorf("child template id = %q, want the source's %q", got.TemplateID, source+"-tmpl")
	}
	s.mu.RLock()
	_, registered := s.sandboxes[child]
	s.mu.RUnlock()
	if !registered {
		t.Fatal("a reseeded live fork must be registered")
	}
}

// TestLiveForkUnknownSourceReturns404 keeps the not-found path honest: a live
// fork of a sandbox that is not running returns 404, never a template lookup.
func TestLiveForkUnknownSourceReturns404(t *testing.T) {
	s := newServer(t.TempDir(), "", false, 16, 86400)
	w := liveForkRequest(t, s, "nope", "child", true)
	if w.Code != http.StatusNotFound {
		t.Fatalf("live fork of an unknown source: status %d, want 404, body %s", w.Code, w.Body.String())
	}
}

// TestLiveForkRejectsUnsafeIDs guards the path-traversal boundary: both the
// source (path component) and the child id (path component) must pass the safe
// id check, so neither can escape the sandboxes directory.
func TestLiveForkRejectsUnsafeIDs(t *testing.T) {
	s := newServer(t.TempDir(), "", false, 16, 86400)
	s.sandboxes["ok-src"] = &sandboxInfo{ID: "ok-src", TemplateID: "t"}
	// Unsafe child id.
	w := liveForkRequest(t, s, "ok-src", "../escape", true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unsafe child id: status %d, want 400", w.Code)
	}
	// Unsafe source id.
	w2 := liveForkRequest(t, s, "../escape", "child", true)
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("unsafe source id: status %d, want 400", w2.Code)
	}
}

// TestLiveForkIdempotencyKeyReturnsSameChild proves a repeat live fork under the
// same Idempotency-Key returns the child the first call forked, never a second
// one, matching the cold fork contract (issue #22).
func TestLiveForkIdempotencyKeyReturnsSameChild(t *testing.T) {
	const source = "sb-idem-src"
	const child = "sb-idem-child"
	s, _ := realServerWithAgent(t, child, true)
	s.sandboxes[source] = &sandboxInfo{ID: source, TemplateID: source + "-tmpl"}

	do := func() *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"id": child, "pause_source": true})
		r := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+source+"/fork", bytes.NewReader(body))
		r.SetPathValue("id", source)
		r.Header.Set("Idempotency-Key", "live-k1")
		w := httptest.NewRecorder()
		s.handleForkRunning(w, r)
		return w
	}
	w1 := do()
	if w1.Code != http.StatusOK {
		t.Fatalf("first live fork: status %d body %s", w1.Code, w1.Body.String())
	}
	w2 := do()
	if w2.Code != http.StatusOK {
		t.Fatalf("idempotent replay live fork: status %d body %s", w2.Code, w2.Body.String())
	}
	var a, b sandboxInfo
	_ = json.Unmarshal(w1.Body.Bytes(), &a)
	_ = json.Unmarshal(w2.Body.Bytes(), &b)
	if a.ID != b.ID {
		t.Fatalf("idempotent replay returned a different child: %q vs %q", a.ID, b.ID)
	}
}
