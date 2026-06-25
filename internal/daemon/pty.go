package daemon

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/coder/websocket"
	"mitos.run/mitos/internal/apierr"
	"mitos.run/mitos/internal/vsock"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// ptySubprotocol is the WebSocket subprotocol the PTY endpoint speaks. Clients
// (the SDKs and browser xterm.js front ends) must offer it.
const ptySubprotocol = "mitos.pty.v1"

// ptyAuth authenticates a PTY WebSocket upgrade. Unlike requireBearer (which
// peeks a JSON request body), the upgrade is a bodyless GET, so the sandbox id
// comes from the ?sandbox= query parameter and the token from the
// Authorization: Bearer header. Semantics match requireBearer exactly:
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
	// the PTY to the single VM. In forkd's default multi-sandbox mode this is the
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

// handlePty upgrades to a WebSocket and bridges it to the guest Exec gRPC PTY
// stream (ExecOpen with Pty set). Client text frames carry input bytes and
// resize events (same vsock.PtyFrame JSON wire shape as before); guest stdout
// chunks and the terminal exit frame are forwarded to the WebSocket. Auth is
// the per-sandbox bearer token (ptyAuth). The concurrent-stream slot is
// acquired via the gRPC Exec call BEFORE the WebSocket upgrade so a cap
// rejection is a clean 429 rather than a post-upgrade close.
func (api *SandboxAPI) handlePty(w http.ResponseWriter, r *http.Request) {
	sandbox, ok := api.ptyAuth(w, r)
	if !ok {
		return
	}
	api.touch(sandbox)

	if err := api.checkSandboxRegistered(sandbox); err != nil {
		writeErr(w, err.Error(), http.StatusNotFound)
		return
	}

	// Parse cols/rows from the query (bounded smallints). Command is
	// intentionally NOT taken from the client: the guest defaults to /bin/sh.
	cols := atoiDefault(r.URL.Query().Get("cols"), 80)
	rows := atoiDefault(r.URL.Query().Get("rows"), 24)

	// Open the guest gRPC PTY Exec stream BEFORE the WebSocket upgrade. This
	// acquires the per-sandbox concurrent-stream slot (cap 3, production-blocker
	// #2) inside vsockGuestConn.Exec, so a cap rejection surfaces as a clean 429
	// envelope rather than a post-upgrade close code. The gRPC context is the
	// request context here; the WebSocket upgrade hijacks the connection and the
	// long-lived bridge runs on the handler goroutine after upgrade using the
	// same context.
	g := newVsockGuestConn(api, sandbox).(*vsockGuestConn)
	execOpen := &sandboxv1.ExecOpen{
		Pty: &sandboxv1.PtyOptions{
			Size: &sandboxv1.WindowSize{
				Cols: uint32(cols),
				Rows: uint32(rows),
			},
		},
	}
	// ExecPTY acquires the per-sandbox concurrent-stream slot (cap 3) and opens
	// the gRPC Exec stream WITHOUT closing the send direction, so the bridge can
	// send stdin and resize frames for the session lifetime.
	grpcPTY, err := g.ExecPTY(r.Context(), execOpen)
	if err != nil {
		// ExecPTY returns a recognisable "concurrent exec-stream limit" message
		// when the per-sandbox slot cap is full; map that to a clean 429.
		if strings.Contains(err.Error(), "concurrent exec-stream limit") {
			writeAPIErr(w, apierr.Get(apierr.CodeTooManyStreams).
				WithCause(fmt.Sprintf("sandbox %s is at its concurrent-stream limit", sandbox)).
				WithContext(map[string]any{"sandbox": sandbox}))
		} else {
			writeErr(w, fmt.Sprintf("pty backend unavailable: %v", err), http.StatusInternalServerError)
		}
		return
	}
	// grpcPTY.Close() releases the stream slot and the gRPC conn; always
	// called on every exit path below (deferred after WebSocket upgrade).

	c, wsErr := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{ptySubprotocol},
	})
	if wsErr != nil {
		// websocket.Accept already wrote the failure status; clean up the
		// gRPC stream (which holds a stream slot) before returning.
		_ = grpcPTY.Close()
		return
	}
	ctx := r.Context()
	defer c.Close(websocket.StatusNormalClosure, "")
	defer grpcPTY.Close()

	go func() {
		for {
			typ, data, rerr := c.Read(ctx)
			if rerr != nil {
				// WebSocket closed or context cancelled; cancel the gRPC stream
				// by closing the gRPC connection so the writer pump below unblocks.
				grpcPTY.cc.Close()
				return
			}
			if typ != websocket.MessageText {
				continue
			}
			var f vsock.PtyFrame
			if jsonErr := json.Unmarshal(data, &f); jsonErr != nil {
				continue
			}
			switch f.Kind {
			case vsock.PtyInput:
				// Forward input bytes as ExecRequest stdin.
				_ = grpcPTY.stream.Send(&sandboxv1.ExecRequest{
					Msg: &sandboxv1.ExecRequest_Stdin{Stdin: f.Data},
				})
			case vsock.PtyResize:
				// Forward resize as ExecRequest resize.
				_ = grpcPTY.stream.Send(&sandboxv1.ExecRequest{
					Msg: &sandboxv1.ExecRequest_Resize{
						Resize: &sandboxv1.WindowSize{
							Cols: uint32(f.Cols),
							Rows: uint32(f.Rows),
						},
					},
				})
			}
		}
	}()

	// Writer pump: guest gRPC output frames -> WebSocket.
	// Uses grpcExecStream.Recv to get properly mapped frames.
	for {
		frame, recvErr := grpcPTY.Recv()
		if recvErr != nil {
			if errors.Is(recvErr, context.Canceled) {
				return
			}
			eb, _ := json.Marshal(vsock.PtyFrame{Kind: vsock.PtyExit, ExitCode: 1, Error: fmt.Sprintf("pty stream failed: %v", recvErr)})
			_ = c.Write(ctx, websocket.MessageText, eb)
			return
		}
		if frame.Done {
			eb, _ := json.Marshal(vsock.PtyFrame{Kind: vsock.PtyExit, ExitCode: int(frame.ExitCode)})
			_ = c.Write(ctx, websocket.MessageText, eb)
			break
		}
		// For a PTY exec, guest merges stdout+stderr on the terminal stream;
		// forward both as output frames.
		if len(frame.Stdout) > 0 {
			fb, mErr := json.Marshal(vsock.PtyFrame{Kind: vsock.PtyOutput, Data: frame.Stdout})
			if mErr != nil {
				return
			}
			if wErr := c.Write(ctx, websocket.MessageText, fb); wErr != nil {
				return
			}
		}
		if len(frame.Stderr) > 0 {
			fb, mErr := json.Marshal(vsock.PtyFrame{Kind: vsock.PtyOutput, Data: frame.Stderr})
			if mErr != nil {
				return
			}
			if wErr := c.Write(ctx, websocket.MessageText, fb); wErr != nil {
				return
			}
		}
	}

	api.auditor.Record(AuditEvent{
		SandboxID: sandbox,
		Op:        "pty",
		Detail:    fmt.Sprintf("cols=%d rows=%d", cols, rows),
		OK:        true,
	})
}

// atoiDefault parses s as a positive int, returning def on any parse failure or
// non-positive value. Used to bound cols/rows from the query string.
func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n := 0
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return def
		}
		n = n*10 + int(ch-'0')
		if n > 100000 { // absurd; clamp to default
			return def
		}
	}
	if n <= 0 {
		return def
	}
	return n
}
