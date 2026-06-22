package console

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// spyRawLogStreamer records whether the transport was ever reached. It is the
// stand-in for the real forkd→guest vsock transport so a test can prove the BFF
// authorizes BEFORE it streams.
type spyRawLogStreamer struct {
	lines  map[string][]string // sandboxID -> lines
	called bool
}

func (s *spyRawLogStreamer) StreamRaw(_ context.Context, sandboxID string, sink LogSink) error {
	s.called = true
	for _, ln := range s.lines[sandboxID] {
		if err := sink.Write([]byte(ln)); err != nil {
			return err
		}
	}
	return nil
}

// TestAuthorizingLogStreamerRefusesCrossOrgBeforeTransport is the load-bearing
// isolation test: a cross-org sandbox id must be refused as ErrNotFound and the
// raw transport must NEVER be reached, so authorization cannot be bypassed by
// the streaming path.
func TestAuthorizingLogStreamerRefusesCrossOrgBeforeTransport(t *testing.T) {
	control := NewMemSandboxControl()
	control.Add(SandboxView{ID: "sb-1", OrgID: "org-alice"})
	raw := &spyRawLogStreamer{lines: map[string][]string{"sb-1": {"secret-line"}}}
	ls := NewAuthorizingLogStreamer(control, raw)

	sink := &captureSink{}
	err := ls.StreamLogs(context.Background(), "org-bob", "sb-1", sink)
	if err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if raw.called {
		t.Fatal("raw transport was reached for a cross-org sandbox; authz bypassed")
	}
	if len(sink.lines) != 0 {
		t.Fatalf("lines leaked across org: %v", sink.lines)
	}
}

// TestAuthorizingLogStreamerStreamsOwnedSandbox asserts the owning org's stream
// reaches the transport and delivers the lines.
func TestAuthorizingLogStreamerStreamsOwnedSandbox(t *testing.T) {
	control := NewMemSandboxControl()
	control.Add(SandboxView{ID: "sb-1", OrgID: "org-alice"})
	raw := &spyRawLogStreamer{lines: map[string][]string{"sb-1": {"hello\n", "world\n"}}}
	ls := NewAuthorizingLogStreamer(control, raw)

	sink := &captureSink{}
	if err := ls.StreamLogs(context.Background(), "org-alice", "sb-1", sink); err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	if !raw.called {
		t.Fatal("raw transport was not reached for the owning org")
	}
	if got := strings.Join(sink.linesAsStrings(), ""); got != "hello\nworld\n" {
		t.Fatalf("streamed %q, want hello\\nworld\\n", got)
	}
}

// TestSandboxLogsEndpointCrossOrgIsNotFound asserts the HTTP route refuses a
// cross-org sandbox id with 404 and leaks no log content.
func TestSandboxLogsEndpointCrossOrgIsNotFound(t *testing.T) {
	f := newFixture(t)
	// sb-alice-1 is seeded for alice in the fixture; bob must not read its logs.
	w := f.req(t, "GET", "/console/sandboxes/sb-alice-1/logs", "", f.bobAcct, f.bobOrg)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "alice-log") {
		t.Fatalf("cross-org request leaked alice's log content: %s", w.Body.String())
	}
}

// TestSandboxLogsEndpointStreamsOwner asserts the owning org streams its logs
// over the HTTP route.
func TestSandboxLogsEndpointStreamsOwner(t *testing.T) {
	f := newFixture(t)
	w := f.req(t, "GET", "/console/sandboxes/sb-alice-1/logs", "", f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "alice-log-line") {
		t.Fatalf("owner stream missing seeded line: %s", w.Body.String())
	}
}

type captureSink struct{ lines [][]byte }

func (c *captureSink) Write(line []byte) error {
	b := make([]byte, len(line))
	copy(b, line)
	c.lines = append(c.lines, b)
	return nil
}

func (c *captureSink) linesAsStrings() []string {
	out := make([]string, len(c.lines))
	for i, b := range c.lines {
		out[i] = string(b)
	}
	return out
}
