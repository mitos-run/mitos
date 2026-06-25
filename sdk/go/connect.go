package mitos

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// This file implements the Connect sandbox.v1.Sandbox runtime protocol (issue
// #24) directly over the SDK's existing standard-library transport, so the Go
// SDK gains streaming exec/files WITHOUT a gRPC runtime or a codegen step: it
// stays dependency-free. It mirrors the Python SDK's _connect.py reference; the
// proto-JSON message shapes come straight from proto/sandbox/v1/sandbox.proto
// (camelCase field names; bytes fields are base64 strings, which encoding/json
// decodes back to []byte automatically).
//
// The protocol has two shapes used here:
//
//   - UNARY (List, Stat, Mkdir, Remove): POST /sandbox.v1.Sandbox/<Method> with
//     Content-Type application/json and the proto-JSON request as the body. On
//     2xx the body is the proto-JSON reply; on non-2xx it is the Connect error
//     envelope {"code","message"}.
//   - SERVER-STREAM (ExecStream, ReadFile, Vitals, Watch): Content-Type
//     application/connect+json. Each message is an ENVELOPED frame: a 5-byte
//     prefix (1 flag byte + 4-byte big-endian length) then the JSON payload. The
//     final server frame sets the end-stream flag (0x02); its payload carries
//     trailers and, on failure, an {"error":{...}} object. The client sends its
//     request message(s) as plain (flag 0x00) frames and closes the body.
//
// The bearer token rides on Authorization and is never logged; it is redacted
// from any error cause by the shared transport redactor.

const (
	connectServiceName       = "sandbox.v1.Sandbox"
	connectStreamContentType = "application/connect+json"
	connectUnaryContentType  = "application/json"
	sandboxIDHeader          = "X-Sandbox-Id"

	// flagEndStream marks the terminal Connect frame; its payload carries
	// trailers and an optional error object.
	flagEndStream byte = 0b00000010
	// flagCompressed marks a compressed frame. The SDK negotiates identity
	// encoding and never sends or accepts a compressed frame, so it is only used
	// to refuse an unexpected one.
	flagCompressed byte = 0b00000001

	// maxConnectFrameBytes guards the frame-length prefix so a malformed or
	// hostile length cannot make the SDK allocate unbounded memory.
	maxConnectFrameBytes = 64 << 20 // 64 MiB
)

// connectCodeStatus maps the Connect textual error codes to the HTTP-ish status
// the SDK's typed-error layer keys remediation on. It is the subset the Sandbox
// service returns; an unmapped code falls back to 500. Mirrors the Python
// _connect.py map.
var connectCodeStatus = map[string]int{
	"canceled":            499,
	"unknown":             500,
	"invalid_argument":    400,
	"deadline_exceeded":   504,
	"not_found":           404,
	"already_exists":      409,
	"permission_denied":   403,
	"resource_exhausted":  429,
	"failed_precondition": 400,
	"aborted":             409,
	"out_of_range":        400,
	"unimplemented":       501,
	"internal":            500,
	"unavailable":         503,
	"data_loss":           500,
	"unauthenticated":     401,
}

// connectPath is the Connect RPC path for a Sandbox method name (e.g.
// "ExecStream" -> "/sandbox.v1.Sandbox/ExecStream").
func connectPath(method string) string {
	return "/" + connectServiceName + "/" + method
}

// encodeFrame wraps one message payload in the Connect 5-byte envelope prefix.
func encodeFrame(payload []byte, endStream bool) []byte {
	var flag byte
	if endStream {
		flag = flagEndStream
	}
	out := make([]byte, 5+len(payload))
	out[0] = flag
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out
}

// connectStream is an open server-streaming Connect call. Recv yields each
// response message payload; it returns io.EOF on a clean end-stream frame and a
// typed *Error on an error end-stream frame.
type connectStream struct {
	t    *transport
	body io.ReadCloser
	r    *bufio.Reader
	done bool
	err  error
}

