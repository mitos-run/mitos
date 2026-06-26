package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestStdoutSinkJSONLines: StdoutSink writes one JSON object per line.
func TestStdoutSinkJSONLines(t *testing.T) {
	var buf bytes.Buffer
	s := NewStdoutSink(&buf)
	events := []sentEvent{
		{Name: "sandbox.created", OrgHash: "abc", Timestamp: time.Unix(1, 0).UTC()},
		{Name: "signup.started", Timestamp: time.Unix(2, 0).UTC()},
	}
	if err := s.Send(context.Background(), events); err != nil {
		t.Fatalf("send: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 JSON lines, got %d: %q", len(lines), buf.String())
	}
	var first sentEvent
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("line 0 not valid JSON: %v", err)
	}
	if first.Name != "sandbox.created" || first.OrgHash != "abc" {
		t.Fatalf("unexpected first line: %+v", first)
	}
}

// TestHTTPSinkPostsJSON: HTTPSink POSTs the batch as a JSON object with an
// events array and attaches the bearer token.
func TestHTTPSinkPostsJSON(t *testing.T) {
	var gotBody []byte
	var gotAuth, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	s := NewHTTPSink(srv.URL, "tok123", nil)
	events := []sentEvent{{Name: "sandbox.created", OrgHash: "h", Timestamp: time.Unix(1, 0).UTC()}}
	if err := s.Send(context.Background(), events); err != nil {
		t.Fatalf("send: %v", err)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q, want application/json", gotCT)
	}
	if gotAuth != "Bearer tok123" {
		t.Errorf("authorization = %q, want Bearer tok123", gotAuth)
	}
	var payload batchPayload
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("body not valid JSON: %v\n%s", err, gotBody)
	}
	if len(payload.Events) != 1 || payload.Events[0].Name != "sandbox.created" {
		t.Fatalf("unexpected payload: %+v", payload)
	}
}

// TestHTTPSinkNon2xxIsError: a non-2xx response is reported as an error so the
// flush loop can observe it.
func TestHTTPSinkNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	s := NewHTTPSink(srv.URL, "", nil)
	err := s.Send(context.Background(), []sentEvent{{Name: "x"}})
	if err == nil {
		t.Fatal("expected an error on a 500 response")
	}
}

// TestHTTPSinkNoTokenNoHeader: with no token, no Authorization header is sent.
func TestHTTPSinkNoTokenNoHeader(t *testing.T) {
	var hadAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadAuth = r.Header["Authorization"]
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	s := NewHTTPSink(srv.URL, "", nil)
	if err := s.Send(context.Background(), []sentEvent{{Name: "x"}}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if hadAuth {
		t.Fatal("Authorization header sent though no token was configured")
	}
}
