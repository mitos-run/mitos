package mcp

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"
	"testing"

	"mitos.run/mitos/internal/atr"
)

// atrTestConfig builds an ATRConfig from a handful of inline rules plus a buffer
// that captures the report-mode log lines.
func atrTestConfig(t *testing.T, maxBytes int, rules ...atr.Rule) (*ATRConfig, *bytes.Buffer) {
	t.Helper()
	ev, err := atr.NewEvaluator(rules)
	if err != nil {
		t.Fatalf("NewEvaluator: %v", err)
	}
	var buf bytes.Buffer
	return &ATRConfig{
		Evaluator:    ev,
		ScanMaxBytes: maxBytes,
		Logger:       log.New(&buf, "", 0),
	}, &buf
}

func execRule(id, pattern string) atr.Rule {
	return atr.Rule{
		ID: id, Title: id, Severity: "critical", Category: "test", ScanTarget: "mcp",
		Condition: "any", Conditions: []atr.Condition{{Field: "content", Operator: atr.OpRegex, Value: pattern}},
	}
}

// callTool drives one tools/call through the server and returns the decoded
// tool result.
func callTool(t *testing.T, s *Server, name string, args map[string]any) toolResult {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"name": name, "arguments": args})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp := callServer(t, s, "tools/call", json.RawMessage(raw))
	var tr toolResult
	if err := json.Unmarshal(resp.Result, &tr); err != nil {
		t.Fatalf("decode tool result: %v", err)
	}
	return tr
}

func TestATRScreenLogsDetectionInReportMode(t *testing.T) {
	cfg, buf := atrTestConfig(t, 0, execRule("ATR-TEST-0001", `(?i)rm\s+-rf\s+/`))
	s := New(NewFakeBackend(), Options{ATR: cfg})

	tr := callTool(t, s, ToolSandboxExec, map[string]any{"sandbox": "sbx-1", "command": "sudo rm -rf /"})

	// Report mode never turns a clean call into an error.
	if tr.IsError {
		t.Fatalf("report mode must not fail the tool call: %+v", tr)
	}
	line := buf.String()
	if !strings.Contains(line, "atr report-mode detection") || !strings.Contains(line, "ATR-TEST-0001") {
		t.Fatalf("expected a detection log line, got: %q", line)
	}
	// The screened payload must never be logged.
	if strings.Contains(line, "rm -rf") {
		t.Fatalf("payload leaked into the detection log: %q", line)
	}
}

func TestATRNoDetectionNoLog(t *testing.T) {
	cfg, buf := atrTestConfig(t, 0, execRule("ATR-TEST-0002", `(?i)rm\s+-rf\s+/`))
	s := New(NewFakeBackend(), Options{ATR: cfg})

	callTool(t, s, ToolSandboxExec, map[string]any{"sandbox": "sbx-1", "command": "ls -la"})

	if buf.Len() != 0 {
		t.Fatalf("a clean command must not log a detection, got: %q", buf.String())
	}
}

func TestATRDisabledByDefault(t *testing.T) {
	// No ATR config: dispatch runs unchanged and the FakeBackend still executes.
	s := New(NewFakeBackend(), Options{})
	tr := callTool(t, s, ToolSandboxExec, map[string]any{"sandbox": "sbx-1", "command": "rm -rf /"})
	if tr.IsError {
		t.Fatalf("dispatch must work with ATR off: %+v", tr)
	}
}

func TestATROnlyScreensExecAndWriteFile(t *testing.T) {
	// A rule that matches any content; only exec and write_file build an event,
	// so create/fork/terminate must never log even if their ids match.
	cfg, buf := atrTestConfig(t, 0, execRule("ATR-TEST-0003", `sbx`))
	s := New(NewFakeBackend(), Options{ATR: cfg})

	callTool(t, s, ToolSandboxTerminate, map[string]any{"sandbox": "sbx-match"})
	if buf.Len() != 0 {
		t.Fatalf("terminate must not be screened, got: %q", buf.String())
	}

	callTool(t, s, ToolSandboxWriteFile, map[string]any{"sandbox": "sbx-1", "path": "/x", "content": "sbx payload"})
	if !strings.Contains(buf.String(), "ATR-TEST-0003") {
		t.Fatalf("write_file should be screened, got: %q", buf.String())
	}
}

func TestATRTruncationIsObservable(t *testing.T) {
	// Cap below the payload length: the match still fires near the head and the
	// detection is logged as truncated.
	cfg, buf := atrTestConfig(t, 8, execRule("ATR-TEST-0004", `(?i)danger`))
	s := New(NewFakeBackend(), Options{ATR: cfg})

	callTool(t, s, ToolSandboxExec, map[string]any{"sandbox": "sbx-1", "command": "danger" + strings.Repeat(" pad", 100)})
	if !strings.Contains(buf.String(), "truncated=true") {
		t.Fatalf("a capped scan must log truncated=true, got: %q", buf.String())
	}
}