// connectServerStream opens a server-streaming Connect call: it sends msg as the
// single opening enveloped frame and returns a stream over the response. The
// response body stays open and is read incrementally by Recv. A non-2xx status
// on open (the server rejected before streaming) is parsed into a typed error.
func (t *transport) connectServerStream(ctx context.Context, method, sandboxID string, msg any) (*connectStream, error) {
	payload, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("encode %s request: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+connectPath(method), bytes.NewReader(encodeFrame(payload, false)))
	if err != nil {
		return nil, fmt.Errorf("build %s request: %w", method, err)
	}
	req.Header.Set("Content-Type", connectStreamContentType)
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set(sandboxIDHeader, sandboxID)
	if t.token != "" {
		req.Header.Set("Authorization", "Bearer "+t.token)
	}

	resp, err := t.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return nil, t.connectErrorFromBody(resp.StatusCode, b)
	}
	return &connectStream{t: t, body: resp.Body, r: bufio.NewReader(resp.Body)}, nil
}

// Recv returns the next response message payload. It returns io.EOF on a clean
// end-stream frame (or a clean transport EOF) and a typed *Error on an error
// end-stream frame. After a terminal result every subsequent call repeats it.
func (s *connectStream) Recv() ([]byte, error) {
	if s.done {
		if s.err != nil {
			return nil, s.err
		}
		return nil, io.EOF
	}
	flag, payload, err := s.recvFrame()
	if err != nil {
		s.done = true
		// A clean EOF without an explicit end-stream frame is treated as the end
		// of the stream; any other read error surfaces as-is.
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		s.err = err
		return nil, err
	}
	if flag&flagEndStream != 0 {
		s.done = true
		s.err = s.parseEndStream(payload)
		if s.err != nil {
			return nil, s.err
		}
		return nil, io.EOF
	}
	return payload, nil
}

// recvFrame reads one enveloped frame (1 flag byte + 4-byte big-endian length +
// payload). It refuses a compressed frame and a length over the guard.
func (s *connectStream) recvFrame() (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(s.r, hdr[:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, nil, io.EOF
		}
		return 0, nil, err
	}
	flag := hdr[0]
	n := binary.BigEndian.Uint32(hdr[1:5])
	if flag&flagCompressed != 0 {
		return 0, nil, fmt.Errorf("connect: unexpected compressed response frame")
	}
	if n > maxConnectFrameBytes {
		return 0, nil, fmt.Errorf("connect: response frame too large (%d bytes)", n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(s.r, payload); err != nil {
		return 0, nil, fmt.Errorf("connect: short frame payload: %w", err)
	}
	return flag, payload, nil
}

// parseEndStream inspects the terminal end-stream frame payload. A payload with
// an {"error":{...}} object yields a typed *Error; a clean payload (trailers
// only) yields nil. A non-JSON payload is tolerated as a clean end.
func (s *connectStream) parseEndStream(payload []byte) error {
	var end struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &end); err != nil {
		return nil
	}
	if end.Error == nil {
		return nil
	}
	return s.t.connectError(end.Error.Code, end.Error.Message)
}

// Close releases the underlying response body. It is safe to call more than once.
func (s *connectStream) Close() error {
	if s.body == nil {
		return nil
	}
	return s.body.Close()
}

// connectError builds a typed *Error from a Connect error code and message. The
// Connect textual code is the stable Code; the status is mapped so the typed
// layer picks the right remediation, and the message is redacted of any token.
func (t *transport) connectError(code, message string) *Error {
	status := connectCodeStatus[code]
	if status == 0 {
		status = 500
	}
	stable := code
	if stable == "" {
		stable = "internal"
	}
	return &Error{
		Code:        stable,
		Message:     fmt.Sprintf("sandbox RPC failed: %s", stable),
		Cause:       t.redact(message),
		Remediation: "Inspect the request against the sandbox.v1.Sandbox contract.",
		Status:      status,
	}
}

// connectErrorFromBody turns a non-2xx Connect response into a typed *Error. It
// prefers the Connect error envelope {"code","message"}; when the body is not the
// envelope (a proxy 502, a transport error) it falls back to the raw redacted
// body and the HTTP status.
func (t *transport) connectErrorFromBody(status int, body []byte) error {
	var env struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Code != "" {
		e := t.connectError(env.Code, env.Message)
		e.Status = status
		return e
	}
	return &Error{
		Code:    "http_error",
		Message: fmt.Sprintf("server returned HTTP %d", status),
		Cause:   t.redact(strings.TrimSpace(string(body))),
		Status:  status,
	}
}
