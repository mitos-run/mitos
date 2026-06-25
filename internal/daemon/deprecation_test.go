package daemon

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestLegacyRuntimeRoutesCarryDeprecationHeader asserts the legacy JSON /v1
// runtime endpoints (those with a live Connect successor) carry the RFC 8594
// Deprecation header and a Link to the successor protocol, so a caller is told
// the JSON runtime shape is deprecated in favor of the Connect sandbox.v1.Sandbox
// protocol (issue #24, "the old HTTP shape is removed or shimmed with a
// deprecation note"). The header is set regardless of the response status, so a
// caller learns of the deprecation even on an error.
func TestLegacyRuntimeRoutesCarryDeprecationHeader(t *testing.T) {
	api := NewSandboxAPI(t.TempDir())
	if err := api.RegisterSandbox("sb-1", "/nonexistent.sock"); err != nil {
		t.Fatalf("RegisterSandbox: %v", err)
	}
	api.AllowTokenless() // so requireBearer lets the request reach the handler
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	runtimeRoutes := []string{
		"/v1/exec",
		"/v1/exec/stream",
		"/v1/run_code/stream",
		"/v1/files/read",
		"/v1/files/write",
		"/v1/files/list",
		"/v1/files/mkdir",
		"/v1/files/remove",
		"/v1/vitals",
	}
	for _, route := range runtimeRoutes {
		t.Run(route, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, ts.URL+route, bytes.NewReader([]byte(`{"sandbox":"sb-1"}`)))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request %s: %v", route, err)
			}
			defer resp.Body.Close()
			if got := resp.Header.Get("Deprecation"); got != "true" {
				t.Errorf("%s: Deprecation header = %q, want \"true\"", route, got)
			}
			if link := resp.Header.Get("Link"); !bytes.Contains([]byte(link), []byte("successor-version")) {
				t.Errorf("%s: Link header = %q, want it to name the successor-version", route, link)
			}
		})
	}
}

// TestLifecycleRoutesAreNotDeprecated asserts the lifecycle/management JSON
// routes that have NO Connect runtime successor (set_timeout, pause, resume) do
// NOT carry the Deprecation header: only the runtime exec/files surface that
// Connect replaces is deprecated, not the whole /v1 API.
func TestLifecycleRoutesAreNotDeprecated(t *testing.T) {
	api := NewSandboxAPI(t.TempDir())
	if err := api.RegisterSandbox("sb-1", "/nonexistent.sock"); err != nil {
		t.Fatalf("RegisterSandbox: %v", err)
	}
	api.AllowTokenless()
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	for _, route := range []string{"/v1/set_timeout", "/v1/pause", "/v1/resume"} {
		req, err := http.NewRequest(http.MethodPost, ts.URL+route, bytes.NewReader([]byte(`{"sandbox":"sb-1"}`)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request %s: %v", route, err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("Deprecation"); got != "" {
			t.Errorf("%s: lifecycle route must NOT be deprecated, got Deprecation=%q", route, got)
		}
	}
}
