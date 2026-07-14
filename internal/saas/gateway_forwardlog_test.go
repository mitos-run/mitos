package saas

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// forwardLogFixture is the gateway fixture wired to a JSON slog handler, so a
// test can assert the exact structured fields the forward completion line
// carries. Issue #901: a client-observed hang was unattributable because the
// gateway logged only a forward ENTRY line, with neither the response status
// nor where the time went.
type forwardLogFixture struct {
	gatewayFixture
	logs *bytes.Buffer
}

func newForwardLogFixture(t *testing.T) forwardLogFixture {
	t.Helper()
	f := newGatewayFixture(t, nil)
	logs := &bytes.Buffer{}
	f.gw.log = slog.New(slog.NewJSONHandler(logs, nil))
	return forwardLogFixture{gatewayFixture: f, logs: logs}
}

// doneLine returns the decoded "gateway forward done" log record, failing the
// test if none (or more than one) was emitted.
func (f forwardLogFixture) doneLine(t *testing.T) map[string]any {
	t.Helper()
	var found []map[string]any
	dec := json.NewDecoder(bytes.NewReader(f.logs.Bytes()))
	for {
		var rec map[string]any
		if err := dec.Decode(&rec); err != nil {
			break
		}
		if rec["msg"] == "gateway forward done" {
			found = append(found, rec)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one 'gateway forward done' line, got %d; full log:\n%s", len(found), f.logs.String())
	}
	return found[0]
}

// requireNumber asserts a log field is present and a non-negative number, and
// returns it.
func requireNumber(t *testing.T, rec map[string]any, field string) float64 {
	t.Helper()
	v, ok := rec[field].(float64)
	if !ok {
		t.Fatalf("forward done line missing numeric %q: %v", field, rec)
	}
	if v < 0 {
		t.Fatalf("%q = %v, want >= 0", field, v)
	}
	return v
}

// TestForwardDoneLineCarriesStatusAndDurations pins the #901 contract: one
// completion line per forwarded request, carrying the response status, the
// control-plane forward duration, and the client response-write duration and
// byte count as SEPARATE fields, so a client-observed hang is attributable to a
// leg from the gateway log alone. The line must identify the request the same
// way the entry line does (key_id, org, op) and must never carry the key or a
// token value.
func TestForwardDoneLineCarriesStatusAndDurations(t *testing.T) {
	f := newForwardLogFixture(t)
	rec := doRequest(f.gw, http.MethodPost, "/v1/sandboxes", f.rawA, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	line := f.doneLine(t)
	if got := line["status"]; got != float64(http.StatusOK) {
		t.Errorf("status field = %v, want 200", got)
	}
	requireNumber(t, line, "forward_ms")
	requireNumber(t, line, "write_ms")
	if got := requireNumber(t, line, "bytes"); got != float64(len(`{"ok":true}`)) {
		t.Errorf("bytes = %v, want %d", got, len(`{"ok":true}`))
	}
	if line["org"] != f.orgA {
		t.Errorf("org field = %v, want %q", line["org"], f.orgA)
	}
	if line["op"] == nil || line["key_id"] == nil {
		t.Errorf("done line must identify op and key_id: %v", line)
	}
	if strings.Contains(f.logs.String(), f.rawA) {
		t.Errorf("the raw api key leaked into the log")
	}
}

// TestForwardDoneLineOnControlPlaneError pins that the completion line also
// fires when the control-plane forward itself fails: the 4/100 prod timeouts
// were invisible precisely because only successes left any trace at all.
func TestForwardDoneLineOnControlPlaneError(t *testing.T) {
	f := newForwardLogFixture(t)
	f.cp.err = errors.New("boom")

	rec := doRequest(f.gw, http.MethodPost, "/v1/sandboxes", f.rawA, `{}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}

	line := f.doneLine(t)
	if got := line["status"]; got != float64(http.StatusInternalServerError) {
		t.Errorf("status field = %v, want 500", got)
	}
	requireNumber(t, line, "forward_ms")
	if line["error"] == nil {
		t.Errorf("a failed forward must say so on the done line: %v", line)
	}
}

// TestForwardDoneLineOnStreamedResponse pins the streamed (runtime proxy)
// response leg: the done line fires AFTER the stream is fully copied to the
// client and reports the streamed byte count, so a wedged or slow stream shows
// up as write_ms on this line instead of vanishing.
func TestForwardDoneLineOnStreamedResponse(t *testing.T) {
	f := newForwardLogFixture(t)
	const streamed = "streamed-connect-frames"
	f.cp.respStream = io.NopCloser(strings.NewReader(streamed))
	f.cp.respBody = nil

	rec := doRequest(f.gw, http.MethodPost, "/v1/sandboxes/sb-1/runtime/exec", f.rawA, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != streamed {
		t.Fatalf("streamed body = %q, want %q", rec.Body.String(), streamed)
	}

	line := f.doneLine(t)
	if got := requireNumber(t, line, "bytes"); got != float64(len(streamed)) {
		t.Errorf("bytes = %v, want %d", got, len(streamed))
	}
	requireNumber(t, line, "write_ms")
}

// failingWriter wraps a ResponseWriter and fails every body write, the shape of
// a client that stopped reading (the prod read-timeout signature). The
// completion line must record the write error instead of swallowing it.
type failingWriter struct {
	http.ResponseWriter
}

func (w failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("client went away")
}

func (w failingWriter) FlushError() error { return nil }

func (w failingWriter) ReadFrom(r io.Reader) (int64, error) {
	// io.Copy prefers ReadFrom when the writer offers it; fail there too so the
	// wrapped recorder can never accept the bytes behind Write's back.
	return 0, errors.New("client went away")
}

func TestForwardDoneLineRecordsClientWriteError(t *testing.T) {
	f := newForwardLogFixture(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+f.rawA)
	f.gw.ServeHTTP(failingWriter{httptest.NewRecorder()}, req)

	line := f.doneLine(t)
	we, _ := line["write_error"].(string)
	if !strings.Contains(we, "client went away") {
		t.Errorf("write_error = %q, want the client write failure recorded", we)
	}
}
