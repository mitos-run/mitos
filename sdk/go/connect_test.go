package mitos

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// b64 standard-base64-encodes s, matching proto-JSON's encoding of bytes fields
// (stdout/stderr) that the SDK decodes back to raw bytes.
func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// writeFrame writes one Connect enveloped frame (1 flag byte + 4-byte big-endian
// length + payload) to w, mirroring the server side of the wire the SDK parses.
func writeFrame(t *testing.T, w io.Writer, flag byte, payload []byte) {
	t.Helper()
	var hdr [5]byte
	hdr[0] = flag
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		t.Fatalf("write frame header: %v", err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("write frame payload: %v", err)
	}
}

// execStreamServer is a fake Connect sandbox.v1.Sandbox/ExecStream endpoint that
// streams the given response frames. It records the request it saw so a test can
// assert the path, headers, and request message the SDK sent.
func execStreamServer(t *testing.T, frames func(w io.Writer)) (*httptest.Server, *recordedConnectRequest) {
	t.Helper()
	rec := &recordedConnectRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.path = r.URL.Path
		rec.contentType = r.Header.Get("Content-Type")
		rec.sandboxID = r.Header.Get("X-Sandbox-Id")
		rec.authorization = r.Header.Get("Authorization")
		// The request body is a single enveloped ExecStreamRequest frame.
		body, _ := io.ReadAll(r.Body)
		if len(body) >= 5 {
			rec.requestPayload = body[5:]
		}
		w.Header().Set("Content-Type", "application/connect+json")
		w.WriteHeader(http.StatusOK)
		frames(w)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

type recordedConnectRequest struct {
	path           string
	contentType    string
	sandboxID      string
	authorization  string
	requestPayload []byte
}

func newSandboxFor(t *testing.T, baseURL, token string) *Sandbox {
	t.Helper()
	opts := []Option{WithBaseURL(baseURL)}
	if token != "" {
		opts = append(opts, WithAPIKey(token))
	}
	return &Sandbox{ID: "sb-1", server: NewSandboxServer(opts...)}
}

// TestExecStreamOverConnect proves the Go SDK execs over the Connect
// sandbox.v1.Sandbox/ExecStream RPC: it sends a single enveloped ExecStreamRequest
// to the correct path with the X-Sandbox-Id and bearer headers, then yields each
// stdout/stderr chunk incrementally as its frame arrives, and exposes the exit
// code from the terminal exit frame.
func TestExecStreamOverConnect(t *testing.T) {
	srv, rec := execStreamServer(t, func(w io.Writer) {
		f, _ := w.(http.Flusher)
		writeFrame(t, w, 0, []byte(`{"stdout":"`+b64("hello\n")+`"}`))
		if f != nil {
			f.Flush()
		}
		writeFrame(t, w, 0, []byte(`{"stderr":"`+b64("oops\n")+`"}`))
		writeFrame(t, w, 0, []byte(`{"exit":{"exitCode":0,"execTimeMs":12.5}}`))
		// End-stream frame (flag 0x02), clean (no error).
		writeFrame(t, w, 0x02, []byte(`{}`))
	})

	sb := newSandboxFor(t, srv.URL, "tok-123")
	st, err := sb.ExecStream(context.Background(), "echo hi")
	if err != nil {
		t.Fatalf("ExecStream: %v", err)
	}
	defer st.Close()

	var stdout, stderr strings.Builder
	for {
		chunk, err := st.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		stdout.Write(chunk.Stdout)
		stderr.Write(chunk.Stderr)
	}
	if stdout.String() != "hello\n" {
		t.Errorf("stdout = %q, want %q", stdout.String(), "hello\n")
	}
	if stderr.String() != "oops\n" {
		t.Errorf("stderr = %q, want %q", stderr.String(), "oops\n")
	}
	if res := st.Result(); res.ExitCode != 0 || res.ExecTimeMs != 12.5 {
		t.Errorf("Result = %+v, want exit 0 / 12.5ms", res)
	}

	// The SDK targeted the Connect ExecStream RPC with the streaming content type
	// and the routing + auth headers.
	if rec.path != "/sandbox.v1.Sandbox/ExecStream" {
		t.Errorf("path = %q, want /sandbox.v1.Sandbox/ExecStream", rec.path)
	}
	if rec.contentType != "application/connect+json" {
		t.Errorf("content-type = %q, want application/connect+json", rec.contentType)
	}
	if rec.sandboxID != "sb-1" {
		t.Errorf("X-Sandbox-Id = %q, want sb-1", rec.sandboxID)
	}
	if rec.authorization != "Bearer tok-123" {
		t.Errorf("Authorization = %q, want Bearer tok-123", rec.authorization)
	}
	// The request message carried the command as proto-JSON (camelCase).
	var reqMsg map[string]any
	if err := json.Unmarshal(rec.requestPayload, &reqMsg); err != nil {
		t.Fatalf("request payload not JSON: %v (%q)", err, rec.requestPayload)
	}
	if reqMsg["command"] != "echo hi" {
		t.Errorf("request command = %v, want echo hi", reqMsg["command"])
	}
}

// TestExecBuffersOverConnect proves the convenience Exec drains the Connect
// stream into a buffered ExecResult (retiring the legacy /v1/exec path): the same
// frames yield concatenated stdout and the exit code.
func TestExecBuffersOverConnect(t *testing.T) {
	srv, _ := execStreamServer(t, func(w io.Writer) {
		writeFrame(t, w, 0, []byte(`{"stdout":"`+b64("part1 ")+`"}`))
		writeFrame(t, w, 0, []byte(`{"stdout":"`+b64("part2")+`"}`))
		writeFrame(t, w, 0, []byte(`{"exit":{"exitCode":3}}`))
		writeFrame(t, w, 0x02, []byte(`{}`))
	})
	sb := newSandboxFor(t, srv.URL, "")
	res, err := sb.Exec(context.Background(), "echo hi")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Stdout != "part1 part2" {
		t.Errorf("Stdout = %q, want %q", res.Stdout, "part1 part2")
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
}

// TestExecStreamConnectError proves a Connect error on the terminal end-stream
// frame surfaces as a typed *Error (not a bare EOF), so a caller branches on the
// typed code and remediation exactly as the legacy path allowed.
func TestExecStreamConnectError(t *testing.T) {
	srv, _ := execStreamServer(t, func(w io.Writer) {
		writeFrame(t, w, 0x02, []byte(`{"error":{"code":"resource_exhausted","message":"too many streams"}}`))
	})
	sb := newSandboxFor(t, srv.URL, "")
	st, err := sb.ExecStream(context.Background(), "echo hi")
	if err != nil {
		t.Fatalf("ExecStream open: %v", err)
	}
	defer st.Close()
	_, recvErr := st.Recv()
	var apiErr *Error
	if !errors.As(recvErr, &apiErr) {
		t.Fatalf("Recv error = %v, want *Error", recvErr)
	}
	if apiErr.Code != "resource_exhausted" {
		t.Errorf("error code = %q, want resource_exhausted", apiErr.Code)
	}
}
