package daemon

// Single-sandbox-mode auth tests.
//
// The husk-stub serves exactly ONE sandbox per pod. Its OnActivated hook
// registers the activated VM and its per-sandbox bearer token under a fixed
// local id, but the SDK addresses the in-pod API with the claim's
// status.sandboxID (the husk pod name) in the request body's "sandbox" field,
// which never equals that fixed local id. In forkd's per-id token lookup that
// mismatch is a 401 "no token registered for sandbox".
//
// SetSingleSandbox(id) is the explicit opt-in that fixes this: in single-
// sandbox mode requireBearer and ptyAuth validate the presented bearer against
// the ONE registered token regardless of the request's "sandbox" id, then route
// the request to that single sandbox. forkd never sets it, so forkd's
// multi-sandbox per-id gate is unchanged: a token for sandbox A still must not
// authorize sandbox B.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// newSingleSandboxAPI builds a SandboxAPI in single-sandbox mode with a
// connected gRPC fake agent and a registered token under the LOCAL id, then
// returns the API plus an httptest server over its Handler. This mirrors the
// husk-stub: the sandbox is known locally as localID, but the SDK will send a
// different id.
func newSingleSandboxAPI(t *testing.T, localID, token string) (*SandboxAPI, *httptest.Server) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "single")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	sockPath := filepath.Join(dir, "vsock.sock")
	fake := &fakeGuestSandbox{execStdout: "hi\n", execExit: 0}
	startFakeGuestGRPCUDS(t, sockPath, fake)

	api := NewSandboxAPI(dir)
	if err := api.RegisterSandbox(localID, sockPath); err != nil {
		t.Fatal(err)
	}
	api.RegisterStreamPath(localID, sockPath)
	api.RegisterToken(localID, token)
	api.SetSingleSandbox(localID)

	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)
	return api, ts
}

func TestSingleSandboxAcceptsArbitrarySandboxIDWithCorrectToken(t *testing.T) {
	const localID = "husk"
	const podID = "mitos-py-husk-5gwmh" // the id the SDK actually sends in cluster mode
	const token = "tok-correct"

	_, ts := newSingleSandboxAPI(t, localID, token)

	// The SDK sends the pod name, NOT the local id, but the correct token
	// authorizes and the request routes to the single sandbox's agent.
	resp, body := postExec(t, ts.URL, podID, token)
	if resp.StatusCode != 200 {
		t.Fatalf("single-sandbox exec with pod id + correct token: status = %d, body = %s, want 200", resp.StatusCode, body)
	}
	if !strings.Contains(body, "hi") {
		t.Fatalf("exec did not reach the agent: %s", body)
	}
}

func TestSingleSandboxRejectsWrongOrAbsentToken(t *testing.T) {
	const localID = "husk"
	const podID = "mitos-py-husk-5gwmh"
	const token = "tok-correct"

	_, ts := newSingleSandboxAPI(t, localID, token)

	resp, _ := postExec(t, ts.URL, podID, "")
	if resp.StatusCode != 401 {
		t.Fatalf("single-sandbox exec, no token: status = %d, want 401", resp.StatusCode)
	}
	resp, _ = postExec(t, ts.URL, podID, "tok-wrong")
	if resp.StatusCode != 401 {
		t.Fatalf("single-sandbox exec, wrong token: status = %d, want 401", resp.StatusCode)
	}
}

// Fail-closed: single-sandbox mode with NO registered token rejects every
// request, even with a bearer; AllowTokenless is the standalone-server escape
// hatch and is not set here.
func TestSingleSandboxNoTokenFailsClosed(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "single")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "vsock.sock")
	fake := &fakeGuestSandbox{}
	startFakeGuestGRPCUDS(t, sockPath, fake)

	api := NewSandboxAPI(dir)
	if err := api.RegisterSandbox("husk", sockPath); err != nil {
		t.Fatal(err)
	}
	api.SetSingleSandbox("husk") // no token registered
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)

	for _, bearer := range []string{"", "guess"} {
		resp, _ := postExec(t, ts.URL, "anything", bearer)
		if resp.StatusCode != 401 {
			t.Fatalf("single-sandbox, no token, bearer %q: status = %d, want 401", bearer, resp.StatusCode)
		}
	}
}

