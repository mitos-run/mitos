package mcp

import (
	"log"
	"strings"

	"mitos.run/mitos/internal/atr"
)

// maxLoggedDetections caps how many detection lines a single tool call may emit.
// Up to the full ruleset can trip on one payload, so an unbounded log would be a
// flooding and amplification vector once --enable-atr is on. Evaluate sorts
// critical-first, so the cap keeps the most severe matches; any overflow is
// collapsed into one summary line carrying the total.
const maxLoggedDetections = 20

// ATRConfig enables report-mode Agent Threat Rules screening of tool calls at
// the dispatch chokepoint. It is inert unless Evaluator is set (the
// --enable-atr flag, default off).
//
// Screening is DETECTION only: it logs matches and never changes the tool
// result, denies a call, or alters the isolation path. It flags patterns in the
// tool-call traffic mitos routes; it is not a control that blocks prompt
// injection. Deny mode is a deliberate follow-up (issue #474), not this slice.
type ATRConfig struct {
	// Evaluator holds the compiled ATR rules. When nil, screening is off.
	Evaluator *atr.Evaluator
	// ScanMaxBytes caps how many head bytes of each screened field are scanned;
	// 0 or less means no cap. A capped scan is logged as truncated so the limit
	// is observable rather than a silent skip.
	ScanMaxBytes int
	// Logger receives one line per detection on stderr. When nil, detections are
	// not logged (screening becomes a no-op).
	Logger *log.Logger
}

// enabled reports whether report-mode screening should run.
func (c *ATRConfig) enabled() bool {
	return c != nil && c.Evaluator != nil && c.Logger != nil
}

// screen builds an AgentEvent from a validated tool call, evaluates it against
// the ATR ruleset, and logs any detections. Only the two tools that carry
// agent-authored payloads through the chokepoint are screened: sandbox_exec
// (the command) and sandbox_write_file (the path and content). Other tools
// carry only ids and pool names and are not screened.
//
// Payload text is NEVER logged: a detection line names the rule, severity,
// category, and which event fields fired, not the content that matched.
func (c *ATRConfig) screen(tool string, a parsedArgs) {
	if !c.enabled() {
		return
	}
	fields, truncated, ok := c.eventFields(tool, a)
	if !ok {
		return
	}
	detections := c.Evaluator.Evaluate(atr.AgentEvent{
		Type:      "mcp_exchange",
		Fields:    fields,
		Truncated: truncated,
	})
	logged := detections
	if len(logged) > maxLoggedDetections {
		logged = logged[:maxLoggedDetections]
	}
	for _, d := range logged {
		c.Logger.Printf(
			"atr report-mode detection tool=%s rule=%s severity=%s category=%s scan_target=%s fields=%s truncated=%v",
			tool, d.RuleID, d.Severity, d.Category, d.ScanTarget, strings.Join(d.MatchedFields, ","), d.Truncated,
		)
	}
	if len(detections) > len(logged) {
		c.Logger.Printf(
			"atr report-mode detection tool=%s summary total=%d logged=%d suppressed=%d",
			tool, len(detections), len(logged), len(detections)-len(logged),
		)
	}
}

// eventFields maps a tool call's payload onto ATR field names. The command or
// file content is carried as content and tool_args, the fields the MCP-scan-path
// rules inspect. The bool is false for tools that carry no screenable payload.
//
// The mitos dispatch tool name (sandbox_exec, sandbox_write_file) is
// deliberately NOT screened as the ATR tool_name field: that field means the
// agent's target tool, and populating it with a name that always contains "exec"
// or "write" would fire every tool_name rule on every call. What matters here is
// the payload, not that the chokepoint is named exec.
func (c *ATRConfig) eventFields(tool string, a parsedArgs) (map[string]string, bool, bool) {
	switch tool {
	case ToolSandboxExec:
		cmd, trunc := atr.SampleForScan(a.command, c.ScanMaxBytes)
		return map[string]string{
			"content":   cmd,
			"tool_args": cmd,
		}, trunc, true
	case ToolSandboxWriteFile:
		content, trunc := atr.SampleForScan(a.content, c.ScanMaxBytes)
		// The path is small and unbounded scanning it is cheap; the content is
		// the payload the cap protects.
		return map[string]string{
			"content":   content,
			"tool_args": a.path + "\n" + content,
		}, trunc, true
	default:
		return nil, false, false
	}
}
