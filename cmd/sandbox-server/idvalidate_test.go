package main

import (
	"net/http"
	"testing"
)

// TestCreateTemplateRejectsUnsafeID proves the REST boundary rejects a template
// id that is not a safe single path segment with a 400, before it can reach the
// engine and be joined into a host path (CodeQL go/path-injection). The forkd
// gRPC boundary already validates ids; this closes the standalone server gap.
func TestCreateTemplateRejectsUnsafeID(t *testing.T) {
	s := newServer(t.TempDir(), "", true, 16, 86400) // mock mode
	for _, id := range []string{"../../etc", "a/b", "..", ".", `a\b`, "with space"} {
		w := createTemplateRequest(t, s, id, "")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("createTemplate id=%q: status %d, want 400 (body %s)", id, w.Code, w.Body.String())
		}
	}
}

// TestForkRejectsUnsafeID proves the REST boundary rejects an unsafe new sandbox
// id with a 400.
func TestForkRejectsUnsafeID(t *testing.T) {
	s := newServer(t.TempDir(), "", true, 16, 86400) // mock mode
	s.templates["tmpl"] = &templateInfo{ID: "tmpl", Ready: true}
	for _, id := range []string{"../../etc", "a/b", "..", `a\b`} {
		w := forkRequest(t, s, id, "tmpl")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("fork id=%q: status %d, want 400 (body %s)", id, w.Code, w.Body.String())
		}
	}
}

// TestForkRejectsUnsafeTemplate proves the REST boundary rejects an unsafe
// template id with a 400.
func TestForkRejectsUnsafeTemplate(t *testing.T) {
	s := newServer(t.TempDir(), "", true, 16, 86400) // mock mode
	for _, tmpl := range []string{"../../etc", "a/b", ".."} {
		w := forkRequest(t, s, "sb-1", tmpl)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("fork template=%q: status %d, want 400 (body %s)", tmpl, w.Code, w.Body.String())
		}
	}
}

// TestValidIDStillAccepted guards against the validator rejecting legitimate ids.
func TestValidIDStillAccepted(t *testing.T) {
	if !safeIDComponent("sb-1234") || !safeIDComponent("abcDEF_09") {
		t.Fatal("safeIDComponent rejected a legitimate id")
	}
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`, "../x", "/abs"} {
		if safeIDComponent(bad) {
			t.Fatalf("safeIDComponent accepted unsafe id %q", bad)
		}
	}
}
