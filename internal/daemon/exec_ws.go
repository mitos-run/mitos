package daemon

import (
	"context"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// ptyAuth authenticates a Connect-over-WebSocket Exec upgrade. Unlike
// requireBearer (which peeks a JSON request body), the upgrade is a bodyless GET,
// so the sandbox id comes from the ?sandbox= query parameter and the token from
// the Authorization: Bearer header. Semantics match requireBearer exactly:
//   - no token registered: 401 (fail closed) unless allowTokenless
//   - missing/malformed Authorization: 401
//   - mismatch: 401 (constant-time compare)
//
// Token values are never logged. Returns the resolved sandbox id on success.
func (api *SandboxAPI) ptyAuth(w http.ResponseWriter, r *http.Request) (string, bool) {
	requested := r.URL.Query().Get("sandbox")
	if requested == "" {
		writeErr(w, "missing sandbox query parameter", http.StatusBadRequest)
		return "", false
	}

	// In single-sandbox mode (husk-stub) the ?sandbox= id is whatever the SDK
	// sent (the husk pod name); resolve it to the one served sandbox id so the
	// token lookup hits the single registered token and the returned id routes
	// the exec to the single VM. In forkd's default multi-sandbox mode this is the
	// request id unchanged, so the per-id gate is byte-identical.
	sandbox := api.resolveSandboxID(requested)

	api.mu.RLock()
	token, hasToken := api.tokens[sandbox]
	api.mu.RUnlock()

	if !hasToken {
		if api.allowTokenless {
			return sandbox, true
		}
		writeErr(w, "unauthorized: no token registered for sandbox", http.StatusUnauthorized)
		return "", false
	}

	presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		writeErr(w, "unauthorized: bearer token required", http.StatusUnauthorized)
		return "", false
	}
	if subtle.ConstantTimeCompare([]byte(presented), []byte(token)) != 1 {
		writeErr(w, "unauthorized: invalid token", http.StatusUnauthorized)
		return "", false
	}
	return sandbox, true
}

// execWSPath is the route the Connect-over-WebSocket bidi Exec endpoint is
// mounted at. It deliberately matches the Connect service's Exec procedure path
// so the schema is the same sandbox.v1.Sandbox.Exec; only the transport differs.
// The Connect HTTP handler owns POST on this path (HTTP/2 bidi); this handler
// owns the GET WebSocket upgrade, which HTTP/1.1 clients (the thin SDKs) use for
// the full-duplex interactive case (PTY) that HTTP/2-only bidi cannot reach.
const execWSPath = "/sandbox.v1.Sandbox/Exec"

// execWSSubprotocol is the WebSocket subprotocol the Connect Exec transport
// speaks. Clients (the SDKs, browser front ends) must offer it on the upgrade.
const execWSSubprotocol = "connect.sandbox.v1"

// connectFlagEndStream is bit 1 of a Connect enveloped frame's flags byte: the
// terminal server frame sets it. It matches the framing the SDK Connect clients
// already implement (sdk/python/mitos/_connect.py, sdk/typescript/src/connect.ts).
const connectFlagEndStream byte = 0x02

// maxExecWSFrame bounds a single inbound WebSocket message. Exec input frames
// are small (keystrokes, a resize, an open); this caps a hostile client's
// per-message allocation.
const maxExecWSFrame = 1 << 20 // 1 MiB

// encodeConnectFrame frames one proto message as a Connect enveloped frame: a 5-byte
// header (1 flags byte, then the big-endian uint32 payload length) followed by
// the application/connect+json payload (protojson). end sets the end-stream flag.
func encodeConnectFrame(end bool, msg proto.Message) ([]byte, error) {
	payload, err := protojson.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshal connect frame: %w", err)
	}
	out := make([]byte, 5+len(payload))
	if end {
		out[0] = connectFlagEndStream
	}
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out, nil
}

