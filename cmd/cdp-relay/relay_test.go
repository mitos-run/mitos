package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRewriteDiscoveryWS127 covers the ws://127.0.0.1:PORT form.
func TestRewriteDiscoveryWS127(t *testing.T) {
	body := []byte(`{"webSocketDebuggerUrl":"ws://127.0.0.1:9223/devtools/browser/ABC"}`)
	got := rewriteDiscovery(body, "127.0.0.1:9223", "wss", "example.test")
	want := "wss://example.test/devtools/browser/ABC"
	if !strings.Contains(string(got), want) {
		t.Errorf("rewriteDiscovery: got %q, want substring %q", got, want)
	}
	if strings.Contains(string(got), "127.0.0.1:9223") {
		t.Errorf("rewriteDiscovery: output still contains original host: %q", got)
	}
}

// TestRewriteDiscoveryWSLocalhost covers the ws://localhost:PORT form that
// Chromium sometimes emits instead of the IP form.
func TestRewriteDiscoveryWSLocalhost(t *testing.T) {
	body := []byte(`{"webSocketDebuggerUrl":"ws://localhost:9223/devtools/browser/ABC"}`)
	got := rewriteDiscovery(body, "127.0.0.1:9223", "wss", "example.test")
	want := "wss://example.test/devtools/browser/ABC"
	if !strings.Contains(string(got), want) {
		t.Errorf("rewriteDiscovery: got %q, want substring %q", got, want)
	}
	if strings.Contains(string(got), "localhost:9223") {
		t.Errorf("rewriteDiscovery: output still contains original host: %q", got)
	}
}

// TestRewriteDiscoveryDevtoolsFrontendUrl covers devtoolsFrontendUrl fields.
func TestRewriteDiscoveryDevtoolsFrontendUrl(t *testing.T) {
	body := []byte(`{"devtoolsFrontendUrl":"ws://127.0.0.1:9223/devtools/inspector.html"}`)
	got := rewriteDiscovery(body, "127.0.0.1:9223", "ws", "example.test")
	want := "ws://example.test/devtools/inspector.html"
	if !strings.Contains(string(got), want) {
		t.Errorf("rewriteDiscovery: got %q, want substring %q", got, want)
	}
}

// TestRelayEndToEnd proves two things in a single httptest pass:
//  1. The relay sets a Host the Chromium DevTools check accepts (an IP literal
//     or localhost) on the upstream request. The fake upstream mimics that check
//     and returns 403 for any other Host, so a 200 proves the rewrite.
//  2. The relay rewrites webSocketDebuggerUrl in /json* responses from the
//     upstream loopback address to the external origin from X-Forwarded-Host.
func TestRelayEndToEnd(t *testing.T) {
	// Obtain a free port before starting the server so the body template can
	// reference the actual host:port without a data race.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	upstreamHostPort := "127.0.0.1:" + port

	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Mimic Chromium's DevTools host-header check: accept only an IP-literal
		// or localhost host-part. This proves the relay sends such a Host (it
		// sends the upstream host:port, e.g. 127.0.0.1:PORT).
		host := r.Host
		if h, _, err := net.SplitHostPort(r.Host); err == nil {
			host = h
		}
		if host != "localhost" && net.ParseIP(host) == nil {
			http.Error(w, "forbidden: bad Host "+r.Host, http.StatusForbidden)
			return
		}
		if r.URL.Path == "/json/version" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"webSocketDebuggerUrl":"ws://`+upstreamHostPort+`/devtools/browser/ABC"}`)
			return
		}
		http.NotFound(w, r)
	}))
	upstream.Listener = listener
	upstream.Start()
	defer upstream.Close()

	handler := newRelayHandler(upstreamHostPort)

	req := httptest.NewRequest(http.MethodGet, "/json/version", nil)
	req.Header.Set("X-Forwarded-Host", "example.test")
	req.Header.Set("X-Forwarded-Proto", "https")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (upstream rejected the Host header or proxy failed)\nbody: %s",
			rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	want := "wss://example.test/devtools/browser/ABC"
	if !strings.Contains(body, want) {
		t.Errorf("body: got %q, want it to contain %q", body, want)
	}
}

// TestRelayDirectForwardUsesRequestHost proves the host-forward path: with no
// X-Forwarded-Host (a raw TCP forward sets none), the relay rewrites the
// discovery URL to the client's own request Host, which is the reachable origin.
// Without this the body would keep the guest-internal upstream port, unreachable
// to the client.
func TestRelayDirectForwardUsesRequestHost(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	upstreamHostPort := "127.0.0.1:" + port

	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/json/version" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"webSocketDebuggerUrl":"ws://`+upstreamHostPort+`/devtools/browser/ABC"}`)
			return
		}
		http.NotFound(w, r)
	}))
	upstream.Listener = listener
	upstream.Start()
	defer upstream.Close()

	handler := newRelayHandler(upstreamHostPort)
	req := httptest.NewRequest(http.MethodGet, "/json/version", nil)
	req.Host = "127.0.0.1:42999" // the client's forward address; no X-Forwarded-Host
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
	want := "ws://127.0.0.1:42999/devtools/browser/ABC"
	if body := rec.Body.String(); !strings.Contains(body, want) {
		t.Errorf("body: got %q, want it to contain %q", body, want)
	}
}
