package daemon

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// wsExecURL builds the Connect-over-WebSocket Exec URL for sandbox from an
// httptest server base URL.
func wsExecURL(httpURL, sandbox string) string {
	s := httpURL
	if strings.HasPrefix(s, "http://") {
		s = "ws://" + s[len("http://"):]
	}
	return s + execWSPath + "?sandbox=" + sandbox
}

// frameMessage encodes one Connect enveloped frame independently of the
// production codec, so the test exercises the real wire format rather than the
// implementation's own helper. Layout: 1 flags byte, 4 big-endian length bytes,
// then the protojson payload.
func frameMessage(t *testing.T, end bool, msg proto.Message) []byte {
	t.Helper()
	payload, err := protojson.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	hdr := make([]byte, 5)
	if end {
		hdr[0] = connectFlagEndStream
	}
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	return append(hdr, payload...)
}

// unframeResponse decodes one Connect enveloped ExecResponse frame.
func unframeResponse(t *testing.T, b []byte) (byte, *sandboxv1.ExecResponse) {
	t.Helper()
	if len(b) < 5 {
		t.Fatalf("short frame: %d bytes", len(b))
	}
	flags := b[0]
	n := binary.BigEndian.Uint32(b[1:5])
	if int(n) != len(b)-5 {
		t.Fatalf("frame length %d != payload %d", n, len(b)-5)
	}
	var resp sandboxv1.ExecResponse
	if err := protojson.Unmarshal(b[5:], &resp); err != nil {
		t.Fatalf("unmarshal ExecResponse: %v", err)
	}
	return flags, &resp
}

func writeFrame(ctx context.Context, t *testing.T, c *websocket.Conn, end bool, msg proto.Message) {
	t.Helper()
	if err := c.Write(ctx, websocket.MessageBinary, frameMessage(t, end, msg)); err != nil {
		t.Fatalf("ws write: %v", err)
	}
}

func readResponse(ctx context.Context, t *testing.T, c *websocket.Conn) (byte, *sandboxv1.ExecResponse) {
	t.Helper()
	typ, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	if typ != websocket.MessageBinary {
		t.Fatalf("ws message type = %v, want binary", typ)
	}
	return unframeResponse(t, data)
}

// TestExecWSPtyEchoExit drives the Connect-over-WebSocket bidi Exec endpoint: it
// opens a PTY exec with an enveloped ExecRequest{open:{pty}}, sends stdin, reads
// the echoed stdout as an enveloped ExecResponse, then sends "exit\n" and reads
// the terminal exit frame with the end-stream flag set. The fake guest
// (fakePtyGuestSandbox) echoes stdin as stdout and exits on "exit\n".
func TestExecWSPtyEchoExit(t *testing.T) {
	_, srv := newPtyAPI(t, "sekret")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, wsExecURL(srv.URL, "sb1"), &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": {"Bearer sekret"}},
		Subprotocols: []string{execWSSubprotocol},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	// Open frame: a PTY exec, no command (guest defaults to /bin/sh).
	writeFrame(ctx, t, c, false, &sandboxv1.ExecRequest{
		Msg: &sandboxv1.ExecRequest_Open{Open: &sandboxv1.ExecOpen{
			Pty: &sandboxv1.PtyOptions{Size: &sandboxv1.WindowSize{Cols: 80, Rows: 24}},
		}},
	})

	// stdin -> echoed as stdout.
	writeFrame(ctx, t, c, false, &sandboxv1.ExecRequest{
		Msg: &sandboxv1.ExecRequest_Stdin{Stdin: []byte("hello-pty\n")},
	})
	_, resp := readResponse(ctx, t, c)
	if string(resp.GetStdout()) != "hello-pty\n" {
		t.Fatalf("stdout = %q, want %q", resp.GetStdout(), "hello-pty\n")
	}

	// "exit\n" -> terminal exit frame with the end-stream flag.
	writeFrame(ctx, t, c, false, &sandboxv1.ExecRequest{
		Msg: &sandboxv1.ExecRequest_Stdin{Stdin: []byte("exit\n")},
	})
	flags, ex := readResponse(ctx, t, c)
	if ex.GetExit() == nil {
		t.Fatalf("final frame = %+v, want exit", ex)
	}
	if ex.GetExit().GetExitCode() != 0 {
		t.Fatalf("exit code = %d, want 0", ex.GetExit().GetExitCode())
	}
	if flags&connectFlagEndStream == 0 {
		t.Fatalf("exit frame missing end-stream flag (flags=0x%02x)", flags)
	}
}

// TestExecWSRejectsBadToken asserts the bearer gate runs BEFORE the WebSocket
// upgrade: a wrong token yields a 401 on the handshake, not a post-upgrade close.
func TestExecWSRejectsBadToken(t *testing.T) {
	_, srv := newPtyAPI(t, "sekret")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, resp, err := websocket.Dial(ctx, wsExecURL(srv.URL, "sb1"), &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": {"Bearer wrong"}},
		Subprotocols: []string{execWSSubprotocol},
	})
	if err == nil {
		t.Fatal("dial succeeded with a wrong token, want 401")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v, want 401", resp)
	}
}

// ensure fmt is used (kept for parity with sibling test files that format ids).
var _ = fmt.Sprintf

// TestExecWSAcceptsCrossOriginClients asserts an origin-bearing handshake is
// accepted (issue #678). The Python SDK and any browser client send an Origin
// header the backend Host (pod IP:port) can never match, and the library's
// default same-origin check 403ed every such client at upgrade in production.
// Auth here is the bearer token, not cookies, so origin adds no CSRF value.
func TestExecWSAcceptsCrossOriginClients(t *testing.T) {
	_, srv := newPtyAPI(t, "sekret")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, wsExecURL(srv.URL, "sb1"), &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Authorization": {"Bearer sekret"},
			// A public API origin, deliberately mismatching the test server host.
			"Origin": {"https://api.mitos.run"},
		},
		Subprotocols: []string{execWSSubprotocol},
	})
	if err != nil {
		t.Fatalf("dial with cross-origin header: %v (the exec ws must not enforce same-origin; auth is the bearer token)", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	// Prove the session is usable, not merely accepted: open a PTY and see the
	// echo round-trip.
	writeFrame(ctx, t, c, false, &sandboxv1.ExecRequest{
		Msg: &sandboxv1.ExecRequest_Open{Open: &sandboxv1.ExecOpen{
			Pty: &sandboxv1.PtyOptions{Size: &sandboxv1.WindowSize{Cols: 80, Rows: 24}},
		}},
	})
	writeFrame(ctx, t, c, false, &sandboxv1.ExecRequest{
		Msg: &sandboxv1.ExecRequest_Stdin{Stdin: []byte("origin-ok\n")},
	})
	_, resp := readResponse(ctx, t, c)
	if string(resp.GetStdout()) != "origin-ok\n" {
		t.Fatalf("stdout = %q, want %q", resp.GetStdout(), "origin-ok\n")
	}
}