// Guard: forkd's multi-sandbox gate is unchanged. With SetSingleSandbox NOT
// called, sandbox A's token must not authorize sandbox B, and the request id
// must match the registered id exactly.
func TestMultiSandboxModeStillRequiresExactIDMatch(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "multi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	sockA := filepath.Join(dir, "a.sock")
	sockB := filepath.Join(dir, "b.sock")
	fakeA := &fakeGuestSandbox{execStdout: "hi-a\n", execExit: 0}
	fakeB := &fakeGuestSandbox{execStdout: "hi-b\n", execExit: 0}
	startFakeGuestGRPCUDS(t, sockA, fakeA)
	startFakeGuestGRPCUDS(t, sockB, fakeB)

	api := NewSandboxAPI(dir)
	if err := api.RegisterSandbox("sb-a", sockA); err != nil {
		t.Fatal(err)
	}
	if err := api.RegisterSandbox("sb-b", sockB); err != nil {
		t.Fatal(err)
	}
	api.RegisterStreamPath("sb-a", sockA)
	api.RegisterStreamPath("sb-b", sockB)
	api.RegisterToken("sb-a", "tok-a")
	api.RegisterToken("sb-b", "tok-b")
	// Multi-sandbox: SetSingleSandbox is NOT called.

	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)

	// A's token opens A.
	resp, _ := postExec(t, ts.URL, "sb-a", "tok-a")
	if resp.StatusCode != 200 {
		t.Fatalf("sb-a with tok-a: status = %d, want 200", resp.StatusCode)
	}
	// A's token does NOT open B.
	resp, _ = postExec(t, ts.URL, "sb-b", "tok-a")
	if resp.StatusCode != 401 {
		t.Fatalf("cross-sandbox token (tok-a against sb-b): status = %d, want 401", resp.StatusCode)
	}
	// An unknown id (e.g. a pod name) is rejected even with a valid token.
	resp, _ = postExec(t, ts.URL, "mitos-py-husk-5gwmh", "tok-a")
	if resp.StatusCode != 401 {
		t.Fatalf("multi-sandbox unknown id with tok-a: status = %d, want 401", resp.StatusCode)
	}
}

// In single-sandbox mode the interactive Exec upgrade (Connect over WebSocket)
// authenticates against the single registered token regardless of the ?sandbox=
// id the SDK passes (the pod name), and a wrong token is rejected. This proves
// ptyAuth, the gate the ws Exec endpoint shares, got the same single-sandbox fix
// as requireBearer. The legacy /v1/pty JSON wire was removed in #358; this is its
// successor coverage.
func TestSingleSandboxPtyAuthIgnoresRequestID(t *testing.T) {
	const localID = "husk"
	const podID = "mitos-py-husk-5gwmh"
	const token = "tok-correct"

	dir, err := os.MkdirTemp("/tmp", "singlepty")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "vsock.sock")
	startFakePtyGRPCUDS(t, sockPath)

	api := NewSandboxAPI(dir)
	if err := api.RegisterSandbox(localID, sockPath); err != nil {
		t.Fatal(err)
	}
	api.RegisterStreamPath(localID, sockPath)
	api.RegisterToken(localID, token)
	api.SetSingleSandbox(localID)

	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Wrong token: the upgrade is rejected (handshake fails with non-101).
	_, resp, err := websocket.Dial(ctx, wsExecURL(ts.URL, podID),
		&websocket.DialOptions{
			Subprotocols: []string{execWSSubprotocol},
			HTTPHeader:   http.Header{"Authorization": {"Bearer tok-wrong"}},
		})
	if err == nil {
		t.Fatal("single-sandbox exec ws with wrong token: handshake succeeded, want rejection")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v, want 401", resp)
	}

	// Correct token + pod id: the upgrade succeeds and we can drive the terminal.
	c, _, err := websocket.Dial(ctx, wsExecURL(ts.URL, podID),
		&websocket.DialOptions{
			Subprotocols: []string{execWSSubprotocol},
			HTTPHeader:   http.Header{"Authorization": {"Bearer " + token}},
		})
	if err != nil {
		t.Fatalf("single-sandbox exec ws with correct token + pod id: dial failed: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	writeFrame(ctx, t, c, false, &sandboxv1.ExecRequest{
		Msg: &sandboxv1.ExecRequest_Open{Open: &sandboxv1.ExecOpen{
			Pty: &sandboxv1.PtyOptions{Size: &sandboxv1.WindowSize{Cols: 80, Rows: 24}},
		}},
	})
	writeFrame(ctx, t, c, false, &sandboxv1.ExecRequest{
		Msg: &sandboxv1.ExecRequest_Stdin{Stdin: []byte("exit\n")},
	})
	_, ex := readResponse(ctx, t, c)
	if ex.GetExit() == nil {
		t.Fatalf("want exit frame, got %+v", ex)
	}
}
