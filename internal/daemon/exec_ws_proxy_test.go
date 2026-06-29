package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// TestExecWSPtyThroughReverseProxy puts an httputil.ReverseProxy (the same
// upgrade-carrying proxy the hosted gateway uses for the PTY ws, #532) in FRONT
// of the REAL exec_ws handler and drives the bidi PTY through it. This reproduces
// the hosted path locally (minus the real guest VM) to isolate #535: if the PTY
// round-trips here, the gateway-to-exec_ws seam is sound and the live gap is the
// real guest/husk-stub PTY; if it hangs here, the proxy-to-handler seam itself is
// the bug.
// reverseProxyFront builds an httptest server running the gateway's PTY ws proxy
// mechanism (an httputil.ReverseProxy whose Director rewrites to the backend with
// the per-sandbox token on ?sandbox=<dialID>), mirroring internal/saas
// proxyRuntimeWebSocket without importing the saas package.
func reverseProxyFront(t *testing.T, backendURL, dialID, token string) *httptest.Server {
	t.Helper()
	backendHost := strings.TrimPrefix(backendURL, "http://")
	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = backendHost
			req.URL.Path = execWSPath
			req.URL.RawQuery = url.Values{"sandbox": {dialID}}.Encode()
			req.Host = backendHost
			// Mirror the gateway: overwrite the customer credential with the
			// per-sandbox token, never forward the client Authorization.
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("X-Sandbox-Id", dialID)
		},
	}
	front := httptest.NewServer(rp)
	t.Cleanup(front.Close)
	return front
}

func TestExecWSPtyThroughReverseProxy(t *testing.T) {
	_, backend := newPtyAPI(t, "sekret")
	front := reverseProxyFront(t, backend.URL, "sb1", "sekret")

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, wsExecURL(front.URL, "sb1"), &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": {"Bearer customer-key"}},
		Subprotocols: []string{execWSSubprotocol},
	})
	if err != nil {
		t.Fatalf("dial through proxy: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	// Open a PTY exec, then send stdin; the fake guest echoes it as stdout.
	writeFrame(ctx, t, c, false, &sandboxv1.ExecRequest{
		Msg: &sandboxv1.ExecRequest_Open{Open: &sandboxv1.ExecOpen{
			Pty: &sandboxv1.PtyOptions{Size: &sandboxv1.WindowSize{Cols: 80, Rows: 24}},
		}},
	})
	writeFrame(ctx, t, c, false, &sandboxv1.ExecRequest{
		Msg: &sandboxv1.ExecRequest_Stdin{Stdin: []byte("hello-pty\n")},
	})

	readCtx, readCancel := context.WithTimeout(ctx, 4*time.Second)
	defer readCancel()
	_, resp := readResponse(readCtx, t, c)
	if string(resp.GetStdout()) != "hello-pty\n" {
		t.Fatalf("stdout through proxy = %q, want %q (the bidi PTY did not round-trip through the ReverseProxy)", resp.GetStdout(), "hello-pty\n")
	}

	// Drive it to exit to prove the terminal frame also crosses the proxy.
	writeFrame(ctx, t, c, false, &sandboxv1.ExecRequest{
		Msg: &sandboxv1.ExecRequest_Stdin{Stdin: []byte("exit\n")},
	})
	flags, ex := readResponse(readCtx, t, c)
	if ex.GetExit() == nil {
		t.Fatalf("final frame = %+v, want exit", ex)
	}
	if flags&connectFlagEndStream == 0 {
		t.Fatalf("exit frame missing end-stream flag (flags=0x%02x)", flags)
	}
}

// TestExecWSPtySingleSandboxThroughReverseProxy reproduces the WARM-HUSK path: the
// SandboxAPI runs in single-sandbox mode (as cmd/husk-stub does), and the proxy
// addresses it with a DIFFERENT id (the claim/pod name the SDK uses), which
// resolveSandboxID maps to the one local sandbox. This isolates whether the
// husk-stub single-sandbox bridging (not just multi-sandbox forkd) carries the
// bidi PTY data through the gateway's proxy. If this passes, the remaining #535
// suspect is the REAL guest-agent PTY over vsock in the live VM, not the gateway,
// the proxy seam, or the husk-stub mode.
func TestExecWSPtySingleSandboxThroughReverseProxy(t *testing.T) {
	api, backend := newPtyAPI(t, "sekret")
	// Emulate the husk-stub: one served sandbox, single-sandbox mode. The token is
	// registered under "sb1" (the stub's local id); the SDK/gateway will address a
	// different claim id.
	api.SetSingleSandbox("sb1")

	// The proxy dials with a claim/pod name that is NOT the registered local id,
	// exactly as the SDK addresses a husk pod; resolveSandboxID folds it to "sb1".
	front := reverseProxyFront(t, backend.URL, "husk-pod-claim-xyz", "sekret")

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, wsExecURL(front.URL, "husk-pod-claim-xyz"), &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": {"Bearer customer-key"}},
		Subprotocols: []string{execWSSubprotocol},
	})
	if err != nil {
		t.Fatalf("dial through proxy (single-sandbox): %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	writeFrame(ctx, t, c, false, &sandboxv1.ExecRequest{
		Msg: &sandboxv1.ExecRequest_Open{Open: &sandboxv1.ExecOpen{
			Pty: &sandboxv1.PtyOptions{Size: &sandboxv1.WindowSize{Cols: 80, Rows: 24}},
		}},
	})
	writeFrame(ctx, t, c, false, &sandboxv1.ExecRequest{
		Msg: &sandboxv1.ExecRequest_Stdin{Stdin: []byte("hello-husk\n")},
	})

	readCtx, readCancel := context.WithTimeout(ctx, 4*time.Second)
	defer readCancel()
	_, resp := readResponse(readCtx, t, c)
	if string(resp.GetStdout()) != "hello-husk\n" {
		t.Fatalf("stdout through single-sandbox proxy = %q, want %q", resp.GetStdout(), "hello-husk\n")
	}
}