// decodeConnectFrame splits one Connect enveloped frame into its flags byte and the
// payload bytes. Each WebSocket binary message carries exactly one frame.
func decodeConnectFrame(b []byte) (byte, []byte, error) {
	if len(b) < 5 {
		return 0, nil, fmt.Errorf("short connect frame: %d bytes", len(b))
	}
	flags := b[0]
	n := binary.BigEndian.Uint32(b[1:5])
	if int(n) != len(b)-5 {
		return 0, nil, fmt.Errorf("connect frame length %d does not match payload %d", n, len(b)-5)
	}
	return flags, b[5:], nil
}

// handleExecWS serves the bidi sandbox.v1.Sandbox.Exec RPC over a WebSocket so
// the thin half-duplex-over-HTTP/1.1 SDK clients can reach the full-duplex
// interactive Exec (PTY) that Connect's HTTP/2-only bidi cannot serve them. The
// transport carries Connect enveloped frames: the client sends ExecRequest
// frames (the first MUST be the open oneof, with pty set for a terminal; later
// frames carry stdin or resize), the server sends ExecResponse frames (stdout or
// stderr chunks, then a terminal exit frame with the end-stream flag).
//
// SECURITY: auth is the per-sandbox bearer token, enforced BEFORE the upgrade
// (ptyAuth), so a bad token is a clean 401 handshake, not a post-upgrade close.
// No command is taken from the client: as with the legacy PTY path, the open's
// argv is ignored for the interactive case and the guest defaults to /bin/sh.
// Token values are never logged.
func (api *SandboxAPI) handleExecWS(w http.ResponseWriter, r *http.Request) {
	sandbox, ok := api.ptyAuth(w, r)
	if !ok {
		return
	}
	api.touch(sandbox)

	if err := api.checkSandboxRegistered(sandbox); err != nil {
		writeErr(w, err.Error(), http.StatusNotFound)
		return
	}

	c, wsErr := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{execWSSubprotocol},
	})
	if wsErr != nil {
		// websocket.Accept already wrote the failure status.
		return
	}
	c.SetReadLimit(maxExecWSFrame)
	ctx := r.Context()
	defer c.Close(websocket.StatusNormalClosure, "")

	// The first frame MUST carry the open oneof; it defines the exec (pty, env,
	// cwd). Read it before acquiring the guest stream so the ExecOpen is the one
	// the client asked for.
	open, err := readExecOpen(ctx, c)
	if err != nil {
		closeWithExecError(ctx, c, websocket.StatusUnsupportedData, fmt.Sprintf("first frame must be an exec open: %v", err))
		return
	}

	g, ok := newVsockGuestConn(api, sandbox).(*vsockGuestConn)
	if !ok {
		closeWithExecError(ctx, c, websocket.StatusInternalError, "guest connection unavailable")
		return
	}
	// ExecPTY acquires the per-sandbox concurrent-stream slot (cap) and opens the
	// guest Exec stream WITHOUT closing the send direction, so stdin and resize
	// flow for the session lifetime. A cap rejection is a typed error frame:
	// post-upgrade we can no longer return a 429, so the client learns of the cap
	// via the terminal exit frame and a policy-violation close.
	grpcExec, err := g.ExecPTY(ctx, open)
	if err != nil {
		code := websocket.StatusInternalError
		if isStreamCapError(err) {
			code = websocket.StatusPolicyViolation
		}
		closeWithExecError(ctx, c, code, fmt.Sprintf("exec backend unavailable: %v", err))
		return
	}
	defer grpcExec.Close()

	// Read pump: client ExecRequest frames (stdin, resize) -> guest stream.
	go func() {
		for {
			req, rerr := readExecRequest(ctx, c)
			if rerr != nil {
				// WebSocket closed or a bad frame: cancel the guest stream by closing
				// its conn so the writer pump below unblocks and the handler returns.
				grpcExec.cc.Close()
				return
			}
			switch m := req.Msg.(type) {
			case *sandboxv1.ExecRequest_Stdin:
				_ = grpcExec.stream.Send(&sandboxv1.ExecRequest{Msg: &sandboxv1.ExecRequest_Stdin{Stdin: m.Stdin}})
			case *sandboxv1.ExecRequest_Resize:
				_ = grpcExec.stream.Send(&sandboxv1.ExecRequest{Msg: &sandboxv1.ExecRequest_Resize{Resize: m.Resize}})
			default:
				// Ignore further open frames or unknown oneofs; the open was consumed.
			}
		}
	}()

	// Writer pump: guest output frames -> client ExecResponse frames.
	for {
		frame, recvErr := grpcExec.Recv()
		if recvErr != nil {
			if errors.Is(recvErr, context.Canceled) {
				return
			}
			closeWithExecError(ctx, c, websocket.StatusInternalError, fmt.Sprintf("exec stream failed: %v", recvErr))
			return
		}
		if frame.Done {
			eb, mErr := encodeConnectFrame(true, &sandboxv1.ExecResponse{
				Msg: &sandboxv1.ExecResponse_Exit{Exit: &sandboxv1.ExecExit{ExitCode: frame.ExitCode, ExecTimeMs: frame.ExecTimeMs}},
			})
			if mErr == nil {
				_ = c.Write(ctx, websocket.MessageBinary, eb)
			}
			break
		}
		if len(frame.Stdout) > 0 {
			if !writeExecResponse(ctx, c, &sandboxv1.ExecResponse{Msg: &sandboxv1.ExecResponse_Stdout{Stdout: frame.Stdout}}) {
				return
			}
		}
		if len(frame.Stderr) > 0 {
			if !writeExecResponse(ctx, c, &sandboxv1.ExecResponse{Msg: &sandboxv1.ExecResponse_Stderr{Stderr: frame.Stderr}}) {
				return
			}
		}
	}

	api.auditor.Record(AuditEvent{
		SandboxID: sandbox,
		Op:        "exec_ws",
		Detail:    "pty=" + boolStr(open.GetPty() != nil),
		OK:        true,
	})
}

