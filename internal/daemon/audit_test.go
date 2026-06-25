package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// recordingAuditor captures every AuditEvent for assertions.
type recordingAuditor struct {
	mu     sync.Mutex
	events []AuditEvent
}

func (r *recordingAuditor) Record(ev AuditEvent) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
}

func (r *recordingAuditor) snapshot() []AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]AuditEvent, len(r.events))
	copy(out, r.events)
	return out
}

// TestAuditRecordsInteractiveExec drives an interactive Exec over the Connect
// WebSocket endpoint (the runtime path that still audits after the legacy /v1
// JSON exec/file routes were removed in #358) and asserts it records one audit
// event for the operation, attributed to the right sandbox, marked OK.
//
// SECRET HYGIENE: the audit Detail for the interactive exec is a non-content
// marker (pty=true), never command output or any secret. This test also asserts
// no event Detail carries the stdin payload.
func TestAuditRecordsInteractiveExec(t *testing.T) {
	rec := &recordingAuditor{}
	api, srv := newPtyAPI(t, "sekret")
	api.SetAuditor(rec)

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

	writeFrame(ctx, t, c, false, &sandboxv1.ExecRequest{
		Msg: &sandboxv1.ExecRequest_Open{Open: &sandboxv1.ExecOpen{
			Pty: &sandboxv1.PtyOptions{Size: &sandboxv1.WindowSize{Cols: 80, Rows: 24}},
		}},
	})
	const secretStdin = "do-not-audit-this-keystroke\n"
	writeFrame(ctx, t, c, false, &sandboxv1.ExecRequest{
		Msg: &sandboxv1.ExecRequest_Stdin{Stdin: []byte(secretStdin)},
	})
	// Read the echoed stdout, then send exit and read the terminal frame so the
	// handler reaches its audit Record call.
	readResponse(ctx, t, c)
	writeFrame(ctx, t, c, false, &sandboxv1.ExecRequest{
		Msg: &sandboxv1.ExecRequest_Stdin{Stdin: []byte("exit\n")},
	})
	readResponse(ctx, t, c)

	// The audit Record runs after the writer pump ends; allow the handler to
	// finish.
	deadline := time.Now().Add(2 * time.Second)
	var events []AuditEvent
	for time.Now().Before(deadline) {
		events = rec.snapshot()
		if len(events) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(events) == 0 {
		t.Fatal("no audit event recorded for the interactive exec")
	}
	ev := events[0]
	if ev.Op != "exec_ws" {
		t.Errorf("Op = %q, want exec_ws", ev.Op)
	}
	if ev.SandboxID != "sb1" {
		t.Errorf("SandboxID = %q, want sb1", ev.SandboxID)
	}
	if !ev.OK {
		t.Errorf("event not OK: %+v", ev)
	}
	for _, e := range events {
		if strings.Contains(e.Detail, "do-not-audit") {
			t.Fatalf("audit Detail leaked stdin keystrokes: %q", e.Detail)
		}
	}
}

// TestJSONAuditorWritesOneLinePerEvent checks JSON-line framing and the clock.
func TestJSONAuditorWritesOneLinePerEvent(t *testing.T) {
	var buf strings.Builder
	aud := NewJSONAuditor(&buf)
	fixed := time.Unix(1_700_000_000, 0)
	aud.now = func() time.Time { return fixed }

	aud.Record(AuditEvent{SandboxID: "sb", Op: "exec_ws", Detail: "pty=true", OK: true})
	aud.Record(AuditEvent{SandboxID: "sb", Op: "set_timeout", Detail: "600s", OK: true})

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), buf.String())
	}
	var ev AuditEvent
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Unix != fixed.Unix() {
		t.Errorf("Unix = %d, want %d", ev.Unix, fixed.Unix())
	}
	if ev.Op != "exec_ws" {
		t.Errorf("Op = %q", ev.Op)
	}
}
