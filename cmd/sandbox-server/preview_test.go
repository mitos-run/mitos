package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"mitos.run/mitos/internal/preview"
)

// previewTestServer builds a server with the preview signer enabled and one
// known sandbox registered.
func previewTestServer(t *testing.T) *server {
	t.Helper()
	s := newServer(t.TempDir(), "", true, 16, 86400)
	signer, err := preview.NewSigner([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	s.previewSigner = signer
	s.previewDomain = "example.com"
	s.previewTTL = time.Hour
	s.sandboxes["sb-1"] = &sandboxInfo{ID: "sb-1", TemplateID: "python"}
	return s
}

func postPreview(t *testing.T, s *server, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/v1/preview", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	s.handlePreview(rec, r)
	return rec
}

func TestHandlePreviewMintsSignedURL(t *testing.T) {
	s := previewTestServer(t)
	rec := postPreview(t, s, previewReq{Sandbox: "sb-1", Port: 8080})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		URL         string `json:"url"`
		ExpiresUnix int64  `json:"expires_unix"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	parsed, err := url.Parse(out.URL)
	if err != nil {
		t.Fatalf("unparseable URL %q: %v", out.URL, err)
	}
	if parsed.Host != "sb-1.preview.example.com" {
		t.Errorf("host = %q", parsed.Host)
	}
	tok := parsed.Query().Get("token")
	if tok == "" {
		t.Fatal("URL carries no token")
	}
	claims, err := s.previewSigner.Verify(tok)
	if err != nil {
		t.Fatalf("minted token does not verify: %v", err)
	}
	if claims.SandboxID != "sb-1" || claims.Port != 8080 {
		t.Errorf("claims = %+v", claims)
	}
}

func TestHandlePreviewUnknownSandbox(t *testing.T) {
	s := previewTestServer(t)
	rec := postPreview(t, s, previewReq{Sandbox: "nope", Port: 8080})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestHandlePreviewBadPort(t *testing.T) {
	s := previewTestServer(t)
	rec := postPreview(t, s, previewReq{Sandbox: "sb-1", Port: 0})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandlePreviewDisabled(t *testing.T) {
	// No signer configured: the route reports not-implemented rather than
	// fabricating a URL.
	s := newServer(t.TempDir(), "", true, 16, 86400)
	rec := postPreview(t, s, previewReq{Sandbox: "sb-1", Port: 8080})
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rec.Code)
	}
}