// readExecRequest reads one WebSocket binary message and decodes it as an
// enveloped ExecRequest.
func readExecRequest(ctx context.Context, c *websocket.Conn) (*sandboxv1.ExecRequest, error) {
	typ, data, err := c.Read(ctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageBinary {
		return nil, fmt.Errorf("expected a binary frame, got %v", typ)
	}
	_, payload, err := decodeConnectFrame(data)
	if err != nil {
		return nil, err
	}
	var req sandboxv1.ExecRequest
	if err := protojson.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("decode ExecRequest: %w", err)
	}
	return &req, nil
}

// readExecOpen reads the first ExecRequest and requires it to carry the open
// oneof, returning the ExecOpen.
func readExecOpen(ctx context.Context, c *websocket.Conn) (*sandboxv1.ExecOpen, error) {
	req, err := readExecRequest(ctx, c)
	if err != nil {
		return nil, err
	}
	open := req.GetOpen()
	if open == nil {
		return nil, fmt.Errorf("first frame carried no open oneof")
	}
	return open, nil
}

// writeExecResponse frames and writes one non-terminal ExecResponse; it returns
// false if the write fails so the caller can stop the pump.
func writeExecResponse(ctx context.Context, c *websocket.Conn, resp *sandboxv1.ExecResponse) bool {
	eb, err := encodeConnectFrame(false, resp)
	if err != nil {
		return false
	}
	return c.Write(ctx, websocket.MessageBinary, eb) == nil
}

// closeWithExecError sends a terminal ExecResponse error frame (end-stream) then
// closes the WebSocket with code and reason. The reason carries no token value.
func closeWithExecError(ctx context.Context, c *websocket.Conn, code websocket.StatusCode, reason string) {
	eb, err := encodeConnectFrame(true, &sandboxv1.ExecResponse{
		Msg: &sandboxv1.ExecResponse_Exit{Exit: &sandboxv1.ExecExit{ExitCode: 1, Error: reason}},
	})
	if err == nil {
		_ = c.Write(ctx, websocket.MessageBinary, eb)
	}
	_ = c.Close(code, truncateReason(reason))
}

// isStreamCapError reports whether err is the per-sandbox concurrent-stream cap
// rejection from ExecPTY (matched on its stable message).
func isStreamCapError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "concurrent exec-stream limit")
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// truncateReason bounds a WebSocket close reason to the protocol's 123-byte limit.
func truncateReason(s string) string {
	if len(s) > 123 {
		return s[:123]
	}
	return s
}
