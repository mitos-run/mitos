package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// AuditEvent is one structured record of an exec or file operation served by
// the SandboxAPI. It carries only SAFE summaries: command strings (commands are
// not secret values) and file paths plus byte counts. It NEVER carries file
// content, env values, secret values, or bearer tokens.
type AuditEvent struct {
	SandboxID string `json:"sandbox_id"`
	Op        string `json:"op"`
	// Detail is a safe human summary. For exec it is the command, truncated to
	// auditDetailMax with an explicit truncation note. For file ops it is the
	// path. It never contains file content or secret values.
	Detail string `json:"detail,omitempty"`
	// Bytes is the size of the file content read or written, in bytes. It is the
	// COUNT only; the content itself is never recorded.
	Bytes int `json:"bytes,omitempty"`
	// Unix is the event time in Unix seconds, stamped by the auditor.
	Unix int64 `json:"unix"`
	// OK reports whether the handler served the operation without error. For
	// exec, a non-zero exit code is still OK=true (the call succeeded); the exit
	// code is reported in Detail.
	OK bool `json:"ok"`
}

// Auditor records audit events emitted by the SandboxAPI handlers.
type Auditor interface {
	Record(ev AuditEvent)
}

// NopAuditor discards every event. It is the default so auditing is off until a
// real auditor is wired in (via --audit-log).
type NopAuditor struct{}

// Record discards the event.
func (NopAuditor) Record(AuditEvent) {}

// auditDetailMax bounds the command string recorded in an exec event's Detail.
const auditDetailMax = 256

// truncateCommand returns cmd truncated to auditDetailMax runes with an explicit
// truncation note appended when it was cut. Commands are not secret values, so
// the command itself is safe to record; the bound only keeps records small.
func truncateCommand(cmd string) string {
	r := []rune(cmd)
	if len(r) <= auditDetailMax {
		return cmd
	}
	return string(r[:auditDetailMax]) + " ...(truncated)"
}

// JSONAuditor writes one JSON-encoded AuditEvent per line to w. It is safe for
// concurrent use by multiple handlers (the write is mutex-guarded).
type JSONAuditor struct {
	mu  sync.Mutex
	w   io.Writer
	now func() time.Time
}

// NewJSONAuditor returns a JSONAuditor writing to w. The clock defaults to
// time.Now; tests override now for determinism.
func NewJSONAuditor(w io.Writer) *JSONAuditor {
	return &JSONAuditor{w: w, now: time.Now}
}

// AuditorFromFlag builds an Auditor from a --audit-log flag value. An empty
// value disables auditing (NopAuditor). "-" or "stderr" logs to os.Stderr.
// Any other value is a file path opened append-only; the returned closer is the
// open file (nil for stderr/off) and the caller closes it on shutdown.
func AuditorFromFlag(value string) (Auditor, io.Closer, error) {
	switch value {
	case "":
		return NopAuditor{}, nil, nil
	case "-", "stderr":
		return NewJSONAuditor(os.Stderr), nil, nil
	default:
		f, err := os.OpenFile(value, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, nil, fmt.Errorf("open audit log %s: %w", value, err)
		}
		return NewJSONAuditor(f), f, nil
	}
}

// Record stamps the event time (when unset) and writes one JSON line. Encoding
// errors are dropped: audit logging must never break the request path.
func (a *JSONAuditor) Record(ev AuditEvent) {
	if ev.Unix == 0 {
		ev.Unix = a.now().Unix()
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_, _ = a.w.Write(append(line, '\n'))
}
